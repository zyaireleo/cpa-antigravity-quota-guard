package guard

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadAtomicModeReadbackAndNoRawBody(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.RegisterIdentity(testIdentity("auth-1", 100)); err != nil {
		t.Fatal(err)
	}
	applyPositive(t, engine, "auth-1", ModelGroupGemini, now)
	engine.ObserveUsage(UsageObservation{
		Provider: "antigravity", Model: "gemini-pro", AuthID: "id-auth-1",
		Failed: true, StatusCode: 429,
		Body:       `{"error":"quota exhausted","refresh_token":"SECRET-REFRESH","access_token":"SECRET-ACCESS"}`,
		ObservedAt: now.Add(time.Minute),
	})
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if !engine.IsDirty() {
		t.Fatal("mutated engine was not dirty")
	}
	if err := engine.Save(path); err != nil {
		t.Fatal(err)
	}
	if engine.IsDirty() {
		t.Fatal("successful save did not clear dirty backlog")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("state mode = %o, want 600", got)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET") || strings.Contains(string(data), "refresh_token") || strings.Contains(string(data), "access_token") {
		t.Fatalf("secret/raw body persisted: %s", data)
	}
	reloaded, err := Load(path, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	before := engine.Snapshot(now)
	after := reloaded.Snapshot(now)
	if len(after.Entries) != len(before.Entries) {
		t.Fatalf("reloaded entries = %d, want %d", len(after.Entries), len(before.Entries))
	}
	for index := range before.Entries {
		if !entryEqual(before.Entries[index], after.Entries[index]) {
			t.Fatalf("entry %d changed across reload: before=%#v after=%#v", index, before.Entries[index], after.Entries[index])
		}
	}
}

func TestSaveAndLoadRejectSymlinkTarget(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	realPath := filepath.Join(directory, "real.json")
	if err := os.WriteFile(realPath, []byte(`{"schema_version":1,"updated_at":"2026-08-24T00:00:00Z","entries":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(directory, "state.json")
	if err := os.Symlink(realPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	engine := testEngine(t)
	if err := engine.Save(symlinkPath); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("Save symlink error = %v", err)
	}
	if err := engine.Load(symlinkPath); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("Load symlink error = %v", err)
	}
}

func TestSaveRejectsRelativeParentSymlink(t *testing.T) {
	working := t.TempDir()
	outside := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(working); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	if err := os.Symlink(outside, "data"); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	engine := testEngine(t)
	path := filepath.Join("data", "cpa-antigravity-quota-guard", "state.json")
	if err := engine.Save(path); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("Save through parent symlink error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "cpa-antigravity-quota-guard", "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state escaped through parent symlink: %v", err)
	}
}

func TestLoadRejectsUnknownAndDuplicateState(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	unknownPath := filepath.Join(directory, "unknown.json")
	if err := os.WriteFile(unknownPath, []byte(`{"schema_version":1,"updated_at":"2026-08-24T00:00:00Z","entries":[],"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(unknownPath, DefaultConfig()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unknown field error = %v", err)
	}
	duplicatePath := filepath.Join(directory, "duplicate.json")
	entry := `{"auth_id":"id","auth_index":"index","identity_fingerprint":"fp","priority":1,"model_group":"gemini","state":"closed"}`
	document := `{"schema_version":1,"updated_at":"2026-08-24T00:00:00Z","entries":[` + entry + `,` + entry + `]}`
	if err := os.WriteFile(duplicatePath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(duplicatePath, DefaultConfig()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate state error = %v", err)
	}
}

func TestReadyRequiresFreshEvidencePerGroupAndUniformPriority(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	engine := testEngine(t)
	if err := engine.ReplaceRoster([]Identity{testIdentity("a", 100), testIdentity("b", 200)}); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"a", "b"} {
		applyPositive(t, engine, index, ModelGroupGemini, now)
		applyPositive(t, engine, index, ModelGroupClaudeGPT, now)
	}
	readiness := engine.Ready(now.Add(time.Minute), 30*time.Minute)
	if readiness.Ready || readiness.UniformPriority || !containsString(readiness.Reasons, "non_uniform_priority") {
		t.Fatalf("non-uniform readiness = %#v", readiness)
	}
	identityA := testIdentity("a", 100)
	identityB := testIdentity("b", 100)
	if err := engine.ReplaceRoster([]Identity{identityA, identityB}); err != nil {
		t.Fatal(err)
	}
	readiness = engine.Ready(now.Add(time.Minute), 30*time.Minute)
	if !readiness.Ready || !readiness.UniformPriority || readiness.Priority != 100 {
		t.Fatalf("ready state = %#v", readiness)
	}
	for _, group := range []ModelGroup{ModelGroupGemini, ModelGroupClaudeGPT} {
		applyPositive(t, engine, "a", group, now.Add(time.Hour))
	}
	partial := engine.Ready(now.Add(time.Hour+time.Minute), 30*time.Minute)
	if !partial.Ready {
		t.Fatalf("one stale account blocked fresh roster: %#v", partial)
	}
	applyPositive(t, engine, "a", ModelGroupClaudeGPT, now.Add(2*time.Hour))
	stale := engine.Ready(now.Add(2*time.Hour+time.Minute), 30*time.Minute)
	if stale.Ready {
		t.Fatalf("group without fresh evidence reported ready: %#v", stale)
	}
	if !containsString(stale.Reasons, "stale_evidence:gemini") {
		t.Fatalf("stale group reason missing: %#v", stale)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
