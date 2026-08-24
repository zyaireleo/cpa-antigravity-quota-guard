package guard

import (
	"fmt"
	"strings"
	"time"
)

type pickCandidate struct {
	Candidate
	entry      *Entry
	needsLease bool
}

// Pick applies the group-specific breaker to CPA scheduler candidates. In
// observe mode it returns the same projected details but Handled is false and
// no half-open lease or round-robin cursor is consumed.
func (e *Engine) Pick(request PickRequest) (result PickResult) {
	started := time.Now()
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	} else {
		request.Now = request.Now.UTC()
	}
	if request.Mode == "" {
		request.Mode = ModeObserve
	}
	if !routeIncludesAntigravity(request) {
		return PickResult{}
	}

	group, knownModel := ClassifyModel(request.Model)
	e.mu.Lock()
	defer e.mu.Unlock()
	defer func() {
		e.metrics.SchedulerLatencyMillis = float64(time.Since(started).Microseconds()) / 1000
		e.lastPick = &SelectionEvent{
			At:                request.Now,
			RequestID:         strings.TrimSpace(request.RequestID),
			Provider:          strings.TrimSpace(request.Provider),
			Providers:         append([]string(nil), request.Providers...),
			Model:             strings.TrimSpace(request.Model),
			ModelGroup:        result.Group,
			Mode:              request.Mode,
			SelectedAuthID:    result.SelectedAuthID,
			SelectedAuthIndex: result.SelectedAuthIndex,
			Handled:           result.Handled,
			ObserveOnly:       result.ObserveOnly,
			ErrorCode:         result.ErrorCode,
			Excluded:          result.Excluded,
		}
	}()
	e.metrics.SchedulerPickTotal++

	authPolicy := request.UnknownAuthPolicy
	if authPolicy == "" {
		authPolicy = e.config.UnknownAuthPolicy
	}
	modelPolicy := request.UnknownModelPolicy
	if modelPolicy == "" {
		modelPolicy = e.config.UnknownModelPolicy
	}
	if !knownModel {
		e.metrics.UnknownModelTotal++
		return e.pickUnknownModelLocked(request, modelPolicy)
	}
	if !groupEnforced(group, request.EnforcedGroups) {
		return PickResult{Group: group}
	}

	available := make([]pickCandidate, 0, len(request.Candidates))
	excluded := 0
	blockedCooling := 0
	blockedMissing := 0
	blockedUninitialized := 0
	var earliest time.Time
	for _, candidate := range request.Candidates {
		if !candidateIsAntigravity(request, candidate) {
			available = append(available, pickCandidate{Candidate: candidate})
			continue
		}
		entry := e.lookupEntryLocked(candidate.AuthIndex, candidate.AuthID, group)
		if entry == nil {
			e.metrics.UnknownAuthTotal++
			if authPolicy == PolicyFailOpen {
				available = append(available, pickCandidate{Candidate: candidate})
			} else {
				excluded++
				blockedMissing++
			}
			continue
		}
		switch entry.State {
		case StateClosed:
			available = append(available, pickCandidate{Candidate: normalizedCandidate(candidate, entry), entry: entry})
		case StateOpen:
			if !entry.RecoverAt.IsZero() && !request.Now.Before(entry.RecoverAt) {
				available = append(available, pickCandidate{Candidate: normalizedCandidate(candidate, entry), entry: entry, needsLease: true})
			} else {
				excluded++
				blockedCooling++
				earliest = earlierNonZero(earliest, entry.RecoverAt)
			}
		case StateHalfOpen:
			if entry.HalfOpenLeaseUntil.IsZero() || !request.Now.Before(entry.HalfOpenLeaseUntil) {
				available = append(available, pickCandidate{Candidate: normalizedCandidate(candidate, entry), entry: entry, needsLease: true})
			} else {
				excluded++
				blockedCooling++
				earliest = earlierNonZero(earliest, entry.HalfOpenLeaseUntil)
			}
		case StateUninitialized:
			if authPolicy == PolicyFailOpen {
				available = append(available, pickCandidate{Candidate: normalizedCandidate(candidate, entry), entry: entry})
			} else {
				excluded++
				blockedUninitialized++
			}
		default:
			excluded++
			blockedUninitialized++
		}
	}
	e.metrics.SchedulerExcludedTotal += uint64(excluded)
	result = PickResult{Group: group, Excluded: excluded, ObserveOnly: request.Mode == ModeObserve}
	if len(available) == 0 {
		result.AllCooling = blockedCooling > 0 && blockedMissing == 0 && blockedUninitialized == 0
		result.RetryAfter = retryAfter(request.Now, earliest)
		switch {
		case result.AllCooling:
			result.ErrorCode = ErrorCodeModelCooldown
		case blockedMissing > 0:
			result.ErrorCode = ErrorCodeAuthUnknown
		case blockedUninitialized > 0:
			result.ErrorCode = ErrorCodeAuthUninitialized
		default:
			result.ErrorCode = ErrorCodeNoEligibleAuth
		}
		if request.Mode == ModeEnforce {
			result.Handled = true
			e.metrics.FailClosedTotal++
		}
		return result
	}

	cursor := e.cursors[group]
	selected := available[int(cursor%uint64(len(available)))]
	result.SelectedAuthID = selected.AuthID
	result.SelectedAuthIndex = selected.AuthIndex
	if request.Mode != ModeEnforce {
		return result
	}
	result.Handled = true
	e.cursors[group] = cursor + 1
	if selected.needsLease && selected.entry != nil {
		selected.entry.State = StateHalfOpen
		selected.entry.HalfOpenStartedAt = request.Now
		selected.entry.HalfOpenLeaseUntil = request.Now.Add(e.config.HalfOpenLease)
		selected.entry.halfOpenRequestID = strings.TrimSpace(request.RequestID)
		e.metrics.HalfOpenAttemptTotal++
		e.markDirtyLocked(request.Now)
	}
	return result
}

