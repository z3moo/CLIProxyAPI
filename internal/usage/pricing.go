// Package usage records per-request token totals and computes estimated costs.
//
// The pricing tables in this file are a Go port of 9router's
// src/shared/constants/pricing.js. All rates are in $ per 1,000,000 tokens.
//
// Lookup order (first match wins):
//  1. providerPricing[provider][model]   - provider-specific override
//  2. modelPricing[baseModel] / [model]  - canonical price (provider-agnostic)
//  3. patternPricing                     - glob match ("*-codex-mini", "claude-opus-*")
package usage

import (
	"regexp"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/kiromodel"
)

// Pricing holds the four-rate cost row for a single model.
// Rates are per 1,000,000 tokens.
type Pricing struct {
	Input         float64
	Output        float64
	Cached        float64
	Reasoning     float64
	CacheCreation float64
}

var modelPricing = map[string]Pricing{
	// Claude
	"claude-opus-4-6":            {5.00, 25.00, 0.50, 25.00, 6.25},
	"claude-opus-4-5-20251101":   {5.00, 25.00, 0.50, 25.00, 6.25},
	"claude-sonnet-4-6":          {3.00, 15.00, 0.30, 15.00, 3.75},
	"claude-sonnet-4-5-20250929": {3.00, 15.00, 0.30, 15.00, 3.75},
	"claude-haiku-4-5-20251001":  {1.00, 5.00, 0.10, 5.00, 1.25},
	"claude-sonnet-4-20250514":   {3.00, 15.00, 1.50, 15.00, 3.00},
	"claude-opus-4-20250514":     {15.00, 25.00, 7.50, 112.50, 15.00},
	"claude-3-5-sonnet-20241022": {3.00, 15.00, 1.50, 15.00, 3.00},
	"claude-haiku-4.5":           {0.50, 2.50, 0.05, 3.75, 0.50},
	"claude-opus-4.1":            {5.00, 25.00, 0.50, 37.50, 5.00},
	"claude-opus-4.5":            {5.00, 25.00, 0.50, 37.50, 5.00},
	"claude-opus-4.6":            {5.00, 25.00, 0.50, 37.50, 5.00},
	"claude-opus-4.7":            {5.00, 25.00, 0.50, 37.50, 5.00},
	"claude-sonnet-4":            {3.00, 15.00, 0.30, 22.50, 3.00},
	"claude-sonnet-4.5":          {3.00, 15.00, 0.30, 22.50, 3.00},
	"claude-sonnet-4.6":          {3.00, 15.00, 0.30, 22.50, 3.00},
	"claude-opus-4-5-thinking":   {5.00, 25.00, 0.50, 37.50, 5.00},
	"claude-opus-4-6-thinking":   {5.00, 25.00, 0.50, 37.50, 5.00},

	// OpenAI / GPT
	"gpt-3.5-turbo":           {0.50, 1.50, 0.25, 2.25, 0.50},
	"gpt-4":                   {2.50, 10.00, 1.25, 15.00, 2.50},
	"gpt-4-turbo":             {10.00, 30.00, 5.00, 45.00, 10.00},
	"gpt-4o":                  {2.50, 10.00, 1.25, 15.00, 2.50},
	"gpt-4o-mini":             {0.15, 0.60, 0.075, 0.90, 0.15},
	"gpt-4.1":                 {2.50, 10.00, 1.25, 15.00, 2.50},
	"gpt-5":                   {3.00, 12.00, 1.50, 18.00, 3.00},
	"gpt-5-mini":              {0.75, 3.00, 0.375, 4.50, 0.75},
	"gpt-5-codex":             {3.00, 12.00, 1.50, 18.00, 3.00},
	"gpt-5.1":                 {4.00, 16.00, 2.00, 24.00, 4.00},
	"gpt-5.1-codex":           {4.00, 16.00, 2.00, 24.00, 4.00},
	"gpt-5.1-codex-mini":      {1.50, 6.00, 0.75, 9.00, 1.50},
	"gpt-5.1-codex-mini-high": {2.00, 8.00, 1.00, 12.00, 2.00},
	"gpt-5.1-codex-max":       {8.00, 32.00, 4.00, 48.00, 8.00},
	"gpt-5.2":                 {5.00, 20.00, 2.50, 30.00, 5.00},
	"gpt-5.2-codex":           {5.00, 20.00, 2.50, 30.00, 5.00},
	"gpt-5.3-codex":           {6.00, 24.00, 3.00, 36.00, 6.00},
	"gpt-5.3-codex-xhigh":     {10.00, 40.00, 5.00, 60.00, 10.00},
	"gpt-5.3-codex-high":      {8.00, 32.00, 4.00, 48.00, 8.00},
	"gpt-5.3-codex-low":       {4.00, 16.00, 2.00, 24.00, 4.00},
	"gpt-5.3-codex-none":      {3.00, 12.00, 1.50, 18.00, 3.00},
	"gpt-5.3-codex-spark":     {3.00, 12.00, 0.30, 12.00, 3.00},
	"o1":                      {15.00, 60.00, 7.50, 90.00, 15.00},
	"o1-mini":                 {3.00, 12.00, 1.50, 18.00, 3.00},

	// Gemini
	"gemini-3-flash-preview": {0.50, 3.00, 0.03, 4.50, 0.50},
	"gemini-3-pro-preview":   {2.00, 12.00, 0.25, 18.00, 2.00},
	"gemini-3.1-pro-low":     {2.00, 12.00, 0.25, 18.00, 2.00},
	"gemini-3.1-pro-high":    {4.00, 18.00, 0.50, 27.00, 4.00},
	"gemini-pro-agent":       {4.00, 18.00, 0.50, 27.00, 4.00},
	"gemini-3-flash-agent":   {0.50, 3.00, 0.03, 4.50, 0.50},
	"gemini-3.5-flash-low":   {0.50, 3.00, 0.03, 4.50, 0.50},
	"gemini-3-flash":         {0.50, 3.00, 0.03, 4.50, 0.50},
	"gemini-2.5-pro":         {2.00, 12.00, 0.25, 18.00, 2.00},
	"gemini-2.5-flash":       {0.30, 2.50, 0.03, 3.75, 0.30},
	"gemini-2.5-flash-lite":  {0.15, 1.25, 0.015, 1.875, 0.15},

	// Qwen
	"qwen3-coder-plus":  {1.00, 4.00, 0.50, 6.00, 1.00},
	"qwen3-coder-flash": {0.50, 2.00, 0.25, 3.00, 0.50},

	// Kimi
	"kimi-k2":            {1.00, 4.00, 0.50, 6.00, 1.00},
	"kimi-k2-thinking":   {1.50, 6.00, 0.75, 9.00, 1.50},
	"kimi-k2.5":          {1.20, 4.80, 0.60, 7.20, 1.20},
	"kimi-k2.5-thinking": {1.80, 7.20, 0.90, 10.80, 1.80},
	"kimi-latest":        {1.00, 4.00, 0.50, 6.00, 1.00},

	// DeepSeek
	"deepseek-chat":          {0.14, 0.28, 0.0028, 0.28, 0.14},
	"deepseek-reasoner":      {0.14, 0.28, 0.0028, 0.28, 0.14},
	"deepseek-r1":            {0.14, 0.28, 0.0028, 0.28, 0.14},
	"deepseek-v3.2-chat":     {0.14, 0.28, 0.0028, 0.28, 0.14},
	"deepseek-v3.2-reasoner": {0.14, 0.28, 0.0028, 0.28, 0.14},
	"deepseek-v4-flash":      {0.14, 0.28, 0.0028, 0.28, 0.14},
	"deepseek-v4-pro":        {0.435, 0.87, 0.003625, 0.87, 0.435},

	// GLM
	"glm-4.6":  {0.50, 2.00, 0.25, 3.00, 0.50},
	"glm-4.6v": {0.75, 3.00, 0.375, 4.50, 0.75},
	"glm-4.7":  {0.75, 3.00, 0.375, 4.50, 0.75},
	"glm-5":    {1.00, 4.00, 0.50, 6.00, 1.00},

	// MiniMax
	"MiniMax-M2.1": {0.50, 2.00, 0.25, 3.00, 0.50},
	"MiniMax-M2.5": {0.50, 2.00, 0.25, 3.00, 0.50},
	"MiniMax-M2.7": {0.50, 2.00, 0.25, 3.00, 0.50},
	"minimax-m2.1": {0.50, 2.00, 0.25, 3.00, 0.50},
	"minimax-m2.5": {0.60, 2.40, 0.30, 3.60, 0.60},

	// Grok / xAI
	"grok-code-fast-1": {0.50, 2.00, 0.25, 3.00, 0.50},

	// OpenRouter / catch-all
	"auto": {2.00, 8.00, 1.00, 12.00, 2.00},

	// Misc
	"oswe-vscode-prime":   {1.00, 4.00, 0.50, 6.00, 1.00},
	"gpt-oss-120b-medium": {0.50, 2.00, 0.25, 3.00, 0.50},
	"vision-model":        {1.50, 6.00, 0.75, 9.00, 1.50},
	"coder-model":         {1.50, 6.00, 0.75, 9.00, 1.50},
}

