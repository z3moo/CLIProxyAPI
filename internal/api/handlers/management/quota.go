package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	log "github.com/sirupsen/logrus"
)

// Quota exceeded toggles
func (h *Handler) GetSwitchProject(c *gin.Context) {
	c.JSON(200, gin.H{"switch-project": h.cfg.QuotaExceeded.SwitchProject})
}
func (h *Handler) PutSwitchProject(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchProject = v })
}

func (h *Handler) GetSwitchPreviewModel(c *gin.Context) {
	c.JSON(200, gin.H{"switch-preview-model": h.cfg.QuotaExceeded.SwitchPreviewModel})
}
func (h *Handler) PutSwitchPreviewModel(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchPreviewModel = v })
}

func normalizedAuthProvider(provider string) string {
	if isKiroProvider(provider) {
		return "kiro"
	}
	return strings.TrimSpace(provider)
}

func isKiroProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "kiro", "kiro-aws", "kiro-social":
		return true
	default:
		return false
	}
}

func (h *Handler) GetKiroQuota(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth manager not initialized"})
		return
	}

	name := strings.TrimSpace(c.Query("name"))
	authID := strings.TrimSpace(c.Query("auth_id"))
	refresh := strings.EqualFold(c.Query("refresh"), "true") || c.Query("refresh") == "1"
	exec := executor.NewKiroExecutor(h.cfg)
	items := make([]gin.H, 0)
	var first *executor.KiroQuotaSnapshot

	for _, auth := range h.authManager.List() {
		if auth == nil || !isKiroProvider(auth.Provider) {
			continue
		}
		if authID != "" && auth.ID != authID {
			continue
		}
		if name != "" && auth.FileName != name && auth.ID != name {
			continue
		}

		var err error
		snap := exec.CachedKiroQuota(auth.ID)
		if refresh || snap == nil {
			snap, err = exec.FetchKiroQuota(c.Request.Context(), auth)
		}
		if first == nil && snap != nil {
			first = snap
		}
		entry := gin.H{
			"auth_id":  auth.ID,
			"name":     auth.FileName,
			"provider": "kiro",
			"quota":    snap,
		}
		if err != nil {
			entry["error"] = err.Error()
		}
		items = append(items, entry)
	}

	resp := gin.H{"items": items}
	if first != nil {
		resp["plan"] = first.Plan
		resp["quotas"] = first.QuotaMap()
		resp["message"] = first.Message
		resp["fetched_at"] = first.FetchedAt
	}
	c.JSON(http.StatusOK, resp)
}

// PurgeBadCredentialsRequest scopes the credential cleanup probe to a subset
// of providers. An empty Providers slice probes every supported provider.
type PurgeBadCredentialsRequest struct {
	Providers []string `json:"providers"`
	DryRun    bool     `json:"dry_run"`
}

// PurgeBadCredentials probes Codex and Gemini auths against their upstream
// quota endpoints and removes auths whose credentials are dead:
//
//   - Codex: HTTP 401 with "Provided authentication token is expired"
//   - Gemini: response containing "Please check the credential status"
//
// It mirrors what 9router's quota dashboard surfaces by deleting the
// underlying auth file via the existing deleteAuthFileByName path.
func (h *Handler) PurgeBadCredentials(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth manager not initialized"})
		return
	}

	var req PurgeBadCredentialsRequest
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
			return
		}
	}

	wantCodex, wantGemini := scopeProviders(req.Providers)

	type credentialResult struct {
		AuthID    string `json:"auth_id"`
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		Status    int    `json:"status"`
		Reason    string `json:"reason,omitempty"`
		Removed   bool   `json:"removed"`
		Error     string `json:"error,omitempty"`
		ProbeNote string `json:"probe_note,omitempty"`
	}

	results := make([]credentialResult, 0)
	removed := 0
	checked := 0
	ctx := c.Request.Context()

	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		switch {
		case wantCodex && provider == "codex":
		case wantGemini && (provider == "gemini" || provider == "gemini-cli"):
		default:
			continue
		}

		checked++
		entry := credentialResult{
			AuthID:   auth.ID,
			Name:     auth.FileName,
			Provider: auth.Provider,
		}

		var (
			status executor.CredentialQuotaStatus
			err    error
		)
		if provider == "codex" {
			status, err = executor.ProbeCodexCredential(ctx, h.cfg, auth)
		} else {
			status, err = executor.ProbeGeminiCredential(ctx, h.cfg, auth)
		}
		if err != nil {
			entry.Error = err.Error()
			entry.ProbeNote = "probe transport error; auth left in place"
			results = append(results, entry)
			continue
		}
		entry.Status = status.Status
		entry.Reason = status.Reason
		if !status.Revocable {
			entry.ProbeNote = "credential healthy or non-fatal failure; auth left in place"
			results = append(results, entry)
			continue
		}

		if req.DryRun {
			entry.ProbeNote = "would remove; dry_run=true"
			results = append(results, entry)
			continue
		}

		deleteName := strings.TrimSpace(auth.FileName)
		if deleteName == "" {
			deleteName = strings.TrimSpace(auth.ID)
		}
		if deleteName == "" {
			entry.Error = "auth has no filename or id; cannot delete"
			results = append(results, entry)
			continue
		}
		entry.Name = deleteName
		if _, _, errDelete := h.deleteAuthFileByName(ctx, deleteName); errDelete != nil {
			entry.Error = errDelete.Error()
			results = append(results, entry)
			continue
		}
		entry.Removed = true
		removed++
		log.Warnf("removed auth %s (%s) due to %s: %s", auth.ID, auth.Provider, status.Provider, status.Reason)
		results = append(results, entry)
	}

	c.JSON(http.StatusOK, gin.H{
		"checked":   checked,
		"removed":   removed,
		"dry_run":   req.DryRun,
		"providers": resolvedProviderList(wantCodex, wantGemini),
		"results":   results,
	})
}

func scopeProviders(provided []string) (codex bool, gemini bool) {
	if len(provided) == 0 {
		return true, true
	}
	for _, p := range provided {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "codex":
			codex = true
		case "gemini", "gemini-cli":
			gemini = true
		}
	}
	return codex, gemini
}

func resolvedProviderList(codex, gemini bool) []string {
	out := []string{}
	if codex {
		out = append(out, "codex")
	}
	if gemini {
		out = append(out, "gemini")
	}
	return out
}