func (e *Engine) pickUnknownModelLocked(request PickRequest, policy Policy) PickResult {
	result := PickResult{
		ObserveOnly: request.Mode == ModeObserve,
		ErrorCode:   ErrorCodeQuotaGroupUnknown,
	}
	if policy == PolicyFailOpen {
		return result
	}
	// In a mixed route, an unknown model must not be allowed to reach an
	// Antigravity credential, but a non-Antigravity provider remains eligible.
	nonAntigravity := make([]Candidate, 0, len(request.Candidates))
	for _, candidate := range request.Candidates {
		if !candidateIsAntigravity(request, candidate) {
			nonAntigravity = append(nonAntigravity, candidate)
		} else {
			result.Excluded++
		}
	}
	e.metrics.SchedulerExcludedTotal += uint64(result.Excluded)
	if len(nonAntigravity) > 0 {
		selected := nonAntigravity[0]
		result.SelectedAuthID = selected.AuthID
		result.SelectedAuthIndex = selected.AuthIndex
		result.ErrorCode = ""
		if request.Mode == ModeEnforce {
			result.Handled = true
		}
		return result
	}
	if request.Mode == ModeEnforce {
		result.Handled = true
		e.metrics.FailClosedTotal++
	}
	return result
}

func candidateIsAntigravity(request PickRequest, candidate Candidate) bool {
	if candidate.Provider != "" {
		return IsAntigravityProvider(candidate.Provider)
	}
	// CPA normally includes Candidate.Provider. For an older host that omits it,
	// only infer Antigravity when the primary route is unambiguous.
	if !IsAntigravityProvider(request.Provider) {
		return false
	}
	for _, provider := range request.Providers {
		if strings.TrimSpace(provider) != "" && !IsAntigravityProvider(provider) {
			return false
		}
	}
	return true
}

func normalizedCandidate(candidate Candidate, entry *Entry) Candidate {
	if candidate.AuthID == "" {
		candidate.AuthID = entry.AuthID
	}
	if candidate.AuthIndex == "" {
		candidate.AuthIndex = entry.AuthIndex
	}
	return candidate
}

func earlierNonZero(current, candidate time.Time) time.Time {
	if candidate.IsZero() {
		return current
	}
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func retryAfter(now, recoverAt time.Time) time.Duration {
	if recoverAt.IsZero() || !recoverAt.After(now) {
		return 0
	}
	remaining := recoverAt.Sub(now)
	// Retry-After is expressed as whole seconds. Round up so a caller never
	// retries before the actual lease or recovery boundary.
	return ((remaining + time.Second - 1) / time.Second) * time.Second
}

// ManualHalfOpen marks an entry ready for one audited probe. The next
// Scheduler.Pick atomically consumes the pending state and starts the lease.
func (e *Engine) ManualHalfOpen(authIndex string, group ModelGroup, now time.Time) (time.Time, error) {
	if strings.TrimSpace(authIndex) == "" || !group.Valid() {
		return time.Time{}, fmt.Errorf("%w: invalid half-open target", ErrInvalidIdentity)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := e.entries[entryKey(strings.TrimSpace(authIndex), group)]
	if entry == nil {
		return time.Time{}, ErrInvalidIdentity
	}
	if entry.State == StateHalfOpen {
		if entry.HalfOpenLeaseUntil.IsZero() {
			return time.Time{}, fmt.Errorf("guard: half-open probe is already pending")
		}
		if now.Before(entry.HalfOpenLeaseUntil) {
			return time.Time{}, fmt.Errorf("guard: half-open lease already active until %s", entry.HalfOpenLeaseUntil.Format(time.RFC3339))
		}
	}
	if entry.State != StateOpen && entry.State != StateHalfOpen {
		return time.Time{}, fmt.Errorf("guard: half-open requires an open breaker")
	}
	entry.State = StateHalfOpen
	entry.Reason = ReasonManualProbe
	entry.HalfOpenStartedAt = time.Time{}
	entry.HalfOpenLeaseUntil = time.Time{}
	entry.halfOpenRequestID = ""
	e.markDirtyLocked(now)
	return now, nil
}