// providerPricing holds provider-specific overrides where pricing differs from
// the canonical rate (e.g. GitHub Copilot rebates).
var providerPricing = map[string]map[string]Pricing{
	"gh": {
		"gpt-5.3-codex": {1.75, 14.00, 0.175, 14.00, 1.75},
	},
}

type patternRule struct {
	pattern string
	regex   *regexp.Regexp
	pricing Pricing
}

var patternPricing = compilePatterns([]struct {
	pattern string
	pricing Pricing
}{
	// Codex variants
	{"*-codex-xhigh", Pricing{10.00, 40.00, 5.00, 60.00, 10.00}},
	{"*-codex-high", Pricing{8.00, 32.00, 4.00, 48.00, 8.00}},
	{"*-codex-max", Pricing{8.00, 32.00, 4.00, 48.00, 8.00}},
	{"*-codex-mini-*", Pricing{1.50, 6.00, 0.75, 9.00, 1.50}},
	{"*-codex-mini", Pricing{1.50, 6.00, 0.75, 9.00, 1.50}},
	{"*-codex-low", Pricing{4.00, 16.00, 2.00, 24.00, 4.00}},
	{"*-codex-none", Pricing{3.00, 12.00, 1.50, 18.00, 3.00}},
	{"*-codex-spark", Pricing{3.00, 12.00, 0.30, 12.00, 3.00}},
	{"codex-*", Pricing{3.00, 12.00, 1.50, 18.00, 3.00}},
	{"*-codex", Pricing{3.00, 12.00, 1.50, 18.00, 3.00}},

	// Claude
	{"claude-opus-*", Pricing{5.00, 25.00, 0.50, 25.00, 6.25}},
	{"claude-sonnet-*", Pricing{3.00, 15.00, 0.30, 15.00, 3.75}},
	{"claude-haiku-*", Pricing{1.00, 5.00, 0.10, 5.00, 1.25}},
	{"claude-*", Pricing{3.00, 15.00, 0.30, 15.00, 3.75}},

	// Gemini (specific first)
	{"gemini-*-flash-lite", Pricing{0.15, 1.25, 0.015, 1.875, 0.15}},
	{"gemini-*-flash", Pricing{0.30, 2.50, 0.03, 3.75, 0.30}},
	{"gemini-*-pro", Pricing{2.00, 12.00, 0.25, 18.00, 2.00}},
	{"gemini-3-*", Pricing{0.50, 3.00, 0.03, 4.50, 0.50}},
	{"gemini-2.5-*", Pricing{0.30, 2.50, 0.03, 3.75, 0.30}},
	{"gemini-*", Pricing{0.50, 3.00, 0.03, 4.50, 0.50}},

	// GPT (specific first)
	{"gpt-5.3-*", Pricing{6.00, 24.00, 3.00, 36.00, 6.00}},
	{"gpt-5.2-*", Pricing{5.00, 20.00, 2.50, 30.00, 5.00}},
	{"gpt-5.1-*", Pricing{4.00, 16.00, 2.00, 24.00, 4.00}},
	{"gpt-5-*", Pricing{3.00, 12.00, 1.50, 18.00, 3.00}},
	{"gpt-5*", Pricing{3.00, 12.00, 1.50, 18.00, 3.00}},
	{"gpt-4o-*", Pricing{0.15, 0.60, 0.075, 0.90, 0.15}},
	{"gpt-4o", Pricing{2.50, 10.00, 1.25, 15.00, 2.50}},
	{"gpt-4*", Pricing{2.50, 10.00, 1.25, 15.00, 2.50}},

	// o-series
	{"o1-*", Pricing{3.00, 12.00, 1.50, 18.00, 3.00}},
	{"o1", Pricing{15.00, 60.00, 7.50, 90.00, 15.00}},
	{"o3-*", Pricing{10.00, 40.00, 5.00, 60.00, 10.00}},
	{"o4-*", Pricing{2.00, 8.00, 1.00, 12.00, 2.00}},

	// Qwen
	{"qwen3-coder-*", Pricing{1.00, 4.00, 0.50, 6.00, 1.00}},
	{"qwen*-coder-*", Pricing{1.00, 4.00, 0.50, 6.00, 1.00}},
	{"qwen*", Pricing{0.50, 2.00, 0.25, 3.00, 0.50}},

	// Kimi
	{"kimi-*-thinking", Pricing{1.80, 7.20, 0.90, 10.80, 1.80}},
	{"kimi-k2*", Pricing{1.20, 4.80, 0.60, 7.20, 1.20}},
	{"kimi-*", Pricing{1.00, 4.00, 0.50, 6.00, 1.00}},

	// DeepSeek
	{"deepseek-*reasoner*", Pricing{0.14, 0.28, 0.0028, 0.28, 0.14}},
	{"deepseek-r*", Pricing{0.14, 0.28, 0.0028, 0.28, 0.14}},
	{"deepseek-v*", Pricing{0.14, 0.28, 0.0028, 0.28, 0.14}},
	{"deepseek-*", Pricing{0.14, 0.28, 0.0028, 0.28, 0.14}},

	// GLM
	{"glm-5*", Pricing{1.00, 4.00, 0.50, 6.00, 1.00}},
	{"glm-4*", Pricing{0.75, 3.00, 0.375, 4.50, 0.75}},
	{"glm-*", Pricing{0.50, 2.00, 0.25, 3.00, 0.50}},

	// MiniMax
	{"MiniMax-*", Pricing{0.50, 2.00, 0.25, 3.00, 0.50}},
	{"minimax-*", Pricing{0.50, 2.00, 0.25, 3.00, 0.50}},

	// Grok
	{"grok-code-*", Pricing{0.50, 2.00, 0.25, 3.00, 0.50}},
	{"grok-*", Pricing{0.50, 2.00, 0.25, 3.00, 0.50}},
})

