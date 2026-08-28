package guard

import (
	"testing"
	"time"
)

func TestPickFiltersOnlyMatchingGroupAndRoundRobins(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.ReplaceRoster([]Identity{testIdentity("a", 100), testIdentity("b", 100)}); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"a", "b"} {
		applyPositive(t, engine, index, ModelGroupGemini, now)
		applyPositive(t, engine, index, ModelGroupClaudeGPT, now)
	}
	_, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex: "a", AuthID: "id-a", Group: ModelGroupGemini,
		Remaining: 0, ResetAt: now.Add(time.Hour), ObservedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	candidates := []Candidate{
		{AuthID: "id-a", Provider: "antigravity", Priority: 100},
		{AuthID: "id-b", Provider: "antigravity", Priority: 100},
	}
	gemini := engine.Pick(PickRequest{
		Now: now.Add(2 * time.Minute), Provider: "antigravity", Model: "gemini-2.5-pro",
		Candidates: candidates, Mode: ModeEnforce,
	})
	if gemini.SelectedAuthID != "id-b" || gemini.Excluded != 1 {
		t.Fatalf("gemini pick = %#v", gemini)
	}
	lastSelection := engine.Snapshot(now.Add(2 * time.Minute)).LastSelection
	if lastSelection == nil || lastSelection.SelectedAuthID != "id-b" || lastSelection.ModelGroup != ModelGroupGemini || lastSelection.Excluded != 1 || !lastSelection.Handled {
		t.Fatalf("last selection = %#v, want enforced Gemini selection of id-b", lastSelection)
	}
	firstClaude := engine.Pick(PickRequest{
		Now: now.Add(2 * time.Minute), Provider: "antigravity", Model: "claude-opus",
		Candidates: candidates, Mode: ModeEnforce,
	})
	secondClaude := engine.Pick(PickRequest{
		Now: now.Add(2 * time.Minute), Provider: "antigravity", Model: "claude-opus",
		Candidates: candidates, Mode: ModeEnforce,
	})
	if firstClaude.SelectedAuthID == "" || secondClaude.SelectedAuthID == "" || firstClaude.SelectedAuthID == secondClaude.SelectedAuthID {
		t.Fatalf("round robin did not rotate: first=%#v second=%#v", firstClaude, secondClaude)
	}
}

func TestPickExcludesStaleEvidenceButUsesFreshCandidate(t *testing.T) {
	t.Parallel()
	old := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.ReplaceRoster([]Identity{testIdentity("a", 100), testIdentity("b", 100)}); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"a", "b"} {
		applyPositive(t, engine, index, ModelGroupGemini, old)
		applyPositive(t, engine, index, ModelGroupClaudeGPT, old)
	}
	applyPositive(t, engine, "b", ModelGroupGemini, now)
	applyPositive(t, engine, "b", ModelGroupClaudeGPT, now)
	pick := engine.Pick(PickRequest{
		Now: now, Provider: "antigravity", Model: "gemini-2.5-pro", Mode: ModeEnforce,
		Candidates: []Candidate{
			{AuthID: "id-a", Provider: "antigravity", Priority: 100},
			{AuthID: "id-b", Provider: "antigravity", Priority: 100},
		},
	})
	if pick.SelectedAuthID != "id-b" || pick.Excluded != 1 || !pick.Handled {
		t.Fatalf("stale candidate was not excluded: %#v", pick)
	}
}

func TestMixedProviderRetainsFallbackAndUnknownModelFailsClosed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("a", 100)); err != nil {
		t.Fatal(err)
	}
	applyPositive(t, engine, "a", ModelGroupGemini, now)
	_, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex: "a", AuthID: "id-a", Group: ModelGroupGemini,
		Remaining: 0, ResetAt: now.Add(time.Hour), ObservedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	candidates := []Candidate{
		{AuthID: "id-a", Provider: "antigravity"},
		{AuthID: "anthropic-a", Provider: "anthropic"},
	}
	mixed := engine.Pick(PickRequest{
		Now: now.Add(2 * time.Minute), Provider: "router", Providers: []string{"antigravity", "anthropic"},
		Model: "gemini-pro", Candidates: candidates, Mode: ModeEnforce,
	})
	if mixed.SelectedAuthID != "anthropic-a" || !mixed.Handled || mixed.AllCooling {
		t.Fatalf("mixed fallback = %#v", mixed)
	}
	unknown := engine.Pick(PickRequest{
		Now: now, Provider: "antigravity", Model: "future-model", Mode: ModeEnforce,
		Candidates: []Candidate{{AuthID: "id-a", Provider: "antigravity"}},
	})
	if !unknown.Handled || unknown.ErrorCode != ErrorCodeQuotaGroupUnknown || unknown.SelectedAuthID != "" {
		t.Fatalf("unknown model = %#v", unknown)
	}
	nonAntigravity := engine.Pick(PickRequest{
		Now: now, Provider: "anthropic", Model: "claude-opus", Mode: ModeEnforce,
		Candidates: []Candidate{{AuthID: "anthropic-a", Provider: "anthropic"}},
	})
	if nonAntigravity.Handled || nonAntigravity.SelectedAuthID != "" {
		t.Fatalf("non-Antigravity route was captured: %#v", nonAntigravity)
	}
}

func TestUnknownAuthFailsClosedByDefaultAndCanBeExplicitlyOpened(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	request := PickRequest{
		Now: now, Provider: "antigravity", Model: "gemini-pro", Mode: ModeEnforce,
		Candidates: []Candidate{{AuthID: "not-registered", Provider: "antigravity"}},
	}
	closed := engine.Pick(request)
	if !closed.Handled || closed.ErrorCode != ErrorCodeAuthUnknown || closed.SelectedAuthID != "" {
		t.Fatalf("default unknown auth behavior = %#v", closed)
	}
	request.UnknownAuthPolicy = PolicyFailOpen
	opened := engine.Pick(request)
	if !opened.Handled || opened.SelectedAuthID != "not-registered" {
		t.Fatalf("explicit fail-open behavior = %#v", opened)
	}
}

func TestManualHalfOpenIsConsumedByNextPick(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("a", 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ApplyQuotaEvidence(QuotaEvidence{
		AuthIndex: "a", AuthID: "id-a", Group: ModelGroupGemini,
		Remaining: 0, ResetAt: now.Add(time.Hour), ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	readyAt, err := engine.ManualHalfOpen("a", ModelGroupGemini, now.Add(time.Minute))
	if err != nil || !readyAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("manual half-open failed: ready=%s err=%v", readyAt, err)
	}
	pending := entryFromSnapshot(t, engine, "a", ModelGroupGemini, readyAt)
	if pending.State != StateHalfOpen || !pending.HalfOpenLeaseUntil.IsZero() || !pending.HalfOpenStartedAt.IsZero() {
		t.Fatalf("manual half-open was not left pending: %#v", pending)
	}
	pick := engine.Pick(PickRequest{
		Now: readyAt, Provider: "antigravity", Model: "gemini-pro", Mode: ModeEnforce,
		Candidates: []Candidate{{AuthID: "id-a", Provider: "antigravity"}},
	})
	if pick.SelectedAuthID != "id-a" {
		t.Fatalf("pending manual probe was not selected: %#v", pick)
	}
	leased := entryFromSnapshot(t, engine, "a", ModelGroupGemini, readyAt)
	if !leased.HalfOpenStartedAt.Equal(readyAt) || !leased.HalfOpenLeaseUntil.Equal(readyAt.Add(30*time.Second)) {
		t.Fatalf("manual probe lease was not consumed atomically: %#v", leased)
	}
}
