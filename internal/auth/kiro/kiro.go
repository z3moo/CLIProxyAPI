// Package kiro provides authentication and token management for Kiro
// (AWS CodeWhisperer) accounts. Two auth flows are supported:
//
//   - AWS SSO OIDC device authorization (Builder ID or IAM Identity Center)
//   - Kiro Social Auth (Google / GitHub) via the kiro.dev refresh endpoint
//
// Storage uses the same on-disk JSON layout as the other providers
// (codex, claude, kimi, gemini) so that operators can drop a `kiro-*.json`
// file into the auth dir and have it picked up by the file watcher.
package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	// AWSDefaultRegion is the AWS region used when none is recorded with the credential.
	AWSDefaultRegion = "us-east-1"
	// SocialTokenURL is the Kiro social auth refresh endpoint used when no AWS clientId/secret are present.
	SocialTokenURL = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	// AWSTokenURLFmt formats the AWS SSO OIDC token endpoint for a given region.
	AWSTokenURLFmt = "https://oidc.%s.amazonaws.com/token"

	// AuthMethodBuilderID identifies AWS SSO OIDC Builder ID auth.
	AuthMethodBuilderID = "builder-id"
	// AuthMethodIDC identifies AWS SSO OIDC IAM Identity Center auth.
	AuthMethodIDC = "idc"
	// AuthMethodSocial identifies Kiro Google/GitHub Social Auth.
	AuthMethodSocial = "social"

	// RefreshThresholdSeconds matches kimi/codex semantics: refresh when within 5 minutes of expiry.
	RefreshThresholdSeconds = 300
)

// KiroAuth coordinates Kiro token operations for the configured workspace.
type KiroAuth struct {
	cfg *config.Config
}

// NewKiroAuth constructs a Kiro auth helper bound to the running config.
func NewKiroAuth(cfg *config.Config) *KiroAuth { return &KiroAuth{cfg: cfg} }

// RefreshAccessToken exchanges the stored refresh token for a fresh access
// token. The auth method is auto-detected from `bundle`: if a clientId and
// clientSecret are present it goes through AWS SSO OIDC, otherwise it falls
// back to Kiro Social Auth.
func (a *KiroAuth) RefreshAccessToken(ctx context.Context, bundle *KiroAuthBundle, httpClient *http.Client) (*KiroTokenData, error) {
	if bundle == nil || strings.TrimSpace(bundle.RefreshToken) == "" {
		return nil, fmt.Errorf("kiro: missing refresh token")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	if strings.TrimSpace(bundle.ClientID) != "" && strings.TrimSpace(bundle.ClientSecret) != "" {
		region := strings.TrimSpace(bundle.Region)
		if region == "" || !strings.EqualFold(bundle.AuthMethod, AuthMethodIDC) {
			region = AWSDefaultRegion
		}
		return refreshAWSSSO(ctx, httpClient, region, bundle)
	}

	return refreshSocial(ctx, httpClient, bundle)
}

func refreshAWSSSO(ctx context.Context, client *http.Client, region string, bundle *KiroAuthBundle) (*KiroTokenData, error) {
	endpoint := fmt.Sprintf(AWSTokenURLFmt, region)
	payload, _ := json.Marshal(map[string]any{
		"clientId":     bundle.ClientID,
		"clientSecret": bundle.ClientSecret,
		"refreshToken": bundle.RefreshToken,
		"grantType":    "refresh_token",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kiro AWS refresh failed: %d: %s", resp.StatusCode, string(body))
	}

	td := &KiroTokenData{RefreshToken: bundle.RefreshToken, AuthMethod: bundle.AuthMethod}
	parseTokenJSON(body, td)
	if td.RefreshToken == "" {
		td.RefreshToken = bundle.RefreshToken
	}
	if td.AuthMethod == "" {
		td.AuthMethod = AuthMethodBuilderID
	}
	return td, nil
}

func refreshSocial(ctx context.Context, client *http.Client, bundle *KiroAuthBundle) (*KiroTokenData, error) {
	payload, _ := json.Marshal(map[string]any{"refreshToken": bundle.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, SocialTokenURL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kiro-cli/1.0.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kiro social refresh failed: %d: %s", resp.StatusCode, string(body))
	}

	td := &KiroTokenData{RefreshToken: bundle.RefreshToken, AuthMethod: AuthMethodSocial}
	parseTokenJSON(body, td)
	if td.RefreshToken == "" {
		td.RefreshToken = bundle.RefreshToken
	}
	return td, nil
}

func parseTokenJSON(body []byte, td *KiroTokenData) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return
	}
	if v, ok := parsed["accessToken"].(string); ok && v != "" {
		td.AccessToken = v
	} else if v, ok := parsed["access_token"].(string); ok {
		td.AccessToken = v
	}
	if v, ok := parsed["refreshToken"].(string); ok && v != "" {
		td.RefreshToken = v
	} else if v, ok := parsed["refresh_token"].(string); ok && v != "" {
		td.RefreshToken = v
	}
	expiresIn := int64(0)
	switch v := parsed["expiresIn"].(type) {
	case float64:
		expiresIn = int64(v)
	case string:
		if d, err := time.ParseDuration(v + "s"); err == nil {
			expiresIn = int64(d.Seconds())
		}
	}
	if expiresIn == 0 {
		switch v := parsed["expires_in"].(type) {
		case float64:
			expiresIn = int64(v)
		}
	}
	if expiresIn > 0 {
		td.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
	}
}

// CreateTokenStorage produces the persisted shape used by the FileTokenStore
// so a call to SaveTokenToFile lays a `kiro-*.json` file out the same way
// every other provider does.
func (a *KiroAuth) CreateTokenStorage(bundle *KiroAuthBundle, td *KiroTokenData) *KiroTokenStorage {
	storage := &KiroTokenStorage{
		Type:         "kiro",
		AuthMethod:   bundle.AuthMethod,
		AccessToken:  td.AccessToken,
		RefreshToken: td.RefreshToken,
		ClientID:     bundle.ClientID,
		ClientSecret: bundle.ClientSecret,
		Region:       bundle.Region,
		ProfileArn:   bundle.ProfileArn,
		Email:        bundle.Email,
	}
	if td.ExpiresAt > 0 {
		storage.Expired = time.Unix(td.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	if storage.AuthMethod == "" {
		if storage.ClientID != "" && storage.ClientSecret != "" {
			storage.AuthMethod = AuthMethodBuilderID
		} else {
			storage.AuthMethod = AuthMethodSocial
		}
	}
	return storage
}