func compilePatterns(rules []struct {
	pattern string
	pricing Pricing
}) []patternRule {
	out := make([]patternRule, 0, len(rules))
	for _, r := range rules {
		parts := strings.Split(r.pattern, "*")
		for i, p := range parts {
			parts[i] = regexp.QuoteMeta(p)
		}
		expr := "^" + strings.Join(parts, ".*") + "$"
		out = append(out, patternRule{
			pattern: r.pattern,
			regex:   regexp.MustCompile(expr),
			pricing: r.pricing,
		})
	}
	return out
}

var pricingResolveCacheMu sync.Mutex
var pricingResolveCache = map[string]Pricing{}

// stripModelPrefix removes proxy-side prefixes like "kr/", "kiro/", or any
// "vendor/" prefix to get the canonical model name used in the pricing table.
func stripModelPrefix(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if i := strings.LastIndex(model, "/"); i >= 0 && i < len(model)-1 {
		return model[i+1:]
	}
	return model
}

// LookupPricing resolves a (provider, model) pair to a pricing row using the
// 9router fallback chain. Returns false when no rule matches.
func LookupPricing(provider, model string) (Pricing, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return Pricing{}, false
	}

	cacheKey := strings.ToLower(strings.TrimSpace(provider)) + "|" + model
	pricingResolveCacheMu.Lock()
	if cached, ok := pricingResolveCache[cacheKey]; ok {
		pricingResolveCacheMu.Unlock()
		if cached == (Pricing{}) {
			return Pricing{}, false
		}
		return cached, true
	}
	pricingResolveCacheMu.Unlock()

	resolved, ok := resolvePricing(provider, model)
	pricingResolveCacheMu.Lock()
	pricingResolveCache[cacheKey] = resolved
	pricingResolveCacheMu.Unlock()
	return resolved, ok
}

