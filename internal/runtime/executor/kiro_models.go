// Package executor: live Kiro model catalog.
//
// This file calls AWS CodeWhisperer's ListAvailableModels endpoint per Kiro
// account, parses the response, and expands each upstream model into the
// 9router-style "-thinking" / "-agentic" synthetic variants. Results are
// cached per-auth so /v1/models can answer cheaply between background ticks.
package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	kiroListModelsPathFmt     = "https://q.%s.amazonaws.com/ListAvailableModels"
	kiroModelsCacheTTL        = 5 * time.Minute
	kiroRuntimeSDKVersion     = "1.0.0"
	kiroAgentOS               = "windows"
	kiroAgentOSVersion        = "10.0.26200"
	kiroNodeVersion           = "22.21.1"
	kiroVersion               = "0.10.32"
	kiroCodeWhispererAPIModel = "codewhispererruntime"
)

// kiroModelEntry is one expanded model variant for /v1/models / dashboards.
// It embeds *registry.ModelInfo so it can be used directly with the global
// model registry, plus the upstream metadata that drove the expansion.
type kiroModelEntry struct {
	Info            *registry.ModelInfo
	UpstreamModelID string
	RateMultiplier  float64
	Description     string
	ContextWindow   int
	HasThinking     bool
	HasAgentic      bool
}

// kiroModelCacheRecord is what we cache per Kiro auth.
type kiroModelCacheRecord struct {
	expandedModels []*registry.ModelInfo
	rawEntries     []*kiroModelEntry
	fetchedAt      time.Time
}

var (
	kiroModelCacheMu sync.RWMutex
	kiroModelCache   = map[string]*kiroModelCacheRecord{} // keyed by auth ID
)

// FetchKiroModels resolves the live Kiro model catalog for one auth, expands
// each upstream model into the 9router-style synthetic variants, caches the
// result, and returns the expanded list ready to feed into the model registry.
//
// The caller is expected to hand in an Auth that already has a fresh
// access_token; ensureAccessToken is called as a courtesy when the token is
// stale, keeping the on-demand path consistent with chat requests.
func (e *KiroExecutor) FetchKiroModels(ctx context.Context, auth *cliproxyauth.Auth) ([]*registry.ModelInfo, []*kiroModelEntry, error) {
	if auth == nil {
		return nil, nil, fmt.Errorf("kiro models: auth is nil")
	}

	if cached := lookupKiroModelCache(auth.ID); cached != nil && time.Since(cached.fetchedAt) < kiroModelsCacheTTL {
		return cached.expandedModels, cached.rawEntries, nil
	}

	token, err := e.ensureAccessToken(ctx, auth)
	if err != nil {
		return nil, nil, err
	}

	region := kiroRegionForAuth(auth)
	endpoint := fmt.Sprintf(kiroListModelsPathFmt, region)
	profileArn := kiroExtractProfileArn(auth.Attributes, auth.Metadata)
	q := url.Values{}
	q.Set("origin", "AI_EDITOR")
	if profileArn != "" {
		q.Set("profileArn", profileArn)
	}
	endpoint = endpoint + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range buildKiroFingerprintHeaders(auth, token) {
		req.Header.Set(k, v)
	}

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, statusErr{code: resp.StatusCode, msg: string(body)}
	}

	expanded, raw, err := parseKiroListAvailableModels(body)
	if err != nil {
		return nil, nil, err
	}

	storeKiroModelCache(auth.ID, &kiroModelCacheRecord{
		expandedModels: expanded,
		rawEntries:     raw,
		fetchedAt:      time.Now(),
	})
	return expanded, raw, nil
}

// CachedKiroModels returns the most recent cached catalog for an auth, or nil.
func (e *KiroExecutor) CachedKiroModels(authID string) ([]*registry.ModelInfo, []*kiroModelEntry, time.Time) {
	rec := lookupKiroModelCache(authID)
	if rec == nil {
		return nil, nil, time.Time{}
	}
	return rec.expandedModels, rec.rawEntries, rec.fetchedAt
}

// InvalidateKiroModelCache drops the cached catalog for an auth so the next
// fetch is forced to refresh.
func (e *KiroExecutor) InvalidateKiroModelCache(authID string) {
	kiroModelCacheMu.Lock()
	delete(kiroModelCache, authID)
	kiroModelCacheMu.Unlock()
}

func lookupKiroModelCache(authID string) *kiroModelCacheRecord {
	kiroModelCacheMu.RLock()
	defer kiroModelCacheMu.RUnlock()
	return kiroModelCache[authID]
}

func storeKiroModelCache(authID string, rec *kiroModelCacheRecord) {
	if authID == "" || rec == nil {
		return
	}
	kiroModelCacheMu.Lock()
	kiroModelCache[authID] = rec
	kiroModelCacheMu.Unlock()
}

