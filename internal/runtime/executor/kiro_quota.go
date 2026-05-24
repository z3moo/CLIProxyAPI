// Package executor: live Kiro quota tracking.
//
// This file calls the AWS CodeWhisperer / Kiro getUsageLimits endpoints with
// the same fall-through pattern as 9router (codewhisperer-get,
// codewhisperer-post, q-get) and parses `usageBreakdownList` into a normalized
// `KiroQuotaSnapshot`. Snapshots are cached per-auth so dashboards can read
// state cheaply between background ticks.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	kiroDefaultProfileArn = "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"
	kiroQuotaCacheTTL     = 5 * time.Minute
	kiroQuotaResource     = "AGENTIC_REQUEST"
)

// KiroQuotaEntry is one normalized usage breakdown row.
type KiroQuotaEntry struct {
	Name      string    `json:"name"`
	Used      float64   `json:"used"`
	Total     float64   `json:"total"`
	Remaining float64   `json:"remaining"`
	ResetAt   time.Time `json:"reset_at,omitempty"`
	Unlimited bool      `json:"unlimited"`
	FreeTrial bool      `json:"free_trial"`
}

// KiroQuotaSnapshot is the surfaced quota state for one Kiro auth.
type KiroQuotaSnapshot struct {
	AuthID    string           `json:"auth_id"`
	Plan      string           `json:"plan,omitempty"`
	Quotas    []KiroQuotaEntry `json:"quotas"`
	Message   string           `json:"message,omitempty"`
	FetchedAt time.Time        `json:"fetched_at"`
}

func (s *KiroQuotaSnapshot) QuotaMap() map[string]KiroQuotaEntry {
	out := map[string]KiroQuotaEntry{}
	if s == nil {
		return out
	}
	for _, q := range s.Quotas {
		name := strings.TrimSpace(q.Name)
		if name == "" {
			name = "unknown"
		}
		out[name] = q
	}
	return out
}

var (
	kiroQuotaCacheMu sync.RWMutex
	kiroQuotaCache   = map[string]*KiroQuotaSnapshot{}
)

// FetchKiroQuota refreshes the live quota snapshot for one auth, caches it,
// and returns it. The fall-through order mirrors 9router's getKiroUsage.
func (e *KiroExecutor) FetchKiroQuota(ctx context.Context, auth *cliproxyauth.Auth) (*KiroQuotaSnapshot, error) {
	if auth == nil {
		return nil, fmt.Errorf("kiro quota: auth is nil")
	}

	if cached := lookupKiroQuotaCache(auth.ID); cached != nil && time.Since(cached.FetchedAt) < kiroQuotaCacheTTL {
		return cached, nil
	}

	token, err := e.ensureAccessToken(ctx, auth)
	if err != nil {
		return nil, err
	}

	profileArn := kiroExtractProfileArn(auth.Attributes, auth.Metadata)
	if profileArn == "" {
		profileArn = kiroDefaultProfileArn
	}
	authMethod := strings.ToLower(kiroMetaString(auth.Metadata, "auth_method", "authMethod"))
	if authMethod == "" {
		authMethod = "builder-id"
	}

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	type attempt struct {
		name string
		req  func() (*http.Request, error)
	}
	attempts := []attempt{
		{
			name: "codewhisperer-get",
			req: func() (*http.Request, error) {
				params := url.Values{}
				params.Set("isEmailRequired", "true")
				params.Set("origin", "AI_EDITOR")
				params.Set("resourceType", kiroQuotaResource)
				u := "https://codewhisperer.us-east-1.amazonaws.com/getUsageLimits?" + params.Encode()
				r, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
				if err != nil {
					return nil, err
				}
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Accept", "application/json")
				r.Header.Set("X-Amz-User-Agent", "aws-sdk-js/1.0.0 KiroIDE")
				r.Header.Set("User-Agent", "aws-sdk-js/1.0.0 KiroIDE")
				return r, nil
			},
		},
		{
			name: "codewhisperer-post",
			req: func() (*http.Request, error) {
				body, _ := json.Marshal(map[string]any{
					"origin":       "AI_EDITOR",
					"profileArn":   profileArn,
					"resourceType": kiroQuotaResource,
				})
				r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://codewhisperer.us-east-1.amazonaws.com", bytes.NewReader(body))
				if err != nil {
					return nil, err
				}
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Content-Type", "application/x-amz-json-1.0")
				r.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.GetUsageLimits")
				r.Header.Set("Accept", "application/json")
				return r, nil
			},
		},
		{
			name: "q-get",
			req: func() (*http.Request, error) {
				params := url.Values{}
				params.Set("origin", "AI_EDITOR")
				params.Set("profileArn", profileArn)
				params.Set("resourceType", kiroQuotaResource)
				u := "https://q.us-east-1.amazonaws.com/getUsageLimits?" + params.Encode()
				r, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
				if err != nil {
					return nil, err
				}
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Accept", "application/json")
				return r, nil
			},
		},
	}

	var (
		sawAuthError bool
		errs         []string
	)
	for _, a := range attempts {
		req, err := a.req()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s:%v", a.name, err))
			continue
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s:%v", a.name, err))
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			sawAuthError = true
			errs = append(errs, fmt.Sprintf("%s:%d:%s", a.name, resp.StatusCode, truncateForError(string(body))))
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errs = append(errs, fmt.Sprintf("%s:%d:%s", a.name, resp.StatusCode, truncateForError(string(body))))
			continue
		}
		snap, parseErr := parseKiroQuotaResponse(body)
		if parseErr != nil {
			errs = append(errs, fmt.Sprintf("%s:parse:%v", a.name, parseErr))
			continue
		}
		snap.AuthID = auth.ID
		snap.FetchedAt = time.Now()
		storeKiroQuotaCache(auth.ID, snap)
		return snap, nil
	}

	snap := &KiroQuotaSnapshot{AuthID: auth.ID, FetchedAt: time.Now()}
	switch {
	case sawAuthError && authMethod == "idc":
		snap.Message = "Kiro quota API is unavailable for the current AWS IAM Identity Center session. Chat may still work."
	case sawAuthError:
		snap.Message = "Kiro quota API rejected the current token. Chat may still work."
	case len(errs) > 0:
		snap.Message = "Unable to fetch Kiro usage right now: " + errs[len(errs)-1]
	default:
		snap.Message = "Unable to fetch Kiro usage right now."
	}
	storeKiroQuotaCache(auth.ID, snap)
	return snap, nil
}

