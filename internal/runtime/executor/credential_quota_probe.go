// Package executor: probe upstream OAuth quota endpoints to detect dead
// credentials. Used by the management surface to bulk-revoke auths whose
// upstream tokens have been invalidated and can no longer be refreshed.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
	geminiQuotaURL = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
)

// CredentialQuotaStatus describes the outcome of probing one auth's quota.
type CredentialQuotaStatus struct {
	Provider  string
	Status    int
	Message   string
	Revocable bool
	Reason    string
}

// ProbeCodexCredential calls https://chatgpt.com/backend-api/wham/usage with
// the auth's stored access token. A 401 with "authentication token is expired"
// flags the credential as Revocable so the caller can delete it.
func ProbeCodexCredential(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (CredentialQuotaStatus, error) {
	out := CredentialQuotaStatus{Provider: "codex"}
	if auth == nil {
		return out, errors.New("nil auth")
	}
	token := codexAccessTokenForProbe(auth)
	if token == "" {
		out.Message = "missing access token"
		out.Revocable = true
		out.Reason = "codex auth has no access_token"
		return out, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if id, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(id) != "" {
		req.Header.Set("ChatGPT-Account-Id", strings.TrimSpace(id))
	}
	resp, err := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0).Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	out.Status = resp.StatusCode
	out.Message = string(body)
	lower := strings.ToLower(string(body))
	if resp.StatusCode == http.StatusUnauthorized {
		switch {
		case strings.Contains(lower, "authentication token is expired"):
			out.Revocable = true
			out.Reason = "401 Provided authentication token is expired"
		case strings.Contains(lower, "authentication token has been invalidated"),
			strings.Contains(lower, "token has been invalidated"),
			strings.Contains(lower, "token is invalid"),
			strings.Contains(lower, "invalid_token"):
			out.Revocable = true
			out.Reason = "401 Your authentication token has been invalidated"
		}
	}
	return out, nil
}

// ProbeGeminiCredential calls
// https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota and flags
// auths whose response contains "Please check the credential status" as
// Revocable.
func ProbeGeminiCredential(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (CredentialQuotaStatus, error) {
	out := CredentialQuotaStatus{Provider: "gemini"}
	if auth == nil {
		return out, errors.New("nil auth")
	}
	token := geminiAccessTokenForProbe(auth)
	if token == "" {
		out.Message = "missing access token"
		out.Revocable = true
		out.Reason = "gemini auth has no access_token"
		return out, nil
	}
	body := map[string]string{}
	if projectID := geminiProjectIDForProbe(auth); projectID != "" {
		body["project"] = projectID
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiQuotaURL, bytes.NewReader(payload))
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0).Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out.Status = resp.StatusCode
	out.Message = string(raw)
	lower := strings.ToLower(string(raw))
	if strings.Contains(lower, "please check the credential status") {
		out.Revocable = true
		out.Reason = "Please check the credential status"
	} else if resp.StatusCode == http.StatusUnauthorized {
		out.Revocable = true
		out.Reason = fmt.Sprintf("gemini quota returned 401: %s", truncateProbeMessage(string(raw)))
	}
	return out, nil
}

func codexAccessTokenForProbe(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if v, ok := auth.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v := strings.TrimSpace(auth.Attributes["access_token"]); v != "" {
		return v
	}
	return ""
}

func geminiAccessTokenForProbe(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := strings.TrimSpace(auth.Attributes["access_token"]); v != "" {
		return v
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		if tok, ok := auth.Metadata["token"].(map[string]interface{}); ok {
			if v, ok2 := tok["access_token"].(string); ok2 && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func geminiProjectIDForProbe(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["project_id"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(auth.Attributes["project"]); v != "" {
			return v
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["project_id"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		if v, ok := auth.Metadata["project"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func truncateProbeMessage(s string) string {
	const max = 200
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
