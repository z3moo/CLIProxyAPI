// Package usage records per-request token totals and computes estimated costs.
//
// The aggregator registers as a usage.Plugin on the global usage manager and
// keeps in-memory counters keyed by (provider, model, apiKey, authID). It
// persists a JSON snapshot to <authDir>/usage-stats.json on a debounced timer
// so stats survive restarts.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	maxDailyRollupDays = 90
	recentRequestLimit = 50
	persistDebounce    = 5 * time.Second
	maxLifetimeKeys    = 4096 // safety cap on cardinality
)

// Counter aggregates token totals and request counts for a single dimension.
type Counter struct {
	Requests            int64     `json:"requests"`
	PromptTokens        int64     `json:"prompt_tokens"`
	CompletionTokens    int64     `json:"completion_tokens"`
	ReasoningTokens     int64     `json:"reasoning_tokens"`
	CachedTokens        int64     `json:"cached_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	TotalTokens         int64     `json:"total_tokens"`
	Cost                float64   `json:"cost"`
	LastUsed            time.Time `json:"last_used"`
}

// counterKey identifies a counter row.
type counterKey struct {
	Provider string
	Model    string
	APIKey   string
	AuthID   string
}

// dimensionRecord wraps a Counter with its identity fields for JSON output.
type DimensionRecord struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	APIKey           string  `json:"api_key,omitempty"`
	APIKeyName       string  `json:"key_name,omitempty"`
	AuthID           string  `json:"auth_id,omitempty"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	Cost             float64 `json:"cost"`
	LastUsed         string  `json:"lastUsed,omitempty"`
}

