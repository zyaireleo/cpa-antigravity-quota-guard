package guard

import "strings"

// ClassifyModel maps a CPA model identifier to its independent Antigravity
// quota group. The boolean is false for ambiguous or unknown model names.
func ClassifyModel(model string) (ModelGroup, bool) {
	text := strings.ToLower(strings.TrimSpace(model))
	if text == "" {
		return "", false
	}
	hasGemini := strings.Contains(text, "gemini")
	hasClaudeGPT := strings.Contains(text, "claude") || strings.Contains(text, "gpt") || strings.Contains(text, "openai")
	if hasGemini == hasClaudeGPT {
		return "", false
	}
	if hasGemini {
		return ModelGroupGemini, true
	}
	return ModelGroupClaudeGPT, true
}

// IsAntigravityProvider deliberately accepts only Antigravity provider aliases.
// A plain "google" or "gemini" provider may be a different CPA executor and
// must not be captured accidentally by this plugin.
func IsAntigravityProvider(provider string) bool {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "antigravity", "google-antigravity", "antigravity-oauth":
		return true
	default:
		return false
	}
}

func routeIncludesAntigravity(req PickRequest) bool {
	if IsAntigravityProvider(req.Provider) {
		return true
	}
	for _, provider := range req.Providers {
		if IsAntigravityProvider(provider) {
			return true
		}
	}
	for _, candidate := range req.Candidates {
		if IsAntigravityProvider(candidate.Provider) {
			return true
		}
	}
	return false
}

func groupEnforced(group ModelGroup, groups []ModelGroup) bool {
	if len(groups) == 0 {
		return true
	}
	for _, candidate := range groups {
		if candidate == group {
			return true
		}
	}
	return false
}
