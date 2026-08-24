package guard

import (
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func testIdentity(index string, priority int) Identity {
	return Identity{
		AuthID:              "id-" + index,
		AuthIndex:           index,
		IdentityFingerprint: "fingerprint-" + index,
		Provider:            "antigravity",
		Priority:            priority,
	}
}

func entryFromSnapshot(t *testing.T, engine *Engine, index string, group ModelGroup, now time.Time) Entry {
	t.Helper()
	for _, entry := range engine.Snapshot(now).Entries {
		if entry.AuthIndex == index && entry.ModelGroup == group {
			return entry
		}
	}
	t.Fatalf("entry %s/%s not found", index, group)
	return Entry{}
}

func applyPositive(t *testing.T, engine *Engine, index string, group ModelGroup, now time.Time) {
	t.Helper()
	result, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex:  index,
		AuthID:     "id-" + index,
		Group:      group,
		Remaining:  80,
		ResetAt:    now.Add(time.Hour),
		ObservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.State != StateClosed {
		t.Fatalf("positive evidence result = %#v", result)
	}
}

func TestClassifyModelAndProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model string
		group ModelGroup
		ok    bool
	}{
		{"gemini-2.5-pro", ModelGroupGemini, true},
		{"Claude-Opus-4.1", ModelGroupClaudeGPT, true},
		{"gpt-5.2-codex", ModelGroupClaudeGPT, true},
		{"openai/gpt-5", ModelGroupClaudeGPT, true},
		{"gemini-claude-bridge", "", false},
		{"other-model", "", false},
	}
	for _, test := range tests {
		group, ok := ClassifyModel(test.model)
		if group != test.group || ok != test.ok {
			t.Errorf("ClassifyModel(%q) = %q,%v; want %q,%v", test.model, group, ok, test.group, test.ok)
		}
	}
	if !IsAntigravityProvider("Google_Antigravity") || IsAntigravityProvider("google") || IsAntigravityProvider("gemini") {
		t.Fatal("provider classification captured the wrong executor")
	}
}

func TestRosterEvidenceIsModelGroupIndependentAndIdentityFenced(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.ReplaceRoster([]Identity{testIdentity("auth-1", 100)}); err != nil {
		t.Fatal(err)
	}
	if got := len(engine.Snapshot(now).Entries); got != 2 {
		t.Fatalf("entries = %d, want 2", got)
	}
	applyPositive(t, engine, "auth-1", ModelGroupClaudeGPT, now)
	zero, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex: "auth-1", AuthID: "id-auth-1", Group: ModelGroupGemini,
		Remaining: 0, ResetAt: now.Add(5 * time.Hour), ObservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if zero.State != StateOpen || zero.Reason != ReasonQuotaZero {
		t.Fatalf("zero evidence = %#v", zero)
	}
	gemini := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, now)
	claude := entryFromSnapshot(t, engine, "auth-1", ModelGroupClaudeGPT, now)
	if gemini.State != StateOpen || claude.State != StateClosed {
		t.Fatalf("groups interfered: gemini=%s claude=%s", gemini.State, claude.State)
	}

	replacement := testIdentity("auth-1", 100)
	replacement.AuthID = "replacement-id"
	replacement.IdentityFingerprint = "replacement-fingerprint"
	if err := engine.ReplaceRoster([]Identity{replacement}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range engine.Snapshot(now).Entries {
		if entry.State != StateUninitialized || entry.AuthID != "replacement-id" || entry.RemainingPercent != nil {
			t.Fatalf("replacement inherited prior state: %#v", entry)
		}
	}
}

func TestRegisterIdentityRejectsDuplicateAuthIDAcrossIndexes(t *testing.T) {
	t.Parallel()
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	duplicate := testIdentity("auth-2", 100)
	duplicate.AuthID = "id-auth-1"
	if err := engine.RegisterIdentity(duplicate); err == nil {
		t.Fatal("duplicate AuthID was accepted for a second auth index")
	}
	if got := len(engine.Snapshot(time.Now()).Entries); got != 2 {
		t.Fatalf("failed registration mutated roster: entries=%d", got)
	}
}