// recentRequest mirrors 9router's recent-request panel row.
type RecentRequest struct {
	Timestamp        time.Time `json:"timestamp"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	PromptTokens     int64     `json:"promptTokens"`
	CompletionTokens int64     `json:"completionTokens"`
	Status           string    `json:"status"`
	APIKey           string    `json:"api_key,omitempty"`
	AuthID           string    `json:"auth_id,omitempty"`
	Cost             float64   `json:"cost"`
}

// Snapshot is the JSON payload for /v0/management/usage-stats.
type Snapshot struct {
	GeneratedAt           time.Time         `json:"generated_at"`
	Period                string            `json:"period"`
	TotalRequests         int64             `json:"totalRequests"`
	TotalPromptTokens     int64             `json:"totalPromptTokens"`
	TotalCompletionTokens int64             `json:"totalCompletionTokens"`
	TotalTokens           int64             `json:"totalTokens"`
	TotalCost             float64           `json:"totalCost"`
	ByProvider            []DimensionRecord `json:"byProvider"`
	ByModel               []DimensionRecord `json:"byModel"`
	ByAPIKey              []DimensionRecord `json:"byApiKey"`
	ByAuth                []DimensionRecord `json:"byAuth"`
	RecentRequests        []RecentRequest   `json:"recentRequests"`
}

// persistedAggregator is the on-disk JSON layout of the aggregator.
type persistedAggregator struct {
	Version    int                            `json:"version"`
	UpdatedAt  time.Time                      `json:"updated_at"`
	Lifetime   map[string]*Counter            `json:"lifetime"` // canonicalKey -> counter
	Daily      map[string]map[string]*Counter `json:"daily"`    // dateKey (YYYY-MM-DD UTC) -> canonicalKey -> counter
	KeyMeta    map[string]counterKey          `json:"key_meta"` // canonicalKey -> identifying fields
	APIKeyName map[string]string              `json:"api_key_name,omitempty"`
	Recent     []RecentRequest                `json:"recent_requests,omitempty"`
}

// Aggregator captures usage records and serves snapshots.
type Aggregator struct {
	mu       sync.Mutex
	lifetime map[string]*Counter
	daily    map[string]map[string]*Counter
	meta     map[string]counterKey
	keyNames map[string]string // apiKey -> keyName
	recent   []RecentRequest

	persistPath  string
	persistTimer *time.Timer
	dirty        bool
	stopped      bool
}

var (
	defaultAggregator *Aggregator
	defaultOnce       sync.Once
)

// Default returns the process-wide aggregator instance, creating it on first use.
func Default() *Aggregator {
	defaultOnce.Do(func() {
		defaultAggregator = newAggregator()
		coreusage.RegisterPlugin(defaultAggregator)
	})
	return defaultAggregator
}

func newAggregator() *Aggregator {
	return &Aggregator{
		lifetime: make(map[string]*Counter),
		daily:    make(map[string]map[string]*Counter),
		meta:     make(map[string]counterKey),
		keyNames: make(map[string]string),
	}
}

// Configure sets the on-disk persistence path and loads any existing snapshot.
// Call once at startup after AuthDir is resolved. Safe to call multiple times.
func (a *Aggregator) Configure(authDir string) {
	if a == nil {
		return
	}
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		log.Warnf("usage aggregator: mkdir auth dir %s: %v", authDir, err)
		return
	}
	path := filepath.Join(authDir, "usage-stats.json")

	a.mu.Lock()
	defer a.mu.Unlock()
	a.persistPath = path
	a.loadLocked()
}

// SetAPIKeyName records a friendly name for an API key. Optional; when set the
// /usage-stats response substitutes the name in the byApiKey rows.
func (a *Aggregator) SetAPIKeyName(apiKey, name string) {
	if a == nil {
		return
	}
	apiKey = strings.TrimSpace(apiKey)
	name = strings.TrimSpace(name)
	if apiKey == "" || name == "" {
		return
	}
	a.mu.Lock()
	a.keyNames[apiKey] = name
	a.dirty = true
	a.scheduleFlushLocked()
	a.mu.Unlock()
}

// HandleUsage implements coreusage.Plugin. Successful records are credited;
// failed records still bump the request counter so dashboards reflect traffic.
func (a *Aggregator) HandleUsage(ctx context.Context, record coreusage.Record) {
	if a == nil {
		return
	}
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	provider := strings.TrimSpace(record.Provider)
	if provider == "" {
		provider = "unknown"
	}
	provider = collapseProvider(provider)

	model := strings.TrimSpace(record.Alias)
	if model == "" {
		model = strings.TrimSpace(record.Model)
	}
	if model == "" {
		model = "unknown"
	}
	apiKey := strings.TrimSpace(record.APIKey)
	authID := strings.TrimSpace(record.AuthID)

	in := record.Detail.InputTokens
	out := record.Detail.OutputTokens
	reasoning := record.Detail.ReasoningTokens
	cached := record.Detail.CachedTokens
	if cached == 0 {
		cached = record.Detail.CacheReadTokens
	}
	cacheCreation := record.Detail.CacheCreationTokens
	total := record.Detail.TotalTokens
	if total == 0 {
		total = in + out + reasoning
	}

	pricing, _ := LookupPricing(provider, model)
	cost := CalculateCost(pricing, in, out, reasoning, cached, cacheCreation)

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return
	}

	key := counterKey{Provider: provider, Model: model, APIKey: apiKey, AuthID: authID}
	canonical := canonicalKey(key)
	if _, ok := a.meta[canonical]; !ok {
		if len(a.meta) >= maxLifetimeKeys {
			// silently drop new tuples once cap is reached; existing keys still update
			return
		}
		a.meta[canonical] = key
	}

	apply := func(c *Counter) {
		c.Requests++
		c.PromptTokens += in
		c.CompletionTokens += out
		c.ReasoningTokens += reasoning
		c.CachedTokens += cached
		c.CacheCreationTokens += cacheCreation
		c.TotalTokens += total
		c.Cost += cost
		if timestamp.After(c.LastUsed) {
			c.LastUsed = timestamp
		}
	}

	lifetime := a.lifetime[canonical]
	if lifetime == nil {
		lifetime = &Counter{}
		a.lifetime[canonical] = lifetime
	}
	apply(lifetime)

	day := dayKey(timestamp)
	bucket := a.daily[day]
	if bucket == nil {
		bucket = make(map[string]*Counter)
		a.daily[day] = bucket
	}
	rollup := bucket[canonical]
	if rollup == nil {
		rollup = &Counter{}
		bucket[canonical] = rollup
	}
	apply(rollup)

	a.pruneDailyLocked()

	status := "ok"
	if record.Failed {
		status = "error"
	}
	a.recent = append(a.recent, RecentRequest{
		Timestamp:        timestamp,
		Provider:         provider,
		Model:            model,
		PromptTokens:     in,
		CompletionTokens: out,
		Status:           status,
		APIKey:           apiKey,
		AuthID:           authID,
		Cost:             cost,
	})
	if len(a.recent) > recentRequestLimit {
		a.recent = a.recent[len(a.recent)-recentRequestLimit:]
	}

	a.dirty = true
	a.scheduleFlushLocked()
}

// pruneDailyLocked drops the oldest day rollups to keep memory bounded.
func (a *Aggregator) pruneDailyLocked() {
	if len(a.daily) <= maxDailyRollupDays {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -maxDailyRollupDays)
	cutoffKey := cutoff.Format("2006-01-02")
	for k := range a.daily {
		if k < cutoffKey {
			delete(a.daily, k)
		}
	}
}

// Snapshot returns aggregated stats for the requested period.
//
// Supported periods: today, 24h, 7d, 30d, 60d, all (default 7d).
func (a *Aggregator) Snapshot(period string) Snapshot {
	if a == nil {
		return Snapshot{}
	}
	period = strings.TrimSpace(period)
	if period == "" {
		period = "7d"
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	totals := map[string]*Counter{}
	if period == "all" {
		for k, v := range a.lifetime {
			totals[k] = cloneCounter(v)
		}
	} else {
		for _, day := range daysInPeriod(period) {
			bucket := a.daily[day]
			for k, v := range bucket {
				existing := totals[k]
				if existing == nil {
					existing = &Counter{}
					totals[k] = existing
				}
				mergeCounter(existing, v)
			}
		}
	}

	snap := Snapshot{
		GeneratedAt: time.Now().UTC(),
		Period:      period,
	}

	byProvider := map[string]*Counter{}
	byModel := map[string]*Counter{}
	byAPIKey := map[string]*Counter{}
	byAuth := map[string]*Counter{}
	providerMeta := map[string]counterKey{}
	modelMeta := map[string]counterKey{}
	apiKeyMeta := map[string]counterKey{}
	authMeta := map[string]counterKey{}

	for canonical, counter := range totals {
		key := a.meta[canonical]

		snap.TotalRequests += counter.Requests
		snap.TotalPromptTokens += counter.PromptTokens
		snap.TotalCompletionTokens += counter.CompletionTokens
		snap.TotalTokens += counter.TotalTokens
		snap.TotalCost += counter.Cost

		mergeInto(byProvider, providerMeta, key.Provider, counterKey{Provider: key.Provider}, counter)
		modelGroupKey := key.Provider + "|" + key.Model
		mergeInto(byModel, modelMeta, modelGroupKey, counterKey{Provider: key.Provider, Model: key.Model}, counter)
		if key.APIKey != "" {
			apiKeyGroupKey := key.APIKey + "|" + key.Provider + "|" + key.Model
			mergeInto(byAPIKey, apiKeyMeta, apiKeyGroupKey, counterKey{Provider: key.Provider, Model: key.Model, APIKey: key.APIKey}, counter)
		}
		if key.AuthID != "" {
			authGroupKey := key.AuthID + "|" + key.Provider + "|" + key.Model
			mergeInto(byAuth, authMeta, authGroupKey, counterKey{Provider: key.Provider, Model: key.Model, AuthID: key.AuthID}, counter)
		}
	}

	snap.ByProvider = renderDimensions(byProvider, providerMeta, a.keyNames)
	snap.ByModel = renderDimensions(byModel, modelMeta, a.keyNames)
	snap.ByAPIKey = renderDimensions(byAPIKey, apiKeyMeta, a.keyNames)
	snap.ByAuth = renderDimensions(byAuth, authMeta, a.keyNames)

	cutoff := periodCutoff(period)
	for i := len(a.recent) - 1; i >= 0; i-- {
		entry := a.recent[i]
		if !cutoff.IsZero() && entry.Timestamp.Before(cutoff) {
			break
		}
		snap.RecentRequests = append(snap.RecentRequests, entry)
		if len(snap.RecentRequests) >= recentRequestLimit {
			break
		}
	}

	return snap
}

// Reset clears all aggregated state. Used when an admin wants a fresh slate.
func (a *Aggregator) Reset() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.lifetime = make(map[string]*Counter)
	a.daily = make(map[string]map[string]*Counter)
	a.meta = make(map[string]counterKey)
	a.recent = nil
	a.dirty = true
	a.scheduleFlushLocked()
	a.mu.Unlock()
}

// Stop flushes pending state and prevents further updates.
func (a *Aggregator) Stop() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.persistTimer != nil {
		a.persistTimer.Stop()
		a.persistTimer = nil
	}
	a.stopped = true
	a.mu.Unlock()
	a.flushNow()
}

// canonicalKey collapses a counterKey into a string suitable for map keys
// and JSON map keys.
func canonicalKey(k counterKey) string {
	return strings.Join([]string{k.Provider, k.Model, k.APIKey, k.AuthID}, "\x1f")
}

func decanonicalKey(s string) counterKey {
	parts := strings.Split(s, "\x1f")
	out := counterKey{}
	if len(parts) >= 1 {
		out.Provider = parts[0]
	}
	if len(parts) >= 2 {
		out.Model = parts[1]
	}
	if len(parts) >= 3 {
		out.APIKey = parts[2]
	}
	if len(parts) >= 4 {
		out.AuthID = parts[3]
	}
	return out
}

func cloneCounter(c *Counter) *Counter {
	if c == nil {
		return &Counter{}
	}
	out := *c
	return &out
}

func mergeCounter(dst, src *Counter) {
	dst.Requests += src.Requests
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.ReasoningTokens += src.ReasoningTokens
	dst.CachedTokens += src.CachedTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.TotalTokens += src.TotalTokens
	dst.Cost += src.Cost
	if src.LastUsed.After(dst.LastUsed) {
		dst.LastUsed = src.LastUsed
	}
}

func mergeInto(target map[string]*Counter, meta map[string]counterKey, groupKey string, identity counterKey, counter *Counter) {
	if counter == nil {
		return
	}
	existing := target[groupKey]
	if existing == nil {
		existing = &Counter{}
		target[groupKey] = existing
		meta[groupKey] = identity
	}
	mergeCounter(existing, counter)
}

func renderDimensions(rows map[string]*Counter, meta map[string]counterKey, keyNames map[string]string) []DimensionRecord {
	out := make([]DimensionRecord, 0, len(rows))
	for groupKey, counter := range rows {
		identity := meta[groupKey]
		row := DimensionRecord{
			Provider:         identity.Provider,
			Model:            identity.Model,
			APIKey:           identity.APIKey,
			AuthID:           identity.AuthID,
			Requests:         counter.Requests,
			PromptTokens:     counter.PromptTokens,
			CompletionTokens: counter.CompletionTokens,
			TotalTokens:      counter.TotalTokens,
			Cost:             counter.Cost,
		}
		if !counter.LastUsed.IsZero() {
			row.LastUsed = counter.LastUsed.UTC().Format(time.RFC3339)
		}
		if identity.APIKey != "" {
			if name, ok := keyNames[identity.APIKey]; ok {
				row.APIKeyName = name
			}
		}
		out = append(out, row)
	}
	return out
}

func collapseProvider(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch p {
	case "kiro", "kiro-aws", "kiro-social":
		return "kiro"
	}
	return p
}

func dayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func periodCutoff(period string) time.Time {
	now := time.Now().UTC()
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	case "24h":
		return now.Add(-24 * time.Hour)
	case "7d":
		return now.AddDate(0, 0, -7)
	case "30d":
		return now.AddDate(0, 0, -30)
	case "60d":
		return now.AddDate(0, 0, -60)
	case "all", "":
		return time.Time{}
	default:
		return now.AddDate(0, 0, -7)
	}
}

func daysInPeriod(period string) []string {
	now := time.Now().UTC()
	var days int
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "today":
		return []string{dayKey(now)}
	case "24h":
		return []string{dayKey(now), dayKey(now.AddDate(0, 0, -1))}
	case "7d":
		days = 7
	case "30d":
		days = 30
	case "60d":
		days = 60
	default:
		days = 7
	}
	out := make([]string, 0, days+1)
	for i := 0; i <= days; i++ {
		out = append(out, dayKey(now.AddDate(0, 0, -i)))
	}
	return out
}

func (a *Aggregator) scheduleFlushLocked() {
	if a.persistPath == "" {
		return
	}
	if a.persistTimer != nil {
		return
	}
	a.persistTimer = time.AfterFunc(persistDebounce, func() {
		a.mu.Lock()
		a.persistTimer = nil
		a.mu.Unlock()
		a.flushNow()
	})
}

func (a *Aggregator) flushNow() {
	a.mu.Lock()
	if !a.dirty || a.persistPath == "" {
		a.mu.Unlock()
		return
	}
	payload := persistedAggregator{
		Version:    1,
		UpdatedAt:  time.Now().UTC(),
		Lifetime:   cloneCounterMap(a.lifetime),
		Daily:      cloneDailyMap(a.daily),
		KeyMeta:    cloneMetaMap(a.meta),
		APIKeyName: cloneStringMap(a.keyNames),
		Recent:     append([]RecentRequest(nil), a.recent...),
	}
	path := a.persistPath
	a.dirty = false
	a.mu.Unlock()

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Warnf("usage aggregator: marshal: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Warnf("usage aggregator: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Warnf("usage aggregator: rename %s: %v", path, err)
	}
}

func (a *Aggregator) loadLocked() {
	if a.persistPath == "" {
		return
	}
	data, err := os.ReadFile(a.persistPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("usage aggregator: read %s: %v", a.persistPath, err)
		}
		return
	}
	var stored persistedAggregator
	if err := json.Unmarshal(data, &stored); err != nil {
		log.Warnf("usage aggregator: parse %s: %v", a.persistPath, err)
		return
	}
	if stored.Lifetime != nil {
		a.lifetime = stored.Lifetime
	}
	if stored.Daily != nil {
		a.daily = stored.Daily
	}
	if stored.KeyMeta != nil {
		a.meta = stored.KeyMeta
	} else {
		// rebuild meta from canonical keys when stored on an older version
		for canonical := range a.lifetime {
			a.meta[canonical] = decanonicalKey(canonical)
		}
	}
	if stored.APIKeyName != nil {
		a.keyNames = stored.APIKeyName
	}
	if stored.Recent != nil {
		a.recent = stored.Recent
	}
	a.pruneDailyLocked()
}

func cloneCounterMap(in map[string]*Counter) map[string]*Counter {
	out := make(map[string]*Counter, len(in))
	for k, v := range in {
		out[k] = cloneCounter(v)
	}
	return out
}

func cloneDailyMap(in map[string]map[string]*Counter) map[string]map[string]*Counter {
	out := make(map[string]map[string]*Counter, len(in))
	for day, bucket := range in {
		out[day] = cloneCounterMap(bucket)
	}
	return out
}

func cloneMetaMap(in map[string]counterKey) map[string]counterKey {
	out := make(map[string]counterKey, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// MarshalJSON for counterKey is needed because JSON object keys cannot be
// structs; persistedAggregator stores meta keyed by canonical string.
var _ = json.Marshal
var _ = fmt.Sprintf