// CachedKiroQuota returns the most recent cached snapshot, or nil.
func (e *KiroExecutor) CachedKiroQuota(authID string) *KiroQuotaSnapshot {
	return lookupKiroQuotaCache(authID)
}

// InvalidateKiroQuotaCache drops the cached quota for an auth.
func (e *KiroExecutor) InvalidateKiroQuotaCache(authID string) {
	kiroQuotaCacheMu.Lock()
	delete(kiroQuotaCache, authID)
	kiroQuotaCacheMu.Unlock()
}

func lookupKiroQuotaCache(authID string) *KiroQuotaSnapshot {
	kiroQuotaCacheMu.RLock()
	defer kiroQuotaCacheMu.RUnlock()
	return kiroQuotaCache[authID]
}

func storeKiroQuotaCache(authID string, snap *KiroQuotaSnapshot) {
	if authID == "" || snap == nil {
		return
	}
	kiroQuotaCacheMu.Lock()
	kiroQuotaCache[authID] = snap
	kiroQuotaCacheMu.Unlock()
}

// parseKiroQuotaResponse decodes the AWS getUsageLimits payload and produces
// the normalized snapshot. Both `usageBreakdownList` (Kiro) and bare numeric
// fields are accepted defensively.
func parseKiroQuotaResponse(body []byte) (*KiroQuotaSnapshot, error) {
	var parsed struct {
		SubscriptionInfo struct {
			SubscriptionTitle string `json:"subscriptionTitle"`
		} `json:"subscriptionInfo"`
		NextDateReset json.RawMessage `json:"nextDateReset"`
		ResetDate     json.RawMessage `json:"resetDate"`
		Usage         []struct {
			ResourceType              string  `json:"resourceType"`
			CurrentUsageWithPrecision float64 `json:"currentUsageWithPrecision"`
			UsageLimitWithPrecision   float64 `json:"usageLimitWithPrecision"`
			FreeTrialInfo             *struct {
				CurrentUsageWithPrecision float64         `json:"currentUsageWithPrecision"`
				UsageLimitWithPrecision   float64         `json:"usageLimitWithPrecision"`
				FreeTrialExpiry           json.RawMessage `json:"freeTrialExpiry"`
			} `json:"freeTrialInfo"`
		} `json:"usageBreakdownList"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &parsed); err != nil {
		return nil, err
	}

	resetAt := parseKiroResetTime(parsed.NextDateReset)
	if resetAt.IsZero() {
		resetAt = parseKiroResetTime(parsed.ResetDate)
	}

	snap := &KiroQuotaSnapshot{
		Plan: strings.TrimSpace(parsed.SubscriptionInfo.SubscriptionTitle),
	}
	if snap.Plan == "" {
		snap.Plan = "Kiro"
	}
	for _, u := range parsed.Usage {
		name := strings.ToLower(strings.TrimSpace(u.ResourceType))
		if name == "" {
			name = "unknown"
		}
		entry := KiroQuotaEntry{
			Name:      name,
			Used:      u.CurrentUsageWithPrecision,
			Total:     u.UsageLimitWithPrecision,
			Remaining: u.UsageLimitWithPrecision - u.CurrentUsageWithPrecision,
			ResetAt:   resetAt,
			Unlimited: u.UsageLimitWithPrecision == 0,
		}
		snap.Quotas = append(snap.Quotas, entry)

		if u.FreeTrialInfo != nil && u.FreeTrialInfo.UsageLimitWithPrecision > 0 {
			fr := *u.FreeTrialInfo
			ft := KiroQuotaEntry{
				Name:      name + "_freetrial",
				Used:      fr.CurrentUsageWithPrecision,
				Total:     fr.UsageLimitWithPrecision,
				Remaining: fr.UsageLimitWithPrecision - fr.CurrentUsageWithPrecision,
				ResetAt:   parseKiroResetTime(fr.FreeTrialExpiry),
				Unlimited: false,
				FreeTrial: true,
			}
			if ft.ResetAt.IsZero() {
				ft.ResetAt = resetAt
			}
			snap.Quotas = append(snap.Quotas, ft)
		}
	}
	return snap, nil
}

func parseKiroResetTime(raw json.RawMessage) time.Time {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return time.Time{}
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return time.Time{}
		}
		if t, ok := parseKiroResetString(text); ok {
			return t
		}
		return time.Time{}
	}

	var n float64
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		sec := int64(n)
		if sec > 1_000_000_000_000 {
			return time.UnixMilli(sec).UTC()
		}
		return time.Unix(sec, 0).UTC()
	}

	return time.Time{}
}

func parseKiroResetString(raw string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05Z", raw); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func truncateForError(s string) string {
	const max = 200
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