func TestQuotaProbeFailureAndStaleEvidenceDoNotMutateAvailability(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	applyPositive(t, engine, "auth-1", ModelGroupGemini, now)
	before := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, now)
	engine.RecordQuotaProbeError()
	result, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex: "auth-1", AuthID: "id-auth-1", Group: ModelGroupGemini,
		Remaining: 0, ResetAt: now.Add(2 * time.Hour), ObservedAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	after := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, now)
	if !result.Ignored || !entryEqual(before, after) {
		t.Fatalf("stale probe changed state: before=%#v after=%#v result=%#v", before, after, result)
	}
	if metrics := engine.Metrics(now); metrics.QuotaProbeErrorTotal != 1 {
		t.Fatalf("quota probe errors = %d", metrics.QuotaProbeErrorTotal)
	}
}

func TestGeneric429ThresholdCooldownAndHalfOpen(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	applyPositive(t, engine, "auth-1", ModelGroupGemini, now)
	observation := UsageObservation{
		Provider: "antigravity", Model: "gemini-2.5-pro", AuthID: "id-auth-1", AuthIndex: "auth-1",
		Failed: true, StatusCode: http.StatusTooManyRequests, Body: `{"error":"temporarily rate limited"}`,
		ObservedAt: now.Add(time.Second),
	}
	first := engine.ObserveUsage(observation)
	if first.State != StateClosed || first.Reason != ReasonNone {
		t.Fatalf("first generic 429 opened breaker: %#v", first)
	}
	observation.ObservedAt = now.Add(30 * time.Second)
	second := engine.ObserveUsage(observation)
	if second.State != StateOpen || second.Reason != ReasonGeneric429 {
		t.Fatalf("second generic 429 did not open: %#v", second)
	}
	entry := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, now)
	if want := observation.ObservedAt.Add(15 * time.Minute); !entry.RecoverAt.Equal(want) {
		t.Fatalf("recover_at = %s, want %s", entry.RecoverAt, want)
	}

	beforeReset := engine.Pick(PickRequest{
		Now: observation.ObservedAt.Add(14 * time.Minute), Provider: "antigravity", Model: "gemini-2.5-pro",
		Candidates: []Candidate{{AuthID: "id-auth-1", Provider: "antigravity"}}, Mode: ModeEnforce,
	})
	if !beforeReset.Handled || !beforeReset.AllCooling || beforeReset.ErrorCode != ErrorCodeModelCooldown || beforeReset.RetryAfter != time.Minute {
		t.Fatalf("pre-reset pick = %#v", beforeReset)
	}
	afterResetAt := entry.RecoverAt
	halfOpen := engine.Pick(PickRequest{
		Now: afterResetAt, RequestID: "half-open-request", Provider: "antigravity", Model: "gemini-2.5-pro",
		Candidates: []Candidate{{AuthID: "id-auth-1", Provider: "antigravity"}}, Mode: ModeEnforce,
	})
	if !halfOpen.Handled || halfOpen.SelectedAuthID != "id-auth-1" {
		t.Fatalf("half-open pick = %#v", halfOpen)
	}
	blockedLease := engine.Pick(PickRequest{
		Now: afterResetAt.Add(time.Second), Provider: "antigravity", Model: "gemini-2.5-pro",
		Candidates: []Candidate{{AuthID: "id-auth-1", Provider: "antigravity"}}, Mode: ModeEnforce,
	})
	if !blockedLease.AllCooling || blockedLease.RetryAfter != 29*time.Second {
		t.Fatalf("concurrent half-open was not fenced: %#v", blockedLease)
	}
	success := engine.ObserveUsage(UsageObservation{
		RequestID: "half-open-request",
		Provider:  "antigravity", Model: "gemini-2.5-pro", AuthID: "id-auth-1",
		Failed: false, RequestedAt: afterResetAt.Add(-5 * time.Millisecond), ObservedAt: afterResetAt.Add(2 * time.Second),
	})
	if success.State != StateClosed {
		t.Fatalf("half-open success = %#v", success)
	}
}

func TestOutOfOrderUsageCannotOverrideFreshQuotaEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	applyPositive(t, engine, "auth-1", ModelGroupGemini, now)
	before := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, now)
	result := engine.ObserveUsage(UsageObservation{
		Provider: "antigravity", Model: "gemini-pro", AuthID: "id-auth-1",
		Failed: true, StatusCode: http.StatusTooManyRequests, Body: `{"error":"quota exhausted"}`,
		RequestedAt: now.Add(-2 * time.Minute), ObservedAt: now.Add(-time.Minute),
	})
	after := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, now)
	if result.Changed || !entryEqual(before, after) {
		t.Fatalf("older usage overrode fresh evidence: result=%#v before=%#v after=%#v", result, before, after)
	}
}

