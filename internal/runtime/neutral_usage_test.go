package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"antigravity-priority/internal/host"
	"antigravity-priority/internal/runtime"
	"antigravity-priority/internal/state"
)

func TestRuntime_NeutralUsageKeepsHistoryWithoutCooldown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	mock := newMockHost()
	mock.files = []host.AuthFile{{Name: "neutral", AuthIndex: "neutral", Provider: "antigravity", Priority: 100}}
	r := newTestRuntime(t, runtime.Options{Host: mock})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{ConfigYAML: "state_cache_path: " + filepath.ToSlash(path) + "\n"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	for _, class := range []string{"upstream_429_ambiguous", "upstream_request_rejected", "upstream_attempt_budget_exhausted", "upstream_rate_limited"} {
		for i := 0; i < 3; i++ {
			raw := fmt.Sprintf(`{"Provider":"antigravity","Model":"gemini-3.7-flash","AuthIndex":"neutral","Failed":true,"Failure":{"StatusCode":429},"FailureClass":%q,"HealthEffect":"neutral","AttemptsUsed":2,"RetryExhausted":true}`, class)
			response := r.Handle(context.Background(), runtime.MethodUsageHandle, []byte(raw))
			var env runtime.Envelope
			if err := json.Unmarshal(response, &env); err != nil || !env.OK {
				t.Fatalf("neutral result failed: %s", response)
			}
		}
	}
	records, err := r.GetRequestHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 12 || !records[0].Failed || records[0].StatusCode != 429 || records[0].HealthEffect != "neutral" {
		t.Fatalf("raw failures lost: %+v", records)
	}
	store, err := state.Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.GetActiveCooldowns(time.Now())) != 0 {
		t.Fatal("neutral result created credential cooldown")
	}
}

func TestRuntime_NeutralPolicySuccessDoesNotClearRealQuota(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"priority":100,"disabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mock := newMockHost()
	mock.files = []host.AuthFile{{Name: "quota", AuthIndex: "quota", Provider: "antigravity", Priority: 100}}
	mock.authDocs["quota"] = host.AuthDocument{AuthIndex: "quota", Path: authPath}
	r := newTestRuntime(t, runtime.Options{Host: mock})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{ConfigYAML: "state_cache_path: " + filepath.ToSlash(cachePath) + "\n"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	quota := []byte(`{"Provider":"antigravity","Model":"gemini-3.7-flash","AuthIndex":"quota","Failed":true,"Failure":{"StatusCode":429},"FailureClass":"upstream_quota_exhausted","HealthEffect":"neutral"}`)
	for i := 0; i < 2; i++ {
		_ = r.Handle(context.Background(), runtime.MethodUsageHandle, quota)
	}
	before, err := state.Load(context.Background(), cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.GetActiveCooldowns(time.Now())) == 0 {
		t.Fatal("explicit quota was incorrectly treated as neutral")
	}
	policy := []byte(`{"Provider":"antigravity","Model":"gemini-3.7-flash","AuthIndex":"quota","Failed":false,"FailureClass":"upstream_request_rejected","HealthEffect":"neutral"}`)
	_ = r.Handle(context.Background(), runtime.MethodUsageHandle, policy)
	after, err := state.Load(context.Background(), cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.GetActiveCooldowns(time.Now())) == 0 {
		t.Fatal("HTTP200 policy refusal cleared real quota")
	}
}
