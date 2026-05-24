package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// kiroRefreshLead governs how early the auto-refresh loop kicks in for Kiro
// access tokens; aligned with kimi for consistency.
var kiroRefreshLead = 5 * time.Minute

// KiroAuthenticator implements credential import for Kiro / AWS CodeWhisperer.
//
// Kiro does not own its login UI — operators authenticate via the Kiro IDE,
// AWS Builder ID, or a Kiro Social provider, then paste the resulting JSON
// token bundle into the proxy. This authenticator accepts that bundle, calls
// the upstream refresh endpoint to validate it, and persists a `kiro-*.json`
// credential file using the same FileTokenStore path as the other providers.
//
// LoginOptions.Metadata keys consumed:
//
//	auth_method      builder-id | idc | social  (auto-detected if empty)
//	access_token     optional initial access token
//	refresh_token    REQUIRED — the long-lived refresh token
//	client_id        AWS SSO OIDC client id (required for builder-id / idc)
//	client_secret    AWS SSO OIDC client secret (required for builder-id / idc)
//	region           AWS region for IDC (default us-east-1)
//	profile_arn      Optional CodeWhisperer profile ARN
//	email            Optional account label
//	bundle           Optional JSON bundle in any of these key shapes:
//	                   { "refreshToken": ..., "clientId": ..., "clientSecret": ..., "region": ..., "profileArn": ... }
//
// When `Prompt` is set on the login options and `bundle` is missing, the
// authenticator will ask the operator to paste the JSON bundle directly.
type KiroAuthenticator struct{}

// NewKiroAuthenticator constructs a new Kiro authenticator.
func NewKiroAuthenticator() Authenticator { return &KiroAuthenticator{} }

// Provider returns the provider key.
func (KiroAuthenticator) Provider() string { return "kiro" }

// RefreshLead reports how early the runtime should refresh the access token.
func (KiroAuthenticator) RefreshLead() *time.Duration { return &kiroRefreshLead }

// Login imports a Kiro credential bundle and persists it to the auth dir.
func (a KiroAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	bundle, err := buildKiroBundleFromOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("kiro: %w", err)
	}
	if strings.TrimSpace(bundle.RefreshToken) == "" && opts.Prompt != nil {
		raw, errPrompt := opts.Prompt("Paste Kiro credential JSON (must include refreshToken):")
		if errPrompt != nil {
			return nil, fmt.Errorf("kiro: prompt failed: %w", errPrompt)
		}
		if errParse := mergeKiroBundleFromJSON(raw, bundle); errParse != nil {
			return nil, fmt.Errorf("kiro: %w", errParse)
		}
	}
	if strings.TrimSpace(bundle.RefreshToken) == "" {
		return nil, fmt.Errorf("kiro: refresh_token is required")
	}
	if bundle.AuthMethod == "" {
		if strings.TrimSpace(bundle.ClientID) != "" && strings.TrimSpace(bundle.ClientSecret) != "" {
			bundle.AuthMethod = kiro.AuthMethodBuilderID
		} else {
			bundle.AuthMethod = kiro.AuthMethodSocial
		}
	}

	authSvc := kiro.NewKiroAuth(cfg)
	td, errRefresh := authSvc.RefreshAccessToken(ctx, bundle, http.DefaultClient)
	if errRefresh != nil {
		// If we already have an access token from the bundle, surface only a warning;
		// the executor will refresh on first use. Otherwise, fail the login.
		if strings.TrimSpace(bundle.AccessToken) == "" {
			return nil, fmt.Errorf("kiro: refresh failed: %w", errRefresh)
		}
		log.Warnf("kiro: pre-refresh failed, using imported access token: %v", errRefresh)
		td = &kiro.KiroTokenData{AccessToken: bundle.AccessToken, RefreshToken: bundle.RefreshToken, AuthMethod: bundle.AuthMethod}
	}

	storage := authSvc.CreateTokenStorage(bundle, td)
	metadata := map[string]any{
		"type":          "kiro",
		"auth_method":   storage.AuthMethod,
		"access_token":  storage.AccessToken,
		"refresh_token": storage.RefreshToken,
		"timestamp":     time.Now().UnixMilli(),
	}
	if storage.ClientID != "" {
		metadata["client_id"] = storage.ClientID
	}
	if storage.ClientSecret != "" {
		metadata["client_secret"] = storage.ClientSecret
	}
	if storage.Region != "" {
		metadata["region"] = storage.Region
	}
	if storage.ProfileArn != "" {
		metadata["profile_arn"] = storage.ProfileArn
	}
	if storage.Email != "" {
		metadata["email"] = storage.Email
	}
	if storage.Expired != "" {
		metadata["expired"] = storage.Expired
	}

	label := strings.TrimSpace(storage.Email)
	if label == "" {
		switch storage.AuthMethod {
		case kiro.AuthMethodIDC:
			label = "Kiro (IDC)"
		case kiro.AuthMethodBuilderID:
			label = "Kiro (Builder ID)"
		default:
			label = "Kiro User"
		}
	}

	fileName := fmt.Sprintf("kiro-%d.json", time.Now().UnixMilli())
	fmt.Println("\nKiro authentication successful!")

	return &coreauth.Auth{
		ID:       fileName,
		Provider: a.Provider(),
		FileName: fileName,
		Label:    label,
		Storage:  storage,
		Metadata: metadata,
	}, nil
}

// buildKiroBundleFromOptions seeds a KiroAuthBundle from LoginOptions.Metadata,
// honoring both flat keys and a single nested "bundle" JSON blob for convenience.
func buildKiroBundleFromOptions(opts *LoginOptions) (*kiro.KiroAuthBundle, error) {
	bundle := &kiro.KiroAuthBundle{}
	if opts == nil || opts.Metadata == nil {
		return bundle, nil
	}
	bundle.AuthMethod = strings.TrimSpace(opts.Metadata["auth_method"])
	bundle.AccessToken = strings.TrimSpace(opts.Metadata["access_token"])
	bundle.RefreshToken = strings.TrimSpace(opts.Metadata["refresh_token"])
	bundle.ClientID = strings.TrimSpace(opts.Metadata["client_id"])
	bundle.ClientSecret = strings.TrimSpace(opts.Metadata["client_secret"])
	bundle.Region = strings.TrimSpace(opts.Metadata["region"])
	bundle.ProfileArn = strings.TrimSpace(opts.Metadata["profile_arn"])
	bundle.Email = strings.TrimSpace(opts.Metadata["email"])
	if raw, ok := opts.Metadata["bundle"]; ok && strings.TrimSpace(raw) != "" {
		if err := mergeKiroBundleFromJSON(raw, bundle); err != nil {
			return bundle, err
		}
	}
	return bundle, nil
}

func mergeKiroBundleFromJSON(raw string, bundle *kiro.KiroAuthBundle) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return fmt.Errorf("invalid bundle JSON: %w", err)
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := decoded[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	if v := get("authMethod", "auth_method"); v != "" {
		bundle.AuthMethod = v
	}
	if v := get("accessToken", "access_token"); v != "" {
		bundle.AccessToken = v
	}
	if v := get("refreshToken", "refresh_token"); v != "" {
		bundle.RefreshToken = v
	}
	if v := get("clientId", "client_id"); v != "" {
		bundle.ClientID = v
	}
	if v := get("clientSecret", "client_secret"); v != "" {
		bundle.ClientSecret = v
	}
	if v := get("region"); v != "" {
		bundle.Region = v
	}
	if v := get("profileArn", "profile_arn"); v != "" {
		bundle.ProfileArn = v
	}
	if v := get("email"); v != "" {
		bundle.Email = v
	}
	return nil
}