func TestQuotaEvidenceIdentityFingerprintFencesReplacedCredential(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	identity := testIdentity("auth-1", 100)
	if err := engine.RegisterIdentity(identity); err != nil {
		t.Fatal(err)
	}
	result, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex:           identity.AuthIndex,
		AuthID:              identity.AuthID,
		IdentityFingerprint: "replaced-account-fingerprint",
		Group:               ModelGroupGemini,
		Remaining:           100,
		ResetAt:             now.Add(time.Hour),
		ObservedAt:          now,
	})
	if err != nil {
		t.Fatal(err)
	}
	entry := entryFromSnapshot(t, engine, identity.AuthIndex, ModelGroupGemini, now)
	if !result.Ignored || entry.State != StateUninitialized || entry.RemainingPercent != nil {
		t.Fatalf("replaced identity accepted old quota: result=%#v entry=%#v", result, entry)
	}
}

func TestPreBreakerSuccessCannotCloseNewHalfOpenLease(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex: "auth-1", AuthID: "id-auth-1", Group: ModelGroupGemini,
		Remaining: 0, ResetAt: now.Add(time.Minute), ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	leaseAt := now.Add(time.Minute)
	pick := engine.Pick(PickRequest{
		Now: leaseAt, RequestID: "half-open-request", Provider: "antigravity", Model: "gemini-pro", Mode: ModeEnforce,
		Candidates: []Candidate{{AuthID: "id-auth-1", Provider: "antigravity"}},
	})
	if pick.SelectedAuthID == "" {
		t.Fatalf("half-open lease was not acquired: %#v", pick)
	}
	result := engine.ObserveUsage(UsageObservation{
		RequestID: "older-request",
		Provider:  "antigravity", Model: "gemini-pro", AuthID: "id-auth-1",
		Failed: false, RequestedAt: now.Add(-time.Second), ObservedAt: leaseAt.Add(time.Second),
	})
	entry := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, leaseAt.Add(time.Second))
	if result.State != StateHalfOpen || entry.State != StateHalfOpen {
		t.Fatalf("pre-breaker success closed half-open state: result=%#v entry=%#v", result, entry)
	}
}

func TestHalfOpenFailureEscalatesToMaximumCooldown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	applyPositive(t, engine, "auth-1", ModelGroupGemini, now)
	for offset := 1; offset <= 2; offset++ {
		engine.ObserveUsage(UsageObservation{
			Provider: "antigravity", Model: "gemini-pro", AuthID: "id-auth-1",
			Failed: true, StatusCode: 429, Body: "rate limited", ObservedAt: now.Add(time.Duration(offset) * time.Second),
		})
	}
	opened := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, now)
	engine.Pick(PickRequest{
		Now: opened.RecoverAt, RequestID: "half-open-request", Provider: "antigravity", Model: "gemini-pro", Mode: ModeEnforce,
		Candidates: []Candidate{{AuthID: "id-auth-1", Provider: "antigravity"}},
	})
	failureAt := opened.RecoverAt.Add(time.Second)
	result := engine.ObserveUsage(UsageObservation{
		RequestID: "half-open-request",
		Provider:  "antigravity", Model: "gemini-pro", AuthID: "id-auth-1",
		Failed: true, StatusCode: 429, Body: "rate limited", ObservedAt: failureAt,
	})
	entry := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, failureAt)
	if result.State != StateOpen || !entry.RecoverAt.Equal(failureAt.Add(30*time.Minute)) {
		t.Fatalf("half-open failure did not escalate: result=%#v entry=%#v", result, entry)
	}
	if engine.Metrics(failureAt).HalfOpenFailureTotal != 1 {
		t.Fatal("half-open failure metric not incremented")
	}
}

func TestGeneric429CannotShortenHardQuotaCooldown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	hardReset := now.Add(5 * time.Hour)
	if _, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex: "auth-1", AuthID: "id-auth-1", Group: ModelGroupGemini,
		Remaining: 0, ResetAt: hardReset, ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for offset := 1; offset <= 2; offset++ {
		engine.ObserveUsage(UsageObservation{
			Provider: "antigravity", Model: "gemini-pro", AuthID: "id-auth-1",
			Failed: true, StatusCode: 429, Body: "rate limited", ObservedAt: now.Add(time.Duration(offset) * time.Second),
		})
	}
	entry := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, now)
	if entry.Reason != ReasonQuotaZero || !entry.RecoverAt.Equal(hardReset) {
		t.Fatalf("generic 429 weakened hard cooldown: %#v", entry)
	}
}

