package runtime

import (
	"testing"
	"time"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/core"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/evidence"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/guard"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/priority"
)

type guardMetricsClock struct{ now time.Time }

func (c guardMetricsClock) Now() time.Time { return c.now }

func TestApplyGuardEvidenceOnlyIgnoresExplicitlyDisabledCredential(t *testing.T) {
	now := time.Date(2026, time.August, 24, 14, 0, 0, 0, time.UTC)
	engine, err := guard.New(guard.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	identity := guard.Identity{
		AuthID:              "active-id",
		AuthIndex:           "active-index",
		IdentityFingerprint: "active-fingerprint",
		Provider:            "antigravity",
	}
	if err := engine.ReplaceRoster([]guard.Identity{identity}); err != nil {
		t.Fatal(err)
	}

	guardState := newGuardRuntimeState()
	guardState.mu.Lock()
	guardState.engine = engine
	guardState.inventoryReady = true
	guardState.mu.Unlock()
	runtime := &Runtime{guardRuntime: guardState, clock: guardMetricsClock{now: now}}
	remaining := int64(100)
	resetAt := now.Add(5 * time.Hour)
	runtime.applyGuardEvidence(map[config.AntigravityModelGroup]evidence.Result{
		config.AntigravityModelGroupGemini: {
			Eligible: []priority.QuotaEvidence{
				{
					AuthIndex:  "disabled-index",
					Provider:   core.ProviderAntigravity,
					ModelGroup: config.AntigravityModelGroupGemini,
					ObservedAt: now,
					ResetAt:    &resetAt,
					Remaining:  &remaining,
				},
				{
					AuthIndex:  identity.AuthIndex,
					Provider:   core.ProviderAntigravity,
					ModelGroup: config.AntigravityModelGroupGemini,
					ObservedAt: now,
					ResetAt:    &resetAt,
					Remaining:  &remaining,
				},
				{
					AuthIndex:  "malformed-enabled-index",
					Provider:   core.ProviderAntigravity,
					ModelGroup: config.AntigravityModelGroupGemini,
					ObservedAt: now,
					ResetAt:    &resetAt,
					Remaining:  &remaining,
				},
			},
		},
	}, map[string]guard.Identity{identity.AuthIndex: identity}, map[string]struct{}{"disabled-index": {}})

	metrics := engine.Metrics(now)
	if metrics.QuotaProbeSuccessTotal != 1 {
		t.Fatalf("quota probe successes = %d, want 1 active guard identity", metrics.QuotaProbeSuccessTotal)
	}
	if metrics.QuotaProbeErrorTotal != 1 {
		t.Fatalf("quota probe errors = %d, want only malformed enabled credential counted", metrics.QuotaProbeErrorTotal)
	}
}
