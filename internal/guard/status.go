package guard

import (
	"fmt"
	"sort"
	"time"
)

type Readiness struct {
	Ready           bool     `json:"ready"`
	UniformPriority bool     `json:"uniform_priority"`
	Priority        int      `json:"priority,omitempty"`
	Reasons         []string `json:"reasons,omitempty"`
}

// EvidenceFresh reports whether an entry has recent quota-probe evidence.
// Breaker state and quota evidence have different lifecycles: a failed probe
// must not erase a prior breaker decision, but stale evidence must not make a
// closed account eligible for a new request.
func EvidenceFresh(entry Entry, now time.Time, maxAge time.Duration) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if maxAge <= 0 {
		return false
	}
	observedAt := entry.LastQuotaEvidenceAt
	return !observedAt.IsZero() && now.Sub(observedAt) <= maxAge && !observedAt.After(now.Add(time.Minute))
}

// Ready verifies the breaker-domain preconditions for enforcement. Host-level
// checks such as being the only active Scheduler remain the runtime's concern.
func (e *Engine) Ready(now time.Time, maxAge time.Duration) Readiness {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if maxAge <= 0 {
		maxAge = e.config.EvidenceMaxAge
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	result := Readiness{UniformPriority: true}
	if len(e.entries) == 0 {
		result.Reasons = append(result.Reasons, "empty_roster")
	}
	prioritySet := false
	groupsByIndex := make(map[string]map[ModelGroup]bool)
	freshByGroup := make(map[ModelGroup]bool, 2)
	uninitializedByGroup := make(map[ModelGroup]bool, 2)
	for _, entry := range e.entries {
		if groupsByIndex[entry.AuthIndex] == nil {
			groupsByIndex[entry.AuthIndex] = make(map[ModelGroup]bool, 2)
		}
		groupsByIndex[entry.AuthIndex][entry.ModelGroup] = true
		if !prioritySet {
			result.Priority = entry.Priority
			prioritySet = true
		} else if result.Priority != entry.Priority {
			result.UniformPriority = false
		}
		if entry.State == StateUninitialized {
			uninitializedByGroup[entry.ModelGroup] = true
		}
		if EvidenceFresh(*entry, now, maxAge) {
			freshByGroup[entry.ModelGroup] = true
		}
	}
	for _, group := range []ModelGroup{ModelGroupGemini, ModelGroupClaudeGPT} {
		if freshByGroup[group] {
			continue
		}
		if uninitializedByGroup[group] {
			result.Reasons = append(result.Reasons, fmt.Sprintf("uninitialized:%s", group))
		} else {
			result.Reasons = append(result.Reasons, fmt.Sprintf("stale_evidence:%s", group))
		}
	}
	for authIndex, groups := range groupsByIndex {
		for _, group := range []ModelGroup{ModelGroupGemini, ModelGroupClaudeGPT} {
			if !groups[group] {
				result.Reasons = append(result.Reasons, fmt.Sprintf("missing_group:%s:%s", authIndex, group))
			}
		}
	}
	if !result.UniformPriority {
		result.Reasons = append(result.Reasons, "non_uniform_priority")
	}
	sort.Strings(result.Reasons)
	result.Ready = len(result.Reasons) == 0
	return result
}

func (e *Engine) UniformPriority() (priority int, uniform bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	first := true
	for _, entry := range e.entries {
		if first {
			priority = entry.Priority
			first = false
			continue
		}
		if entry.Priority != priority {
			return 0, false
		}
	}
	return priority, !first
}

func (e *Engine) IsDirty() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dirty > 0
}

func (e *Engine) Generation() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.generation
}

func (e *Engine) Metrics(now time.Time) Metrics {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.metricsLocked(now)
}

func (e *Engine) metricsLocked(now time.Time) Metrics {
	metrics := e.metrics
	metrics.CooldownOpen = 0
	metrics.QuotaEvidenceStale = 0
	metrics.UsageEventDirtyBacklog = e.dirty
	for _, entry := range e.entries {
		if entry.State == StateOpen || entry.State == StateHalfOpen {
			metrics.CooldownOpen++
		}
		if !EvidenceFresh(*entry, now, e.config.EvidenceMaxAge) {
			metrics.QuotaEvidenceStale++
		}
	}
	return metrics
}

func (e *Engine) Snapshot(now time.Time) Snapshot {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot := Snapshot{
		GeneratedAt:     now,
		Entries:         sortedEntries(e.entries),
		EarliestRecover: make(map[ModelGroup]time.Time),
		Metrics:         e.metricsLocked(now),
	}
	if e.lastPick != nil {
		selection := *e.lastPick
		selection.Providers = append([]string(nil), e.lastPick.Providers...)
		snapshot.LastSelection = &selection
	}
	for _, entry := range e.entries {
		if entry.State != StateOpen && entry.State != StateHalfOpen {
			continue
		}
		boundary := entry.RecoverAt
		if entry.State == StateHalfOpen {
			boundary = entry.HalfOpenLeaseUntil
		}
		current := snapshot.EarliestRecover[entry.ModelGroup]
		snapshot.EarliestRecover[entry.ModelGroup] = earlierNonZero(current, boundary)
	}
	if len(snapshot.EarliestRecover) == 0 {
		snapshot.EarliestRecover = nil
	}
	return snapshot
}
