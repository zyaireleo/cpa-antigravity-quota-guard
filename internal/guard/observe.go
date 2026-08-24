package guard

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxInspectedFailureBody = 64 << 10

// ApplyQuotaEvidence records a successful, trusted quota probe. A failed
// probe must instead call RecordQuotaProbeError, which deliberately preserves
// the current breaker state.
func (e *Engine) ApplyQuotaEvidence(evidence QuotaEvidence) (EvidenceResult, error) {
	if strings.TrimSpace(evidence.AuthIndex) == "" || !evidence.Group.Valid() ||
		math.IsNaN(evidence.Remaining) || math.IsInf(evidence.Remaining, 0) ||
		evidence.Remaining < 0 || evidence.Remaining > 100 {
		return EvidenceResult{}, ErrInvalidEvidence
	}
	if evidence.ObservedAt.IsZero() {
		evidence.ObservedAt = time.Now().UTC()
	} else {
		evidence.ObservedAt = evidence.ObservedAt.UTC()
	}
	if evidence.Source == "" {
		evidence.Source = evidenceSourceQuotaProbe
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.metrics.QuotaProbeSuccessTotal++
	entry := e.entries[entryKey(strings.TrimSpace(evidence.AuthIndex), evidence.Group)]
	if entry == nil {
		e.metrics.UnknownAuthTotal++
		return EvidenceResult{Ignored: true, State: StateUninitialized}, nil
	}
	if evidence.AuthID != "" && entry.AuthID != strings.TrimSpace(evidence.AuthID) {
		e.metrics.UnknownAuthTotal++
		return EvidenceResult{Ignored: true, State: entry.State, Reason: entry.Reason}, nil
	}
	if evidence.IdentityFingerprint != "" && entry.IdentityFingerprint != strings.TrimSpace(evidence.IdentityFingerprint) {
		e.metrics.UnknownAuthTotal++
		return EvidenceResult{Ignored: true, State: entry.State, Reason: entry.Reason}, nil
	}
	if !entry.LastEvidenceAt.IsZero() && evidence.ObservedAt.Before(entry.LastEvidenceAt) {
		return EvidenceResult{Ignored: true, State: entry.State, Reason: entry.Reason}, nil
	}

	before := *entry
	remaining := evidence.Remaining
	entry.RemainingPercent = &remaining
	entry.LastEvidenceAt = evidence.ObservedAt
	entry.LastQuotaEvidenceAt = evidence.ObservedAt
	entry.LastEvidenceSource = sanitizeEvidenceSource(evidence.Source)

	if evidence.Remaining <= 0 && evidence.ResetAt.After(evidence.ObservedAt) {
		e.openLocked(entry, ReasonQuotaZero, evidence.ObservedAt, evidence.ResetAt.UTC(), evidence.ResetAt.UTC())
	} else if evidence.Remaining > 0 && (entry.State == StateUninitialized || entry.Reason == ReasonQuotaZero || entry.Reason == ReasonManualProbe) {
		e.closeLocked(entry)
	}
	changed := !entryEqual(before, *entry)
	if changed {
		e.markDirtyLocked(evidence.ObservedAt)
	}
	return EvidenceResult{Changed: changed, State: entry.State, Reason: entry.Reason}, nil
}

func sanitizeEvidenceSource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if len(source) > 64 {
		source = source[:64]
	}
	var result strings.Builder
	for _, character := range source {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
			character == '_' || character == '-' || character == '.' {
			result.WriteRune(character)
		}
	}
	if result.Len() == 0 {
		return evidenceSourceQuotaProbe
	}
	return result.String()
}

// RecordQuotaProbeError increments observability counters without changing an
// account's state, availability, reset, or recovery time.
func (e *Engine) RecordQuotaProbeError() {
	e.mu.Lock()
	e.metrics.QuotaProbeErrorTotal++
	e.mu.Unlock()
}