func resolvePricing(provider, model string) (Pricing, bool) {
	if provider != "" {
		if byProv, ok := providerPricing[strings.ToLower(strings.TrimSpace(provider))]; ok {
			if p, ok := byProv[model]; ok {
				return p, true
			}
		}
	}
	base := stripModelPrefix(model)
	canonicalBase := kiromodel.UpstreamID(base)
	canonicalModel := kiromodel.UpstreamID(model)
	if p, ok := modelPricing[canonicalBase]; ok {
		return p, true
	}
	if p, ok := modelPricing[base]; ok {
		return p, true
	}
	if p, ok := modelPricing[canonicalModel]; ok {
		return p, true
	}
	if p, ok := modelPricing[model]; ok {
		return p, true
	}
	for _, rule := range patternPricing {
		if rule.regex.MatchString(canonicalBase) || rule.regex.MatchString(base) || rule.regex.MatchString(canonicalModel) || rule.regex.MatchString(model) {
			return rule.pricing, true
		}
	}
	return Pricing{}, false
}

// CalculateCost computes the estimated dollar cost for a single request given
// per-million-token rates. Cached input tokens are billed at the cached rate
// (subtracted from the input rate); reasoning falls back to output if missing.
func CalculateCost(p Pricing, inputTokens, outputTokens, reasoningTokens, cachedTokens, cacheCreationTokens int64) float64 {
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	if reasoningTokens < 0 {
		reasoningTokens = 0
	}
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cacheCreationTokens < 0 {
		cacheCreationTokens = 0
	}

	nonCachedInput := inputTokens - cachedTokens
	if nonCachedInput < 0 {
		nonCachedInput = 0
	}

	const million = 1_000_000.0
	cost := float64(nonCachedInput) * (p.Input / million)

	if cachedTokens > 0 {
		rate := p.Cached
		if rate == 0 {
			rate = p.Input
		}
		cost += float64(cachedTokens) * (rate / million)
	}

	cost += float64(outputTokens) * (p.Output / million)

	if reasoningTokens > 0 {
		rate := p.Reasoning
		if rate == 0 {
			rate = p.Output
		}
		cost += float64(reasoningTokens) * (rate / million)
	}

	if cacheCreationTokens > 0 {
		rate := p.CacheCreation
		if rate == 0 {
			rate = p.Input
		}
		cost += float64(cacheCreationTokens) * (rate / million)
	}

	return cost
}