func TestExplicitQuota429UsesTrustedResetAndDoesNotPersistBody(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	applyPositive(t, engine, "auth-1", ModelGroupClaudeGPT, now)
	reset := now.Add(2 * time.Hour)
	result := engine.ObserveUsage(UsageObservation{
		Provider: "antigravity", Model: "claude-opus", AuthID: "id-auth-1",
		Failed: true, StatusCode: 429,
		Body:       `{"error":"weekly quota exhausted","access_token":"must-not-persist"}`,
		Headers:    http.Header{"X-Ratelimit-Reset": []string{reset.Format(time.RFC3339)}},
		ObservedAt: now.Add(time.Minute),
	})
	entry := entryFromSnapshot(t, engine, "auth-1", ModelGroupClaudeGPT, now)
	if result.Reason != ReasonExplicit429 || !entry.RecoverAt.Equal(reset) {
		t.Fatalf("explicit 429 = %#v entry=%#v", result, entry)
	}
}

func TestObserveModeDoesNotConsumeHalfOpenLease(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	_, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex: "auth-1", AuthID: "id-auth-1", Group: ModelGroupGemini,
		Remaining: 0, ResetAt: now.Add(time.Hour), ObservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	pick := engine.Pick(PickRequest{
		Now: now.Add(time.Hour), Provider: "antigravity", Model: "gemini-pro", Mode: ModeObserve,
		Candidates: []Candidate{{AuthID: "id-auth-1", Provider: "antigravity"}},
	})
	entry := entryFromSnapshot(t, engine, "auth-1", ModelGroupGemini, now)
	if pick.Handled || !pick.ObserveOnly || pick.SelectedAuthID == "" || entry.State != StateOpen || !entry.HalfOpenLeaseUntil.IsZero() {
		t.Fatalf("observe mode mutated scheduling state: pick=%#v entry=%#v", pick, entry)
	}
}

func TestConcurrentHalfOpenOnlyOneWinner(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	_, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex: "auth-1", AuthID: "id-auth-1", Group: ModelGroupGemini,
		Remaining: 0, ResetAt: now.Add(time.Hour), ObservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := PickRequest{
		Now: now.Add(time.Hour), Provider: "antigravity", Model: "gemini-pro", Mode: ModeEnforce,
		Candidates: []Candidate{{AuthID: "id-auth-1", Provider: "antigravity"}},
	}
	const workers = 32
	var wait sync.WaitGroup
	winners := make(chan bool, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			winners <- engine.Pick(request).SelectedAuthID != ""
		}()
	}
	wait.Wait()
	close(winners)
	count := 0
	for winner := range winners {
		if winner {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("half-open winners = %d, want 1", count)
	}
}

func TestConcurrentObserveSnapshotAndPersistenceAreRaceSafe(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	applyPositive(t, engine, "auth-1", ModelGroupGemini, now)
	path := filepath.Join(t.TempDir(), "state.json")
	const iterations = 50
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 1)
	wait.Add(4)
	go func() {
		defer wait.Done()
		for offset := range iterations {
			engine.ObserveUsage(UsageObservation{
				Provider: "antigravity", Model: "gemini-pro", AuthID: "id-auth-1",
				Failed: false, ObservedAt: now.Add(time.Duration(offset+1) * time.Second),
			})
		}
	}()
	go func() {
		defer wait.Done()
		for offset := range iterations {
			engine.Pick(PickRequest{
				Now: now.Add(time.Duration(offset+1) * time.Second), Provider: "antigravity",
				Model: "gemini-pro", Mode: ModeObserve,
				Candidates: []Candidate{{AuthID: "id-auth-1", Provider: "antigravity"}},
			})
		}
	}()
	go func() {
		defer wait.Done()
		for offset := range iterations {
			_ = engine.Snapshot(now.Add(time.Duration(offset) * time.Second))
			_ = engine.Metrics(now.Add(time.Duration(offset) * time.Second))
		}
	}()
	go func() {
		defer wait.Done()
		for range iterations {
			if err := engine.Save(path); err != nil {
				select {
				case errorsSeen <- err:
				default:
				}
				return
			}
		}
	}()
	wait.Wait()
	select {
	case err := <-errorsSeen:
		t.Fatalf("concurrent persistence failed: %v", err)
	default:
	}
}
