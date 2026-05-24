// Package kiro provides authentication and token management functionality
// for Kiro (AWS CodeWhisperer) services. This file owns the on-disk JSON
// shape used by the FileTokenStore.
package kiro

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

// KiroTokenStorage is the persisted credential record for a single Kiro account.
// The JSON keys mirror what the file watcher and FileTokenStore expect.
type KiroTokenStorage struct {
	// Type indicates the authentication provider type, always "kiro" for this storage.
	Type string `json:"type"`
	// AuthMethod records which Kiro auth flow produced these tokens (builder-id, idc, social).
	AuthMethod string `json:"auth_method,omitempty"`
	// AccessToken is the bearer token used for upstream Kiro requests.
	AccessToken string `json:"access_token"`
	// RefreshToken is the OAuth2 refresh token used to obtain new access tokens.
	RefreshToken string `json:"refresh_token"`
	// ClientID is the AWS SSO OIDC clientId; absent for Social Auth credentials.
	ClientID string `json:"client_id,omitempty"`
	// ClientSecret is the AWS SSO OIDC clientSecret; absent for Social Auth credentials.
	ClientSecret string `json:"client_secret,omitempty"`
	// Region is the AWS region used by AWS SSO OIDC IAM Identity Center auth.
	Region string `json:"region,omitempty"`
	// ProfileArn optionally pins the Kiro request to a specific CodeWhisperer profile.
	ProfileArn string `json:"profile_arn,omitempty"`
	// Email is the operator-provided account label used for dashboard display.
	Email string `json:"email,omitempty"`
	// Expired is the RFC3339 timestamp when the access token expires.
	Expired string `json:"expired,omitempty"`

	// Metadata holds arbitrary key-value pairs injected via hooks; not exported directly so it can be flattened during serialization.
	Metadata map[string]any `json:"-"`
}

// SetMetadata allows external callers to inject metadata into the storage before saving.
func (ts *KiroTokenStorage) SetMetadata(meta map[string]any) { ts.Metadata = meta }

// SaveTokenToFile serializes the Kiro token storage to a JSON file with 0600 perms.
func (ts *KiroTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "kiro"

	if err := os.MkdirAll(filepath.Dir(authFilePath), 0o700); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("failed to create token file: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("failed to merge metadata: %w", errMerge)
	}

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to write token to file: %w", err)
	}
	return nil
}

// IsExpired reports whether the access token is past its (refresh-threshold-adjusted) expiry.
func (ts *KiroTokenStorage) IsExpired() bool {
	if ts.Expired == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, ts.Expired)
	if err != nil {
		return true
	}
	return time.Now().Add(time.Duration(RefreshThresholdSeconds) * time.Second).After(t)
}

// NeedsRefresh reports whether a refresh attempt should be made before the next request.
func (ts *KiroTokenStorage) NeedsRefresh() bool {
	if ts.RefreshToken == "" {
		return false
	}
	return ts.IsExpired()
}

// KiroTokenData holds the parsed token response from upstream.
type KiroTokenData struct {
	AccessToken  string
	RefreshToken string
	AuthMethod   string
	ExpiresAt    int64
}

// KiroAuthBundle is the input the SDK authenticator hands to the Kiro auth helper.
// Operators populate it from a JSON import or interactive prompt. It is also the
// shape that the FileTokenStore reads back when a `kiro-*.json` file is present.
type KiroAuthBundle struct {
	AuthMethod   string
	AccessToken  string
	RefreshToken string
	ClientID     string
	ClientSecret string
	Region       string
	ProfileArn   string
	Email        string
}
