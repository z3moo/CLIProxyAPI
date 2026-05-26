// Package executor: background refresher for Kiro live models + quota.
//
// One goroutine per process. Every tick (kiroRefreshInterval) it walks every
// Kiro Auth tracked by the runtime, refreshes the live model catalog and the
// quota snapshot, and pushes the resulting model list into the global model
// registry so /v1/models reflects what the account can actually use.
package executor

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// kiroRefreshInterval is how often the refresher walks every Kiro auth.
	// Matches kimi/codex auto-refresh cadence.
	kiroRefreshInterval = 10 * time.Minute
	// kiroRefreshKickoffDelay is how long the refresher waits after start
	// before its first tick (lets the rest of the system settle).
	kiroRefreshKickoffDelay = 30 * time.Second
)

// KiroAuthLister is the minimal slice of cliproxyauth.Manager the refresher
// needs. The service supplies a closure backed by `coreManager.List()`.
type KiroAuthLister func() []*cliproxyauth.Auth

// KiroModelRegistrar is what the refresher uses to wire freshly fetched live
// model catalogs into the global model registry. Wrapping the global API in
// this interface keeps the executor package decoupled from the SDK service.
type KiroModelRegistrar interface {
	RegisterClient(authID, providerKey string, models []*registry.ModelInfo)
	UnregisterClient(authID string)
}

// KiroRefresher walks every Kiro auth on a fixed interval and keeps the
// per-auth model catalog + quota snapshot fresh in the executor caches.
//
// It is safe for concurrent Trigger() calls; reactive nudges from auth
// imports / token refresh callers are coalesced with the periodic ticks.
type KiroRefresher struct {
	executor  *KiroExecutor
	lister    KiroAuthLister
	registrar KiroModelRegistrar

	mu        sync.Mutex
	cancel    context.CancelFunc
	trigger   chan struct{}
	startOnce sync.Once
}

// NewKiroRefresher builds a refresher; Start launches the goroutine.
func NewKiroRefresher(exec *KiroExecutor, lister KiroAuthLister, registrar KiroModelRegistrar) *KiroRefresher {
	return &KiroRefresher{
		executor:  exec,
		lister:    lister,
		registrar: registrar,
		trigger:   make(chan struct{}, 1),
	}
}

// Start kicks off the refresher goroutine. Idempotent.
func (r *KiroRefresher) Start(ctx context.Context) {
	if r == nil || r.executor == nil || r.lister == nil {
		return
	}
	r.startOnce.Do(func() {
		runCtx, cancel := context.WithCancel(ctx)
		r.cancel = cancel
		go r.loop(runCtx)
	})
}

// Stop halts the refresher goroutine.
func (r *KiroRefresher) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Trigger schedules an out-of-band tick. Safe to call repeatedly; extras are
// coalesced.
func (r *KiroRefresher) Trigger() {
	if r == nil {
		return
	}
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

func (r *KiroRefresher) loop(ctx context.Context) {
	// Initial delay so we don't fight startup work.
	select {
	case <-time.After(kiroRefreshKickoffDelay):
	case <-ctx.Done():
		return
	}
	r.tick(ctx)

	t := time.NewTicker(kiroRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		case <-r.trigger:
			r.tick(ctx)
		}
	}
}

func (r *KiroRefresher) tick(ctx context.Context) {
	auths := r.lister()
	if len(auths) == 0 {
		return
	}
	for _, auth := range auths {
		if auth == nil || !isKiroProviderName(auth.Provider) {
			continue
		}
		if auth.Disabled {
			if r.registrar != nil {
				r.registrar.UnregisterClient(auth.ID)
			}
			continue
		}
		r.refreshOne(ctx, auth)
	}
}

func (r *KiroRefresher) refreshOne(ctx context.Context, auth *cliproxyauth.Auth) {
	models, _, err := r.executor.FetchKiroModels(ctx, auth)
	if err != nil {
		log.Debugf("kiro refresher: %s: model fetch failed: %v", auth.ID, err)
	} else if r.registrar != nil && len(models) > 0 {
		r.registrar.RegisterClient(auth.ID, kiroProvider, mergeKiroModelInfos(registry.GetKiroModels(), models))
	}

	if _, err := r.executor.FetchKiroQuota(ctx, auth); err != nil {
		log.Debugf("kiro refresher: %s: quota fetch failed: %v", auth.ID, err)
	}
}

func isKiroProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "kiro", "kiro-aws", "kiro-social":
		return true
	default:
		return false
	}
}

func mergeKiroModelInfos(base []*registry.ModelInfo, live []*registry.ModelInfo) []*registry.ModelInfo {
	merged := make([]*registry.ModelInfo, 0, len(base)+len(live))
	seen := map[string]struct{}{}
	for _, model := range append(base, live...) {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		if _, ok := seen[model.ID]; ok {
			continue
		}
		seen[model.ID] = struct{}{}
		merged = append(merged, model)
	}
	return merged
}
