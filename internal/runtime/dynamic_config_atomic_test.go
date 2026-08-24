package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/guard"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/state"
)

func TestSetDynamicConfigRejectsAutoApplyWithoutMutation(t *testing.T) {
	withDynamicConfigTempWorkingDirectory(t)
	cachePath := filepath.Join(t.TempDir(), "quota-cache.json")
	r := New(Options{StateCachePath: cachePath})
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	if _, err := r.Register(context.Background(), RegisterRequest{}); err != nil {
		t.Fatal(err)
	}

	beforeConfig, err := r.Config()
	if err != nil {
		t.Fatal(err)
	}
	beforeGuard := r.guardRuntime.currentConfig()
	dynamic := beforeConfig.Dynamic()
	dynamic.AutoApply = true
	dynamic.Interval = "45m"

	err = r.SetDynamicConfig(context.Background(), dynamic)
	if err == nil || !strings.Contains(err.Error(), "auto_apply is no longer supported") {
		t.Fatalf("SetDynamicConfig error=%v, want auto_apply rejection", err)
	}
	assertDynamicConfigRuntimeUnchanged(t, r, beforeConfig, beforeGuard)

	store, err := state.Load(context.Background(), cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.GetDynamicConfig(); ok {
		t.Fatal("rejected auto_apply document was persisted")
	}
}

func TestSetDynamicConfigPersistenceFailureDoesNotPartiallySwitch(t *testing.T) {
	withDynamicConfigTempWorkingDirectory(t)
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(parentFile, "quota-cache.json")
	r := New(Options{StateCachePath: cachePath})
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	if _, err := r.Register(context.Background(), RegisterRequest{}); err != nil {
		t.Fatal(err)
	}

	beforeConfig, err := r.Config()
	if err != nil {
		t.Fatal(err)
	}
	beforeGuard := r.guardRuntime.currentConfig()
	dynamic := beforeConfig.Dynamic()
	dynamic.Interval = "45m"
	dynamic.Guard.Generic429.Threshold = 4

	err = r.SetDynamicConfig(context.Background(), dynamic)
	if err == nil || (!strings.Contains(err.Error(), "load state for save") && !strings.Contains(err.Error(), "save dynamic config")) {
		t.Fatalf("SetDynamicConfig error=%v, want persistence failure", err)
	}
	assertDynamicConfigRuntimeUnchanged(t, r, beforeConfig, beforeGuard)
}

func TestSetDynamicConfigGuardPreparationFailureDoesNotPersistOrSwitch(t *testing.T) {
	withDynamicConfigTempWorkingDirectory(t)
	cachePath := filepath.Join(t.TempDir(), "quota-cache.json")
	r := New(Options{StateCachePath: cachePath})
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	if _, err := r.Register(context.Background(), RegisterRequest{
		ConfigYAML: `required-scheduler-for: [antigravity]`,
		HostFeatures: []string{
			hostFeatureRequiredSchedulerV1,
			hostFeatureSchedulerRequestIDV1,
			hostFeatureSchedulerDirectResponseV1,
			hostFeatureAuthInventoryReadyV1,
		},
	}); err != nil {
		t.Fatal(err)
	}

	beforeConfig, err := r.Config()
	if err != nil {
		t.Fatal(err)
	}
	beforeGuard := r.guardRuntime.currentConfig()
	dynamic := beforeConfig.Dynamic()
	dynamic.Guard.Mode = config.GuardModeEnforce
	dynamic.Interval = "45m"

	err = r.SetDynamicConfig(context.Background(), dynamic)
	if err == nil || !strings.Contains(err.Error(), "host callbacks are required") {
		t.Fatalf("SetDynamicConfig error=%v, want guard preparation failure", err)
	}
	assertDynamicConfigRuntimeUnchanged(t, r, beforeConfig, beforeGuard)

	store, err := state.Load(context.Background(), cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.GetDynamicConfig(); ok {
		t.Fatal("guard preparation failure persisted dynamic config")
	}
}

func TestSetDynamicConfigPersistsBeforeCanonicalRuntimeSwitch(t *testing.T) {
	withDynamicConfigTempWorkingDirectory(t)
	cachePath := filepath.Join(t.TempDir(), "quota-cache.json")
	r := New(Options{StateCachePath: cachePath})
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	if _, err := r.Register(context.Background(), RegisterRequest{}); err != nil {
		t.Fatal(err)
	}

	dynamic, err := r.GetDynamicConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dynamic.Interval = "45m"
	dynamic.Guard.Generic429.Threshold = 4
	dynamic.Guard.Generic429.Window = "2m"
	if err := r.SetDynamicConfig(context.Background(), dynamic); err != nil {
		t.Fatal(err)
	}

	active, err := r.Config()
	if err != nil {
		t.Fatal(err)
	}
	if active.AutoApply || active.Interval != 45*time.Minute || active.Guard.Generic429.Threshold != 4 || active.Guard.Generic429.Window != 2*time.Minute {
		t.Fatalf("unexpected active config after commit: %+v", active)
	}
	if got := r.guardRuntime.currentConfig(); !reflect.DeepEqual(got, active.Guard) {
		t.Fatalf("guard runtime=%+v, active config guard=%+v", got, active.Guard)
	}

	store, err := state.Load(context.Background(), cachePath)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := store.GetDynamicConfig()
	if !ok {
		t.Fatal("validated dynamic config was not persisted")
	}
	if persisted.AutoApply || persisted.Interval != "45m0s" || persisted.Guard.Generic429.Threshold != 4 || persisted.Guard.Generic429.Window != "2m0s" {
		t.Fatalf("persisted config is not canonical: %+v", persisted)
	}
}

func TestGetDynamicConfigIgnoresPersistedLegacyAutoApply(t *testing.T) {
	withDynamicConfigTempWorkingDirectory(t)
	cachePath := filepath.Join(t.TempDir(), "quota-cache.json")
	store, err := state.Load(context.Background(), cachePath)
	if err != nil {
		t.Fatal(err)
	}
	legacy := config.Default().Dynamic()
	legacy.AutoApply = true
	legacy.Interval = "45m"
	store.SetDynamicConfig(legacy)
	if err := store.SaveAtomic(context.Background()); err != nil {
		t.Fatal(err)
	}

	r := New(Options{StateCachePath: cachePath})
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	got, err := r.GetDynamicConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoApply || got.Interval != config.Default().Interval.String() {
		t.Fatalf("invalid legacy config leaked through API: %+v", got)
	}
}

func TestRapidDynamicConfigSwitchShutdownWaitsForEveryGuardGeneration(t *testing.T) {
	withDynamicConfigTempWorkingDirectory(t)
	cachePath := filepath.Join(t.TempDir(), "quota-cache.json")
	r := New(Options{StateCachePath: cachePath})
	if _, err := r.Register(context.Background(), RegisterRequest{}); err != nil {
		t.Fatal(err)
	}

	doneChannels := make([]<-chan struct{}, 0, 9)
	captureDone := func() {
		r.guardRuntime.mu.RLock()
		doneChannels = append(doneChannels, r.guardRuntime.done)
		r.guardRuntime.mu.RUnlock()
	}
	captureDone()
	for threshold := 2; threshold <= 9; threshold++ {
		dynamic, err := r.GetDynamicConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		dynamic.Guard.Generic429.Threshold = threshold
		if err := r.SetDynamicConfig(context.Background(), dynamic); err != nil {
			t.Fatalf("generation %d: %v", threshold, err)
		}
		captureDone()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown did not retire every guard generation: %v", err)
	}
	for index, done := range doneChannels {
		if done == nil {
			t.Fatalf("generation %d did not publish a done channel", index)
		}
		select {
		case <-done:
		default:
			t.Errorf("generation %d still running after Shutdown returned", index)
		}
	}
}

func TestSetDynamicConfigDoesNotLoseConcurrentUsageCooldown(t *testing.T) {
	withDynamicConfigTempWorkingDirectory(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r := New(Options{
		Clock:          &officialTestClock{now: now},
		StateCachePath: filepath.Join(t.TempDir(), "quota-cache.json"),
	})
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	if _, err := r.Register(context.Background(), RegisterRequest{}); err != nil {
		t.Fatal(err)
	}
	engine, _ := r.guardRuntime.currentEngine()
	if err := engine.RegisterIdentity(guard.Identity{
		AuthID: "auth-id-1", AuthIndex: "auth-1", IdentityFingerprint: "fingerprint-1", Provider: "antigravity", Priority: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ApplyQuotaEvidence(guard.QuotaEvidence{
		AuthID: "auth-id-1", AuthIndex: "auth-1", IdentityFingerprint: "fingerprint-1",
		Group: guard.ModelGroupGemini, Remaining: 80, ResetAt: now.Add(time.Hour), ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Hold persistence so SetDynamicConfig is deterministically paused after it
	// takes the generation write gate and before it reloads/switches engines.
	r.guardRuntime.persistMu.Lock()
	dynamic, err := r.GetDynamicConfig(context.Background())
	if err != nil {
		r.guardRuntime.persistMu.Unlock()
		t.Fatal(err)
	}
	dynamic.Guard.Generic429.Threshold = 3
	configDone := make(chan error, 1)
	go func() { configDone <- r.SetDynamicConfig(context.Background(), dynamic) }()

	deadline := time.Now().Add(2 * time.Second)
	for r.guardRuntime.access.TryRLock() {
		r.guardRuntime.access.RUnlock()
		if time.Now().After(deadline) {
			r.guardRuntime.persistMu.Unlock()
			t.Fatal("SetDynamicConfig did not acquire the guard generation gate")
		}
		time.Sleep(time.Millisecond)
	}

	usageDone := make(chan struct{})
	go func() {
		raw, _ := json.Marshal(usageRecord{
			RequestID: "concurrent-usage", Provider: "antigravity", Model: "gemini-2.5-pro",
			AuthID: "auth-id-1", AuthIndex: "auth-1", Failed: true,
			Failure:     usageFailure{StatusCode: http.StatusTooManyRequests, Body: `{"error":"quota exhausted"}`},
			RequestedAt: now, Latency: time.Second,
		})
		_ = r.Handle(context.Background(), MethodUsageHandle, raw)
		close(usageDone)
	}()

	r.guardRuntime.persistMu.Unlock()
	if err := <-configDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-usageDone:
	case <-time.After(2 * time.Second):
		t.Fatal("usage event remained blocked after config switch")
	}

	active, _ := r.guardRuntime.currentEngine()
	entry := entryFromRuntimeSnapshot(t, active.Snapshot(now.Add(time.Second)), "auth-1", guard.ModelGroupGemini)
	if entry.State != guard.StateOpen || entry.Reason != guard.ReasonExplicit429 {
		t.Fatalf("concurrent cooldown was lost across config switch: %#v", entry)
	}
}

func assertDynamicConfigRuntimeUnchanged(t *testing.T, r *Runtime, wantConfig config.Config, wantGuard config.GuardConfig) {
	t.Helper()
	gotConfig, err := r.Config()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotConfig, wantConfig) {
		t.Fatalf("runtime config partially changed:\n got: %+v\nwant: %+v", gotConfig, wantConfig)
	}
	if gotGuard := r.guardRuntime.currentConfig(); !reflect.DeepEqual(gotGuard, wantGuard) {
		t.Fatalf("guard config partially changed:\n got: %+v\nwant: %+v", gotGuard, wantGuard)
	}
}

func withDynamicConfigTempWorkingDirectory(t *testing.T) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}
