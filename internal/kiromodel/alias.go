package kiromodel

import "strings"

const (
	ThinkingSuffix = "-thinking"
	AgenticSuffix  = "-agentic"
)

func PublicID(upstream string) string {
	base, suffix := splitVariant(strings.TrimSpace(upstream))
	return publicBase(base) + suffix
}

func UpstreamID(public string) string {
	base, suffix := splitVariant(strings.TrimSpace(public))
	return upstreamBase(base) + suffix
}

func DisplayName(upstream string) string {
	base, suffix := splitVariant(strings.TrimSpace(upstream))
	name := "Kiro " + publicDisplayBase(base)
	switch suffix {
	case ThinkingSuffix:
		name += " (Thinking)"
	case AgenticSuffix:
		name += " (Agentic)"
	case ThinkingSuffix + AgenticSuffix:
		name += " (Thinking + Agentic)"
	}
	return name
}

func Description(upstream string) string {
	return DisplayName(upstream) + " served via Kiro."
}

func splitVariant(model string) (string, string) {
	model = strings.TrimSpace(model)
	suffix := ""
	if strings.HasSuffix(model, ThinkingSuffix+AgenticSuffix) {
		suffix = ThinkingSuffix + AgenticSuffix
		model = strings.TrimSuffix(model, suffix)
	} else if strings.HasSuffix(model, AgenticSuffix) {
		suffix = AgenticSuffix
		model = strings.TrimSuffix(model, suffix)
	} else if strings.HasSuffix(model, ThinkingSuffix) {
		suffix = ThinkingSuffix
		model = strings.TrimSuffix(model, suffix)
	}
	return model, suffix
}

func publicBase(base string) string {
	base = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(base, "kr/"), "kiro/"))
	if base == "" || strings.EqualFold(base, "auto") {
		return "auto"
	}
	lower := strings.ToLower(base)
	for _, rule := range []struct{ from, to string }{
		{"claude-opus-", "o-"},
		{"claude-sonnet-", "s-"},
		{"claude-haiku-", "h-"},
		{"opus-", "o-"},
		{"sonnet-", "s-"},
		{"haiku-", "h-"},
	} {
		if strings.HasPrefix(lower, rule.from) {
			return rule.to + base[len(rule.from):]
		}
	}
	if strings.HasPrefix(lower, "claude-") {
		return "cx-" + base[len("claude-"):]
	}
	return base
}

func upstreamBase(base string) string {
	base = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(base, "kr/"), "kiro/"))
	if base == "" || strings.EqualFold(base, "auto") {
		return "auto"
	}
	lower := strings.ToLower(base)
	switch {
	case strings.HasPrefix(lower, "o-"):
		return "claude-opus-" + base[2:]
	case strings.HasPrefix(lower, "s-"):
		return "claude-sonnet-" + base[2:]
	case strings.HasPrefix(lower, "h-"):
		return "claude-haiku-" + base[2:]
	case strings.HasPrefix(lower, "cx-"):
		return "claude-" + base[3:]
	default:
		return base
	}
}

func publicDisplayBase(upstream string) string {
	public := publicBase(upstream)
	if strings.EqualFold(public, "auto") {
		return "Auto"
	}
	if len(public) > 2 && public[1] == '-' {
		prefix := strings.ToUpper(public[:1])
		return prefix + " " + public[2:]
	}
	if strings.HasPrefix(strings.ToLower(public), "cx-") {
		return "CX " + public[3:]
	}
	return public
}