// ObserveUsage updates breaker state from a normalized CPA UsageRecord. The
// raw failure body is inspected in memory only and is never retained.
func (e *Engine) ObserveUsage(observation UsageObservation) ObservationResult {
	if !IsAntigravityProvider(observation.Provider) {
		return ObservationResult{}
	}
	group, known := ClassifyModel(observation.Model)
	if !known {
		e.mu.Lock()
		e.metrics.UnknownModelTotal++
		e.mu.Unlock()
		return ObservationResult{}
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = time.Now().UTC()
	} else {
		observation.ObservedAt = observation.ObservedAt.UTC()
	}
	if observation.RequestedAt.IsZero() {
		observation.RequestedAt = observation.ObservedAt
	} else {
		observation.RequestedAt = observation.RequestedAt.UTC()
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	entry := e.lookupEntryLocked(observation.AuthIndex, observation.AuthID, group)
	if entry == nil {
		e.metrics.UnknownAuthTotal++
		return ObservationResult{Handled: true, Group: group, State: StateUninitialized}
	}
	// CPA delivers Usage records asynchronously. Older completions must not
	// overwrite quota or usage evidence that is already known to be fresher.
	if !entry.LastEvidenceAt.IsZero() && observation.ObservedAt.Before(entry.LastEvidenceAt) {
		return ObservationResult{Handled: true, Group: group, State: entry.State, Reason: entry.Reason}
	}
	before := *entry
	if entry.State == StateHalfOpen && entry.halfOpenRequestID != "" &&
		strings.TrimSpace(observation.RequestID) != entry.halfOpenRequestID {
		return ObservationResult{Handled: true, Group: group, State: entry.State, Reason: entry.Reason}
	}

	if !observation.Failed {
		if entry.State == StateHalfOpen && !entry.HalfOpenStartedAt.IsZero() &&
			!observation.ObservedAt.Before(entry.HalfOpenStartedAt) &&
			entry.halfOpenRequestID != "" && strings.TrimSpace(observation.RequestID) == entry.halfOpenRequestID {
			e.metrics.HalfOpenSuccessTotal++
			e.closeLocked(entry)
			entry.LastEvidenceAt = observation.ObservedAt
			entry.LastEvidenceSource = evidenceSourceUsageOK
		} else if entry.State == StateClosed && !observation.ObservedAt.Before(entry.LastFailureAt) {
			entry.FailureCount = 0
			entry.LastFailureAt = time.Time{}
		}
	} else if observation.StatusCode == http.StatusTooManyRequests {
		recoverAt, hasReset := extractResetAt(observation.Headers, observation.Body, observation.ObservedAt)
		if explicitQuotaExhaustion(observation.Body) {
			if !hasReset || !recoverAt.After(observation.ObservedAt) {
				recoverAt = observation.ObservedAt.Add(e.config.Generic429MaxCooldown)
			}
			reason := ReasonExplicit429
			if entry.State == StateOpen && observation.ObservedAt.Before(entry.RecoverAt) &&
				(entry.Reason == ReasonQuotaZero || entry.Reason == ReasonExplicit429) {
				if entry.RecoverAt.After(recoverAt) {
					recoverAt = entry.RecoverAt
				}
				if entry.Reason == ReasonQuotaZero {
					reason = ReasonQuotaZero
				}
			}
			e.openLocked(entry, reason, observation.ObservedAt, recoverAt, recoverAt)
			entry.FailureCount++
			entry.LastFailureAt = observation.ObservedAt
			entry.LastEvidenceAt = observation.ObservedAt
			entry.LastEvidenceSource = evidenceSourceUsage429
		} else {
			e.observeGeneric429Locked(entry, observation.ObservedAt, recoverAt, hasReset)
		}
	}

	changed := !entryEqual(before, *entry)
	if changed {
		e.markDirtyLocked(observation.ObservedAt)
	}
	return ObservationResult{
		Handled: true,
		Changed: changed,
		Group:   group,
		State:   entry.State,
		Reason:  entry.Reason,
	}
}

func (e *Engine) lookupEntryLocked(authIndex, authID string, group ModelGroup) *Entry {
	authIndex = strings.TrimSpace(authIndex)
	authID = strings.TrimSpace(authID)
	if authIndex == "" && authID != "" {
		authIndex = e.authByID[authID]
	}
	entry := e.entries[entryKey(authIndex, group)]
	if entry == nil || (authID != "" && entry.AuthID != authID) {
		return nil
	}
	return entry
}

func (e *Engine) observeGeneric429Locked(entry *Entry, observedAt, resetAt time.Time, hasReset bool) {
	wasOpen := entry.State == StateOpen || entry.State == StateHalfOpen
	if entry.LastFailureAt.IsZero() || observedAt.Sub(entry.LastFailureAt) > e.config.Generic429Window || observedAt.Before(entry.LastFailureAt) {
		entry.FailureCount = 1
	} else {
		entry.FailureCount++
	}
	entry.LastFailureAt = observedAt
	entry.LastEvidenceAt = observedAt
	entry.LastEvidenceSource = evidenceSourceUsage429
	// A generic rate-limit response is weaker evidence than a still-active hard
	// quota/reset breaker. Record it, but never shorten or replace that breaker.
	if entry.State == StateOpen && observedAt.Before(entry.RecoverAt) &&
		(entry.Reason == ReasonQuotaZero || entry.Reason == ReasonExplicit429) {
		return
	}
	if entry.State == StateHalfOpen {
		e.metrics.HalfOpenFailureTotal++
	}
	if entry.FailureCount < e.config.Generic429Threshold && !wasOpen {
		return
	}
	cooldown := e.config.Generic429InitialCooldown
	if wasOpen || entry.FailureCount > e.config.Generic429Threshold {
		cooldown = e.config.Generic429MaxCooldown
	}
	recoverAt := observedAt.Add(cooldown)
	if hasReset && resetAt.After(recoverAt) {
		recoverAt = resetAt
	}
	e.openLocked(entry, ReasonGeneric429, observedAt, recoverAt, resetAt)
}

func (e *Engine) openLocked(entry *Entry, reason Reason, openedAt, recoverAt, resetAt time.Time) {
	wasCooling := entry.State == StateOpen || entry.State == StateHalfOpen
	entry.State = StateOpen
	entry.Reason = reason
	entry.OpenedAt = openedAt.UTC()
	entry.RecoverAt = recoverAt.UTC()
	entry.ResetAt = resetAt.UTC()
	entry.HalfOpenStartedAt = time.Time{}
	entry.HalfOpenLeaseUntil = time.Time{}
	entry.halfOpenRequestID = ""
	if !wasCooling {
		e.metrics.CooldownOpenedTotal++
	}
}

func (e *Engine) closeLocked(entry *Entry) {
	entry.State = StateClosed
	entry.Reason = ReasonNone
	entry.OpenedAt = time.Time{}
	entry.RecoverAt = time.Time{}
	entry.ResetAt = time.Time{}
	entry.FailureCount = 0
	entry.LastFailureAt = time.Time{}
	entry.HalfOpenStartedAt = time.Time{}
	entry.HalfOpenLeaseUntil = time.Time{}
	entry.halfOpenRequestID = ""
}

func explicitQuotaExhaustion(body string) bool {
	text := strings.ToLower(inspectedBody(body))
	if text == "" {
		return false
	}
	hasQuota := strings.Contains(text, "quota") || strings.Contains(text, "usage limit") || strings.Contains(text, "usage_limit")
	hasExhaustion := strings.Contains(text, "exhaust") || strings.Contains(text, "exceed") ||
		strings.Contains(text, "deplet") || strings.Contains(text, "no remaining") ||
		strings.Contains(text, "limit reached") || strings.Contains(text, "limit_reached") ||
		strings.Contains(text, "insufficient")
	return hasQuota && hasExhaustion
}

func inspectedBody(body string) string {
	if len(body) > maxInspectedFailureBody {
		return body[:maxInspectedFailureBody]
	}
	return body
}

func extractResetAt(headers http.Header, body string, now time.Time) (time.Time, bool) {
	for _, key := range []string{"Retry-After", "X-RateLimit-Reset", "RateLimit-Reset", "X-RateLimit-Reset-After"} {
		for _, value := range headers.Values(key) {
			if parsed, ok := parseResetValue(value, key, now); ok && parsed.After(now) {
				return parsed.UTC(), true
			}
		}
	}
	var value any
	if err := json.Unmarshal([]byte(inspectedBody(body)), &value); err == nil {
		if reset, ok := findResetValue(value, now); ok {
			return reset.UTC(), true
		}
	}
	return time.Time{}, false
}

func findResetValue(value any, now time.Time) (time.Time, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
			if strings.Contains(normalized, "reset") || strings.Contains(normalized, "retry_after") || strings.Contains(normalized, "retrydelay") {
				if parsed, ok := parseResetValueAny(child, normalized, now); ok && parsed.After(now) {
					return parsed, true
				}
			}
		}
		for _, child := range typed {
			if parsed, ok := findResetValue(child, now); ok {
				return parsed, true
			}
		}
	case []any:
		for _, child := range typed {
			if parsed, ok := findResetValue(child, now); ok {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

func parseResetValueAny(value any, key string, now time.Time) (time.Time, bool) {
	switch typed := value.(type) {
	case string:
		return parseResetValue(typed, key, now)
	case float64:
		return parseResetNumber(typed, key, now)
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return time.Time{}, false
		}
		return parseResetNumber(number, key, now)
	default:
		return time.Time{}, false
	}
}

func parseResetValue(value, key string, now time.Time) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, true
	}
	if parsed, err := http.ParseTime(value); err == nil {
		return parsed, true
	}
	if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
		return now.Add(duration), true
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return time.Time{}, false
	}
	return parseResetNumber(number, strings.ToLower(key), now)
}

func parseResetNumber(number float64, key string, now time.Time) (time.Time, bool) {
	if math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
		return time.Time{}, false
	}
	lowerKey := strings.ToLower(key)
	if strings.Contains(lowerKey, "after") || strings.Contains(lowerKey, "delay") || number < 100000000 {
		return now.Add(time.Duration(number * float64(time.Second))), true
	}
	if number > 100000000000 {
		number /= 1000
	}
	return time.Unix(int64(number), 0), true
}