func buildKiroFingerprintHeaders(auth *cliproxyauth.Auth, accessToken string) map[string]string {
	seed := "kiro-anonymous"
	if auth != nil {
		if v := strings.TrimSpace(auth.Attributes["client_id"]); v != "" {
			seed = v
		} else if v := strings.TrimSpace(kiroExtractProfileArn(auth.Attributes, auth.Metadata)); v != "" {
			seed = v
		} else if v := strings.TrimSpace(auth.Attributes["refresh_token"]); v != "" {
			seed = v
		}
	}
	if seed == "kiro-anonymous" && strings.TrimSpace(accessToken) != "" {
		seed = strings.TrimSpace(accessToken)
	}
	sum := sha256.Sum256([]byte(seed))
	machineID := hex.EncodeToString(sum[:])
	userAgent := fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s#%s lang/js md/nodejs#%s api/%s#%s m/N,E KiroIDE-%s-%s",
		kiroRuntimeSDKVersion,
		kiroAgentOS,
		kiroAgentOSVersion,
		kiroNodeVersion,
		kiroCodeWhispererAPIModel,
		kiroRuntimeSDKVersion,
		kiroVersion,
		machineID,
	)
	amzUserAgent := fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s-%s", kiroRuntimeSDKVersion, kiroVersion, machineID)
	return map[string]string{
		"User-Agent":                  userAgent,
		"X-Amz-User-Agent":            amzUserAgent,
		"X-Amzn-Kiro-Agent-Mode":      "vibe",
		"X-Amzn-Codewhisperer-Optout": "true",
		"Amz-Sdk-Request":             "attempt=1; max=1",
		"Amz-Sdk-Invocation-Id":       uuid.NewString(),
		"Accept":                      "application/json",
	}
}

// parseKiroListAvailableModels parses the JSON returned by ListAvailableModels
// and expands each upstream model into "{id}", "{id}-thinking", and (when not
// "auto") "{id}-agentic" + "{id}-thinking-agentic" variants.
func parseKiroListAvailableModels(body []byte) ([]*registry.ModelInfo, []*kiroModelEntry, error) {
	var parsed struct {
		Models []struct {
			ModelID        string  `json:"modelId"`
			ID             string  `json:"id"`
			ModelName      string  `json:"modelName"`
			Description    string  `json:"description"`
			RateMultiplier float64 `json:"rateMultiplier"`
			TokenLimits    struct {
				MaxInputTokens int `json:"maxInputTokens"`
			} `json:"tokenLimits"`
		} `json:"models"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &parsed); err != nil {
		return nil, nil, fmt.Errorf("kiro models: parse response: %w", err)
	}

	expanded := []*registry.ModelInfo{}
	raw := []*kiroModelEntry{}
	now := time.Now().Unix()

	for _, m := range parsed.Models {
		id := strings.TrimSpace(m.ModelID)
		if id == "" {
			id = strings.TrimSpace(m.ID)
		}
		if id == "" {
			continue
		}
		// Strip any synthetic suffix that snuck in upstream (defensive).
		if strings.HasSuffix(id, kiroAgenticSuffix) {
			id = strings.TrimSuffix(id, kiroAgenticSuffix)
		}
		if strings.HasSuffix(id, kiroThinkingSuffix) {
			id = strings.TrimSuffix(id, kiroThinkingSuffix)
		}
		display := strings.TrimSpace(m.ModelName)
		if display == "" {
			display = id
		}
		display = "Kiro " + display
		if m.RateMultiplier > 0 && m.RateMultiplier != 1.0 {
			display = fmt.Sprintf("%s (%.1fx credit)", display, m.RateMultiplier)
		}
		ctx := m.TokenLimits.MaxInputTokens
		if ctx <= 0 {
			ctx = 200000
		}
		isAuto := strings.EqualFold(id, "auto")

		variants := []struct {
			suffix   string
			label    string
			thinking bool
			agentic  bool
		}{
			{"", "", false, false},
			{kiroThinkingSuffix, " (Thinking)", true, false},
		}
		if !isAuto {
			variants = append(variants,
				struct {
					suffix   string
					label    string
					thinking bool
					agentic  bool
				}{kiroAgenticSuffix, " (Agentic)", false, true},
				struct {
					suffix   string
					label    string
					thinking bool
					agentic  bool
				}{kiroThinkingSuffix + kiroAgenticSuffix, " (Thinking + Agentic)", true, true},
			)
		}

		for _, v := range variants {
			modelID := "kr/" + id + v.suffix
			info := &registry.ModelInfo{
				ID:                  modelID,
				Object:              "model",
				Created:             now,
				OwnedBy:             "kiro",
				Type:                "kiro",
				DisplayName:         display + v.label,
				Name:                modelID,
				Description:         strings.TrimSpace(m.Description),
				ContextLength:       ctx,
				MaxCompletionTokens: 32000,
			}
			expanded = append(expanded, info)
			raw = append(raw, &kiroModelEntry{
				Info:            info,
				UpstreamModelID: id,
				RateMultiplier:  m.RateMultiplier,
				Description:     strings.TrimSpace(m.Description),
				ContextWindow:   ctx,
				HasThinking:     v.thinking,
				HasAgentic:      v.agentic,
			})
		}
	}
	return expanded, raw, nil
}
