package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/guard"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/host"
)

type officialTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *officialTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *officialTestClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type officialTestHost struct {
	mu        sync.Mutex
	files     []host.AuthFile
	documents map[string]host.AuthDocument
	saveCalls int
	httpCalls int
}

type inventoryAwareTestHost struct {
	*officialTestHost
	ready bool
}

func (h *inventoryAwareTestHost) ListAuthInventory(ctx context.Context) (host.AuthInventory, error) {
	files, err := h.ListAuthFiles(ctx)
	if err != nil {
		return host.AuthInventory{}, err
	}
	h.mu.Lock()
	ready := h.ready
	h.mu.Unlock()
	return host.AuthInventory{Files: files, Ready: ready}, nil
}

func (h *inventoryAwareTestHost) setReady(ready bool) {
	h.mu.Lock()
	h.ready = ready
	h.mu.Unlock()
}

func (h *officialTestHost) ListAuthFiles(context.Context) ([]host.AuthFile, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]host.AuthFile(nil), h.files...), nil
}

func (h *officialTestHost) GetAuth(_ context.Context, authIndex string) (host.AuthDocument, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if document, ok := h.documents[authIndex]; ok {
		return document, nil
	}
	return host.AuthDocument{
		AuthIndex: authIndex,
		JSON:      json.RawMessage(`{"access_token":"test-token","project_id":"test-project"}`),
	}, nil
}

func (h *officialTestHost) GetRuntime(context.Context, string) (host.RuntimeAuth, error) {
	return host.RuntimeAuth{}, nil
}

func (h *officialTestHost) SaveAuth(context.Context, string, json.RawMessage) error {
	h.mu.Lock()
	h.saveCalls++
	h.mu.Unlock()
	return nil
}

func (h *officialTestHost) HTTPDo(context.Context, host.HTTPRequest) (host.HTTPResponse, error) {
	h.mu.Lock()
	h.httpCalls++
	h.mu.Unlock()
	return host.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{
		"models": {
			"gemini-2.5-pro": {"quotaInfo":{"windows":[{"name":"5h","remainingFraction":0.8,"resetTime":"2026-08-25T00:00:00Z"}]}},
			"claude-sonnet-4": {"quotaInfo":{"windows":[{"name":"7d","remainingFraction":0.7,"resetTime":"2026-08-31T00:00:00Z"}]}}
		}
	}`)}, nil
}

func (h *officialTestHost) saves() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.saveCalls
}

func (h *officialTestHost) httpRequestCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.httpCalls
}

func newOfficialGuardRuntime(t *testing.T, identities []guard.Identity) (*Runtime, *officialTestClock, *officialTestHost) {
	t.Helper()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	clock := &officialTestClock{now: now}
	hostCallbacks := &officialTestHost{documents: make(map[string]host.AuthDocument)}
	runtime := New(Options{
		Clock:          clock,
		Host:           hostCallbacks,
		StateCachePath: filepath.Join(t.TempDir(), "quota-cache.json"),
		GuardStatePath: filepath.Join(t.TempDir(), "guard-state.json"),
	})
	t.Cleanup(func() {
		_ = runtime.Shutdown(context.Background())
	})

	engine, _ := runtime.guardRuntime.currentEngine()
	if err := engine.ReplaceRoster(identities); err != nil {
		t.Fatal(err)
	}
	for _, identity := range identities {
		for _, group := range []guard.ModelGroup{guard.ModelGroupGemini, guard.ModelGroupClaudeGPT} {
			if _, err := engine.ApplyQuotaEvidence(guard.QuotaEvidence{
				AuthID:     identity.AuthID,
				AuthIndex:  identity.AuthIndex,
				Group:      group,
				Remaining:  80,
				ResetAt:    now.Add(24 * time.Hour),
				ObservedAt: now,
				Source:     "quota_probe",
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	runtime.mu.Lock()
	runtime.cfg.Enabled = true
	runtime.cfg.AutoApply = false
	runtime.cfg.Guard.Mode = config.GuardModeEnforce
	runtime.cfg.Guard.EnforcedGroups = []config.AntigravityModelGroup{
		config.AntigravityModelGroupGemini,
		config.AntigravityModelGroupClaudeGPT,
	}
	runtime.mu.Unlock()
	runtime.guardRuntime.mu.Lock()
	runtime.guardRuntime.inventoryReady = true
	runtime.guardRuntime.baselineReady = true
	runtime.guardRuntime.mu.Unlock()
	return runtime, clock, hostCallbacks
}

func TestObserveAcceptsStockHostWithoutRequiredSchedulerFeatures(t *testing.T) {
	runtime := New(Options{
		StateCachePath: filepath.Join(t.TempDir(), "quota-cache.json"),
		GuardStatePath: filepath.Join(t.TempDir(), "guard-state.json"),
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	result, err := runtime.Register(context.Background(), RegisterRequest{})
	if err != nil {
		t.Fatalf("Register observe mode error=%v, want stock host compatibility", err)
	}
	if !result.Capabilities["scheduler"] || !result.Capabilities["usage_plugin"] || !result.Capabilities["request_interceptor"] || !result.Capabilities["management_api"] {
		t.Fatalf("Register observe capabilities=%+v, want official quota guard capabilities", result.Capabilities)
	}
}

func TestLifecycleWirePreservesHostFeatures(t *testing.T) {
	runtime := New(Options{
		StateCachePath: filepath.Join(t.TempDir(), "quota-cache.json"),
		GuardStatePath: filepath.Join(t.TempDir(), "guard-state.json"),
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	wire, err := json.Marshal(struct {
		ConfigYAML    []byte   `json:"config_yaml"`
		SchemaVersion uint32   `json:"schema_version"`
		HostFeatures  []string `json:"host_features"`
	}{
		ConfigYAML:    []byte("mode: observe\nrequired-scheduler-for: [antigravity]\n"),
		SchemaVersion: 3,
		HostFeatures: []string{
			hostFeatureRequiredSchedulerV1,
			hostFeatureSchedulerRequestIDV1,
			hostFeatureSchedulerDirectResponseV1,
			hostFeatureAuthInventoryReadyV1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(runtime.Handle(context.Background(), MethodPluginRegister, wire), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		t.Fatalf("plugin.register failed: %+v", envelope.Error)
	}
	features := runtime.currentHostFeatures()
	if !requiredSchedulerABIReady(runtime.currentConfig(), features) {
		t.Fatalf("lifecycle host features were lost: config=%+v features=%v", runtime.currentConfig().RequiredSchedulerFor, sortedFeatureNames(features))
	}
}

func TestEnforceRejectsMissingRequiredSchedulerHostFeatures(t *testing.T) {
	tests := []struct {
		name        string
		features    []string
		wantMissing string
	}{
		{name: "stock host", wantMissing: hostFeatureRequiredSchedulerV1},
		{
			name:        "missing scheduler request id",
			features:    []string{hostFeatureRequiredSchedulerV1},
			wantMissing: hostFeatureSchedulerRequestIDV1,
		},
		{
			name:        "missing scheduler direct response",
			features:    []string{hostFeatureRequiredSchedulerV1, hostFeatureSchedulerRequestIDV1},
			wantMissing: hostFeatureSchedulerDirectResponseV1,
		},
		{
			name: "missing auth inventory readiness",
			features: []string{
				hostFeatureRequiredSchedulerV1,
				hostFeatureSchedulerRequestIDV1,
				hostFeatureSchedulerDirectResponseV1,
			},
			wantMissing: hostFeatureAuthInventoryReadyV1,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			runtime := New(Options{
				StateCachePath: filepath.Join(t.TempDir(), "quota-cache.json"),
				GuardStatePath: filepath.Join(t.TempDir(), "guard-state.json"),
			})
			t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
			_, err := runtime.Register(context.Background(), RegisterRequest{
				ConfigYAML:   "mode: enforce\nrequired-scheduler-for: [antigravity]\n",
				HostFeatures: testCase.features,
			})
			if err == nil || !strings.Contains(err.Error(), testCase.wantMissing) {
				t.Fatalf("Register error=%v, want missing host feature %q", err, testCase.wantMissing)
			}
		})
	}
}

func TestEnforcePreflightRejectsUnwritableGuardState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX chmod-based unwritable-directory check is not portable to Windows")
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "guard.json")
	file := host.AuthFile{
		ID: "auth-id-1", Name: "account-1", AuthIndex: "auth-1", Provider: "antigravity", Type: "antigravity", Priority: 100,
	}
	identity := guard.Identity{
		AuthID: file.ID, AuthIndex: file.AuthIndex, IdentityFingerprint: guardIdentityFingerprint(file), Provider: "antigravity", Priority: 100,
	}
	engine, err := guard.New(guard.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ReplaceRoster([]guard.Identity{identity}); err != nil {
		t.Fatal(err)
	}
	for _, group := range []guard.ModelGroup{guard.ModelGroupGemini, guard.ModelGroupClaudeGPT} {
		if _, err := engine.ApplyQuotaEvidence(guard.QuotaEvidence{
			AuthID: identity.AuthID, AuthIndex: identity.AuthIndex, IdentityFingerprint: identity.IdentityFingerprint,
			Group: group, Remaining: 80, ResetAt: now.Add(time.Hour), ObservedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Save(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })

	hostCallbacks := &officialTestHost{files: []host.AuthFile{file}, documents: make(map[string]host.AuthDocument)}
	runtime := New(Options{
		Clock:          &officialTestClock{now: now},
		Host:           hostCallbacks,
		StateCachePath: filepath.Join(t.TempDir(), "cache.json"),
		GuardStatePath: statePath,
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	_, err = runtime.Register(context.Background(), RegisterRequest{
		ConfigYAML: "mode: enforce\nrequired-scheduler-for: [antigravity]\n",
		HostFeatures: []string{
			hostFeatureRequiredSchedulerV1,
			hostFeatureSchedulerRequestIDV1,
			hostFeatureSchedulerDirectResponseV1,
			hostFeatureAuthInventoryReadyV1,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "verify writable quota guard state") {
		t.Fatalf("Register error=%v, want writable state preflight failure", err)
	}
}

func TestEnforceColdStartArmsUntilAuthInventoryReadyWithoutErasingState(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "guard.json")
	file := host.AuthFile{
		ID: "auth-id-1", Name: "account-1", AuthIndex: "auth-1", Provider: "antigravity", Type: "antigravity", Priority: 100,
	}
	identity := guard.Identity{
		AuthID: file.ID, AuthIndex: file.AuthIndex, IdentityFingerprint: guardIdentityFingerprint(file), Provider: "antigravity", Priority: 100,
	}
	engine, err := guard.New(guard.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ReplaceRoster([]guard.Identity{identity}); err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []guard.QuotaEvidence{
		{
			AuthID: identity.AuthID, AuthIndex: identity.AuthIndex, IdentityFingerprint: identity.IdentityFingerprint,
			Group: guard.ModelGroupGemini, Remaining: 0, ResetAt: now.Add(time.Hour), ObservedAt: now, Source: "quota_probe",
		},
		{
			AuthID: identity.AuthID, AuthIndex: identity.AuthIndex, IdentityFingerprint: identity.IdentityFingerprint,
			Group: guard.ModelGroupClaudeGPT, Remaining: 80, ResetAt: now.Add(time.Hour), ObservedAt: now, Source: "quota_probe",
		},
	} {
		if _, err := engine.ApplyQuotaEvidence(evidence); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Save(statePath); err != nil {
		t.Fatal(err)
	}

	hostCallbacks := &inventoryAwareTestHost{
		officialTestHost: &officialTestHost{files: []host.AuthFile{file}, documents: make(map[string]host.AuthDocument)},
		ready:            false,
	}
	runtime := New(Options{
		Clock:          &officialTestClock{now: now},
		Host:           hostCallbacks,
		StateCachePath: filepath.Join(t.TempDir(), "cache.json"),
		GuardStatePath: statePath,
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	request := RegisterRequest{
		ConfigYAML: "mode: enforce\nrequired-scheduler-for: [antigravity]\n",
		HostFeatures: []string{
			hostFeatureRequiredSchedulerV1,
			hostFeatureSchedulerRequestIDV1,
			hostFeatureSchedulerDirectResponseV1,
			hostFeatureAuthInventoryReadyV1,
		},
	}
	registered, err := runtime.Register(context.Background(), request)
	if err != nil {
		t.Fatalf("cold plugin.register error=%v, want armed registration", err)
	}
	if !registered.Capabilities["scheduler"] || runtime.guardRuntime.enforcementIsReady() {
		t.Fatalf("cold registration capabilities=%+v ready=%v", registered.Capabilities, runtime.guardRuntime.enforcementIsReady())
	}
	_, _, generation := runtime.guardRuntime.currentEngineGeneration()
	runtime.guardRuntime.runProbe(context.Background(), runtime, generation)
	if calls := hostCallbacks.httpRequestCount(); calls != 0 {
		t.Fatalf("armed-not-ready startup probe made %d outbound host HTTP calls", calls)
	}
	managementRaw, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/cpa-antigravity-quota-guard/actions/probe",
	})
	managementResponse, managementErr := decodeOfficialEnvelope[ManagementResponse](t, runtime.Handle(context.Background(), MethodManagementHandle, managementRaw))
	if managementErr != nil || managementResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("armed-not-ready management probe response=%#v error=%+v, want 503", managementResponse, managementErr)
	}
	if calls := hostCallbacks.httpRequestCount(); calls != 0 {
		t.Fatalf("armed-not-ready management probe made %d outbound host HTTP calls", calls)
	}
	loaded, err := guard.Load(statePath, guard.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	entry := entryFromRuntimeSnapshot(t, loaded.Snapshot(now), "auth-1", guard.ModelGroupGemini)
	if entry.State != guard.StateOpen || entry.Reason != guard.ReasonQuotaZero {
		t.Fatalf("bootstrap registration erased persisted cooldown: %#v", entry)
	}

	_, envelopeErr := schedulerPick(t, runtime, "gemini-2.5-pro", []schedulerAuthCandidate{{ID: file.ID, Provider: "antigravity", Priority: 100}})
	if envelopeErr == nil || envelopeErr.Code != guard.ErrorCodeGuardNotReady || envelopeErr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("cold scheduler error=%+v, want local quota_guard_not_ready 503", envelopeErr)
	}

	hostCallbacks.setReady(true)
	if _, err := runtime.Reconfigure(context.Background(), ReconfigureRequest(request)); err != nil {
		t.Fatalf("ready plugin.reconfigure error=%v", err)
	}
	if !runtime.guardRuntime.enforcementIsReady() {
		t.Fatal("enforcement did not become ready after auth inventory reconciliation")
	}
	_, envelopeErr = schedulerPick(t, runtime, "gemini-2.5-pro", []schedulerAuthCandidate{{ID: file.ID, Provider: "antigravity", Priority: 100}})
	if envelopeErr == nil || envelopeErr.Code != guard.ErrorCodeModelCooldown || envelopeErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("reconciled scheduler error=%+v, want persisted model_cooldown 429", envelopeErr)
	}
}

func decodeOfficialEnvelope[T any](t *testing.T, raw []byte) (T, *EnvelopeError) {
	t.Helper()
	var zero T
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v: %s", err, raw)
	}
	if !envelope.OK {
		return zero, envelope.Error
	}
	var result T
	if len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, &result); err != nil {
			t.Fatalf("decode result: %v: %s", err, envelope.Result)
		}
	}
	return result, nil
}

func sendUsage429(t *testing.T, runtime *Runtime, clock *officialTestClock, authID, authIndex, model, body string, headers http.Header) {
	t.Helper()
	record := usageRecord{
		Provider:        "antigravity",
		Model:           model,
		AuthID:          authID,
		AuthIndex:       authIndex,
		Failed:          true,
		Failure:         usageFailure{StatusCode: http.StatusTooManyRequests, Body: body},
		ResponseHeaders: headers,
		RequestedAt:     clock.Now(),
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, envelopeErr := decodeOfficialEnvelope[struct{}](t, runtime.Handle(context.Background(), MethodUsageHandle, raw)); envelopeErr != nil {
		t.Fatalf("usage.handle failed: %+v", envelopeErr)
	}
}

func schedulerPick(t *testing.T, runtime *Runtime, model string, candidates []schedulerAuthCandidate, providers ...string) (schedulerPickResponse, *EnvelopeError) {
	t.Helper()
	request := schedulerPickRequest{
		RequestID:  "scheduler-request",
		Provider:   "antigravity",
		Providers:  providers,
		Model:      model,
		Candidates: candidates,
	}
	if len(providers) > 0 {
		request.Provider = ""
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return decodeOfficialEnvelope[schedulerPickResponse](t, runtime.Handle(context.Background(), MethodSchedulerPick, raw))
}

func TestOfficialUsageAndSchedulerEnforceModelGroupCooldown(t *testing.T) {
	runtime, clock, hostCallbacks := newOfficialGuardRuntime(t, []guard.Identity{
		{AuthID: "auth-id-1", AuthIndex: "auth-1", IdentityFingerprint: "fp-1", Provider: "antigravity", Priority: 100},
		{AuthID: "auth-id-2", AuthIndex: "auth-2", IdentityFingerprint: "fp-2", Provider: "antigravity", Priority: 100},
	})
	candidates := []schedulerAuthCandidate{
		{ID: "auth-id-1", Provider: "antigravity", Priority: 100},
		{ID: "auth-id-2", Provider: "antigravity", Priority: 100},
	}

	sendUsage429(t, runtime, clock, "auth-id-1", "auth-1", "gemini-2.5-pro", `{"error":"rate limited"}`, nil)
	clock.Set(clock.Now().Add(10 * time.Second))
	sendUsage429(t, runtime, clock, "auth-id-1", "auth-1", "gemini-2.5-pro", `{"error":"rate limited"}`, nil)

	gemini, envelopeErr := schedulerPick(t, runtime, "gemini-2.5-pro", candidates)
	if envelopeErr != nil {
		t.Fatalf("gemini pick failed: %+v", envelopeErr)
	}
	if !gemini.Handled || gemini.AuthID != "auth-id-2" {
		t.Fatalf("gemini pick = %#v; want healthy auth-id-2", gemini)
	}

	claude, envelopeErr := schedulerPick(t, runtime, "claude-sonnet-4", candidates)
	if envelopeErr != nil {
		t.Fatalf("claude pick failed: %+v", envelopeErr)
	}
	if !claude.Handled || claude.AuthID != "auth-id-1" {
		t.Fatalf("claude pick = %#v; gemini cooldown must not affect claude_gpt", claude)
	}
	if hostCallbacks.saves() != 0 {
		t.Fatalf("official usage/scheduler path called host.auth.save %d times", hostCallbacks.saves())
	}
}

func TestOfficialSchedulerAllCoolingAndAfterAuthRetryAfter(t *testing.T) {
	runtime, clock, _ := newOfficialGuardRuntime(t, []guard.Identity{
		{AuthID: "auth-id-1", AuthIndex: "auth-1", IdentityFingerprint: "fp-1", Provider: "antigravity", Priority: 100},
		{AuthID: "auth-id-2", AuthIndex: "auth-2", IdentityFingerprint: "fp-2", Provider: "antigravity", Priority: 100},
	})
	for _, identity := range []struct{ id, index string }{{"auth-id-1", "auth-1"}, {"auth-id-2", "auth-2"}} {
		sendUsage429(t, runtime, clock, identity.id, identity.index, "gemini-2.5-pro", `{"error":{"message":"quota exhausted"}}`, http.Header{"Retry-After": {"120"}})
	}
	candidates := []schedulerAuthCandidate{
		{ID: "auth-id-1", Provider: "antigravity", Priority: 100},
		{ID: "auth-id-2", Provider: "antigravity", Priority: 100},
	}
	_, envelopeErr := schedulerPick(t, runtime, "gemini-2.5-pro", candidates)
	if envelopeErr == nil || envelopeErr.Code != guard.ErrorCodeModelCooldown || envelopeErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("scheduler error = %+v; want model_cooldown HTTP 429", envelopeErr)
	}
	if envelopeErr.ResponseHeaders.Get("Retry-After") != "120" || !strings.Contains(string(envelopeErr.ResponseBody), `"code":"model_cooldown"`) {
		t.Fatalf("scheduler direct response contract = headers %#v body %s", envelopeErr.ResponseHeaders, envelopeErr.ResponseBody)
	}

	request, err := json.Marshal(requestInterceptRequest{ToFormat: "antigravity", Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatal(err)
	}
	response, envelopeErr := decodeOfficialEnvelope[requestInterceptResponse](t, runtime.Handle(context.Background(), MethodRequestAfter, request))
	if envelopeErr != nil {
		t.Fatalf("request interceptor failed: %+v", envelopeErr)
	}
	if !response.Terminate || response.StatusCode != http.StatusTooManyRequests || response.ResponseHeaders.Get("Retry-After") != "120" {
		t.Fatalf("after-auth response = %#v; want local 429 with Retry-After=120", response)
	}
	if !strings.Contains(string(response.ResponseBody), `"code":"model_cooldown"`) {
		t.Fatalf("unexpected local error body: %s", response.ResponseBody)
	}
}

func TestOfficialHalfOpenLeasePassesAfterAuthBoundary(t *testing.T) {
	runtime, clock, _ := newOfficialGuardRuntime(t, []guard.Identity{
		{AuthID: "auth-id-1", AuthIndex: "auth-1", IdentityFingerprint: "fp-1", Provider: "antigravity", Priority: 100},
	})
	sendUsage429(t, runtime, clock, "auth-id-1", "auth-1", "gemini-2.5-pro", `{"error":{"message":"quota exhausted"}}`, http.Header{"Retry-After": {"60"}})
	clock.Set(clock.Now().Add(61 * time.Second))
	pick, envelopeErr := schedulerPick(t, runtime, "gemini-2.5-pro", []schedulerAuthCandidate{{ID: "auth-id-1", Provider: "antigravity", Priority: 100}})
	if envelopeErr != nil || !pick.Handled || pick.AuthID != "auth-id-1" {
		t.Fatalf("half-open pick = %#v, error=%+v", pick, envelopeErr)
	}

	raw, _ := json.Marshal(requestInterceptRequest{ToFormat: "antigravity", Model: "gemini-2.5-pro"})
	response, envelopeErr := decodeOfficialEnvelope[requestInterceptResponse](t, runtime.Handle(context.Background(), MethodRequestAfter, raw))
	if envelopeErr != nil || response.Terminate {
		t.Fatalf("leased half-open request was blocked: response=%#v error=%+v", response, envelopeErr)
	}
}

func TestOfficialManagementHalfOpenRunsOneProbeAndClosesOnSuccess(t *testing.T) {
	runtime, clock, _ := newOfficialGuardRuntime(t, []guard.Identity{
		{AuthID: "auth-id-1", AuthIndex: "auth-1", IdentityFingerprint: "fp-1", Provider: "antigravity", Priority: 100},
	})
	sendUsage429(t, runtime, clock, "auth-id-1", "auth-1", "gemini-2.5-pro", `{"error":{"message":"quota exhausted"}}`, http.Header{"Retry-After": {"300"}})

	action := []byte(`{"auth_index":"auth-1","model_group":"gemini"}`)
	managementRaw, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/cpa-antigravity-quota-guard/actions/half-open",
		Body:   action,
	})
	managementResponse, envelopeErr := decodeOfficialEnvelope[ManagementResponse](t, runtime.Handle(context.Background(), MethodManagementHandle, managementRaw))
	if envelopeErr != nil || managementResponse.StatusCode != http.StatusOK {
		body, _ := base64.StdEncoding.DecodeString(managementResponse.Body)
		t.Fatalf("manual half-open failed: response=%#v error=%+v body=%s", managementResponse, envelopeErr, body)
	}

	pick, envelopeErr := schedulerPick(t, runtime, "gemini-2.5-pro", []schedulerAuthCandidate{{ID: "auth-id-1", Provider: "antigravity", Priority: 100}})
	if envelopeErr != nil || pick.AuthID != "auth-id-1" {
		t.Fatalf("manual half-open was not consumed by scheduler: pick=%#v error=%+v", pick, envelopeErr)
	}
	raw, _ := json.Marshal(requestInterceptRequest{ToFormat: "antigravity", Model: "gemini-2.5-pro"})
	intercept, envelopeErr := decodeOfficialEnvelope[requestInterceptResponse](t, runtime.Handle(context.Background(), MethodRequestAfter, raw))
	if envelopeErr != nil || intercept.Terminate {
		t.Fatalf("manual half-open request was blocked: response=%#v error=%+v", intercept, envelopeErr)
	}

	successAt := clock.Now().Add(time.Second)
	successRaw, _ := json.Marshal(usageRecord{
		RequestID: "scheduler-request",
		Provider:  "antigravity", Model: "gemini-2.5-pro", AuthID: "auth-id-1", AuthIndex: "auth-1",
		RequestedAt: clock.Now(), Latency: time.Second, Failed: false,
	})
	if _, envelopeErr := decodeOfficialEnvelope[struct{}](t, runtime.Handle(context.Background(), MethodUsageHandle, successRaw)); envelopeErr != nil {
		t.Fatal(envelopeErr)
	}
	engine, _ := runtime.guardRuntime.currentEngine()
	entry := entryFromRuntimeSnapshot(t, engine.Snapshot(successAt), "auth-1", guard.ModelGroupGemini)
	if entry.State != guard.StateClosed {
		t.Fatalf("successful manual half-open did not close breaker: %#v", entry)
	}
}

func entryFromRuntimeSnapshot(t *testing.T, snapshot guard.Snapshot, authIndex string, group guard.ModelGroup) guard.Entry {
	t.Helper()
	for _, entry := range snapshot.Entries {
		if entry.AuthIndex == authIndex && entry.ModelGroup == group {
			return entry
		}
	}
	t.Fatalf("missing guard entry %s/%s", authIndex, group)
	return guard.Entry{}
}

func TestUnavailableRosterRefreshPreservesBreakerState(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	engine, err := guard.New(guard.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	files := []host.AuthFile{{
		ID: "auth-id-1", Name: "antigravity-user", AuthIndex: "auth-1", Provider: "antigravity", Type: "antigravity", Priority: 100,
	}}
	if err := replaceGuardRosterFromFiles(engine, files); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ApplyQuotaEvidence(guard.QuotaEvidence{
		AuthIndex: "auth-1", AuthID: "auth-id-1", Group: guard.ModelGroupGemini,
		Remaining: 0, ResetAt: now.Add(5 * time.Hour), ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	files[0].Unavailable = true
	if err := replaceGuardRosterFromFiles(engine, files); err != nil {
		t.Fatal(err)
	}
	entry := entryFromRuntimeSnapshot(t, engine.Snapshot(now), "auth-1", guard.ModelGroupGemini)
	if entry.State != guard.StateOpen || !entry.RecoverAt.Equal(now.Add(5*time.Hour)) {
		t.Fatalf("unavailable roster refresh lost breaker state: %#v", entry)
	}
}

func TestOfficialSchedulerDelegatesNonAntigravityAndPreservesMixedProvider(t *testing.T) {
	runtime, clock, _ := newOfficialGuardRuntime(t, []guard.Identity{
		{AuthID: "auth-id-1", AuthIndex: "auth-1", IdentityFingerprint: "fp-1", Provider: "antigravity", Priority: 100},
	})
	nonAntigravity := schedulerPickRequest{
		Provider: "openai",
		Model:    "gpt-5",
		Candidates: []schedulerAuthCandidate{
			{ID: "openai-id", Provider: "openai", Priority: 100},
		},
	}
	raw, _ := json.Marshal(nonAntigravity)
	delegated, envelopeErr := decodeOfficialEnvelope[schedulerPickResponse](t, runtime.Handle(context.Background(), MethodSchedulerPick, raw))
	if envelopeErr != nil || delegated.Handled {
		t.Fatalf("non-Antigravity request must delegate: %#v error=%+v", delegated, envelopeErr)
	}

	sendUsage429(t, runtime, clock, "auth-id-1", "auth-1", "gemini-2.5-pro", `{"error":{"message":"quota exhausted"}}`, http.Header{"Retry-After": {"120"}})
	mixed, envelopeErr := schedulerPick(t, runtime, "gemini-2.5-pro", []schedulerAuthCandidate{
		{ID: "auth-id-1", Provider: "antigravity", Priority: 100},
		{ID: "other-id", Provider: "vertex", Priority: 100},
	}, "antigravity", "vertex")
	if envelopeErr != nil || !mixed.Handled || mixed.AuthID != "other-id" {
		t.Fatalf("mixed-provider pick = %#v error=%+v; want non-Antigravity candidate", mixed, envelopeErr)
	}
}

func TestOfficialRequiredSchedulerDelegatesConfiguredSelectorOutsideEnforcement(t *testing.T) {
	runtime := New(Options{
		StateCachePath: filepath.Join(t.TempDir(), "quota-cache.json"),
		GuardStatePath: filepath.Join(t.TempDir(), "guard-state.json"),
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	if _, err := runtime.Register(context.Background(), RegisterRequest{
		ConfigYAML: "mode: observe\nrequired-scheduler-for: [antigravity]\n",
		HostFeatures: []string{
			hostFeatureRequiredSchedulerV1,
			hostFeatureSchedulerRequestIDV1,
			hostFeatureSchedulerDirectResponseV1,
			hostFeatureAuthInventoryReadyV1,
		},
	}); err != nil {
		t.Fatal(err)
	}

	response, envelopeErr := schedulerPick(t, runtime, "gemini-2.5-pro", []schedulerAuthCandidate{{
		ID: "auth-a", Provider: "antigravity", Priority: 100,
	}}, "gemini")
	if envelopeErr != nil {
		t.Fatalf("scheduler.pick error = %+v", envelopeErr)
	}
	if !response.Handled || response.DelegateBuiltin != schedulerDelegateConfigured || response.AuthID != "" {
		t.Fatalf("observe required scheduler response = %+v, want configured selector delegation", response)
	}

	nonAntigravity, envelopeErr := schedulerPick(t, runtime, "gpt-5.4", []schedulerAuthCandidate{{
		ID: "codex-a", Provider: "codex", Priority: 100,
	}}, "codex")
	if envelopeErr != nil {
		t.Fatalf("non-Antigravity scheduler.pick error = %+v", envelopeErr)
	}
	if nonAntigravity.Handled || nonAntigravity.DelegateBuiltin != "" {
		t.Fatalf("non-Antigravity response = %+v, want unhandled", nonAntigravity)
	}
}

func TestSchedulerDoesNotMixObserveConfigWithEnforceEngineDuringReconfigure(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "guard.json")
	file := host.AuthFile{
		ID: "auth-id-1", Name: "account-1", AuthIndex: "auth-1", Provider: "antigravity", Type: "antigravity", Priority: 100,
	}
	identity := guard.Identity{
		AuthID: file.ID, AuthIndex: file.AuthIndex, IdentityFingerprint: guardIdentityFingerprint(file), Provider: "antigravity", Priority: 100,
	}
	engine, err := guard.New(guard.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ReplaceRoster([]guard.Identity{identity}); err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []guard.QuotaEvidence{
		{
			AuthID: identity.AuthID, AuthIndex: identity.AuthIndex, IdentityFingerprint: identity.IdentityFingerprint,
			Group: guard.ModelGroupGemini, Remaining: 0, ResetAt: now.Add(time.Hour), ObservedAt: now, Source: "quota_probe",
		},
		{
			AuthID: identity.AuthID, AuthIndex: identity.AuthIndex, IdentityFingerprint: identity.IdentityFingerprint,
			Group: guard.ModelGroupClaudeGPT, Remaining: 80, ResetAt: now.Add(time.Hour), ObservedAt: now, Source: "quota_probe",
		},
	} {
		if _, err := engine.ApplyQuotaEvidence(evidence); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Save(statePath); err != nil {
		t.Fatal(err)
	}

	hostCallbacks := &inventoryAwareTestHost{
		officialTestHost: &officialTestHost{files: []host.AuthFile{file}, documents: make(map[string]host.AuthDocument)},
		ready:            true,
	}
	runtime := New(Options{
		Clock:          &officialTestClock{now: now},
		Host:           hostCallbacks,
		StateCachePath: filepath.Join(t.TempDir(), "cache.json"),
		GuardStatePath: statePath,
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	features := []string{
		hostFeatureRequiredSchedulerV1,
		hostFeatureSchedulerRequestIDV1,
		hostFeatureSchedulerDirectResponseV1,
		hostFeatureAuthInventoryReadyV1,
	}
	if _, err := runtime.Register(context.Background(), RegisterRequest{
		ConfigYAML:   "mode: observe\nrequired-scheduler-for: [antigravity]\n",
		HostFeatures: features,
	}); err != nil {
		t.Fatal(err)
	}
	active, _ := runtime.guardRuntime.currentEngine()
	if _, err := active.ApplyQuotaEvidence(guard.QuotaEvidence{
		AuthID: identity.AuthID, AuthIndex: identity.AuthIndex, IdentityFingerprint: identity.IdentityFingerprint,
		Group: guard.ModelGroupGemini, Remaining: 0, ResetAt: now.Add(2 * time.Hour), ObservedAt: now.Add(time.Second), Source: "quota_probe",
	}); err != nil {
		t.Fatal(err)
	}
	if !active.IsDirty() {
		t.Fatal("test setup did not mark the observe generation dirty")
	}

	// Pause Reconfigure after it takes the generation write gate. A concurrent
	// scheduler request must not read the old observe config before waiting for
	// that gate and then combine it with the new enforce engine after the swap.
	runtime.guardRuntime.persistMu.Lock()
	reconfigureDone := make(chan error, 1)
	go func() {
		_, reconfigureErr := runtime.Reconfigure(context.Background(), ReconfigureRequest{
			ConfigYAML:   "mode: enforce\nrequired-scheduler-for: [antigravity]\n",
			HostFeatures: features,
		})
		reconfigureDone <- reconfigureErr
	}()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.guardRuntime.access.TryRLock() {
		runtime.guardRuntime.access.RUnlock()
		if time.Now().After(deadline) {
			runtime.guardRuntime.persistMu.Unlock()
			t.Fatal("Reconfigure did not acquire the guard generation gate")
		}
		time.Sleep(time.Millisecond)
	}

	entered := make(chan struct{}, 1)
	configRead := make(chan struct{}, 1)
	runtime.schedulerPickHook = func(stage string) {
		switch stage {
		case "entered":
			entered <- struct{}{}
		case "config_read":
			configRead <- struct{}{}
		}
	}
	t.Cleanup(func() { runtime.schedulerPickHook = nil })
	schedulerRaw, err := json.Marshal(schedulerPickRequest{
		RequestID: "scheduler-reconfigure-race",
		Provider:  "antigravity",
		Model:     "gemini-2.5-pro",
		Candidates: []schedulerAuthCandidate{{
			ID: file.ID, Provider: "antigravity", Priority: 100,
		}},
	})
	if err != nil {
		runtime.guardRuntime.persistMu.Unlock()
		t.Fatal(err)
	}
	schedulerDone := make(chan []byte, 1)
	go func() {
		schedulerDone <- runtime.Handle(context.Background(), MethodSchedulerPick, schedulerRaw)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		runtime.guardRuntime.persistMu.Unlock()
		t.Fatal("scheduler did not reach the handler while reconfigure held the generation gate")
	}
	select {
	case <-configRead:
		runtime.guardRuntime.persistMu.Unlock()
		t.Fatal("scheduler read runtime config before acquiring the generation read gate")
	case <-time.After(100 * time.Millisecond):
	}

	runtime.guardRuntime.persistMu.Unlock()
	if err := <-reconfigureDone; err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-schedulerDone:
		response, envelopeErr := decodeOfficialEnvelope[schedulerPickResponse](t, raw)
		if response.DelegateBuiltin == schedulerDelegateConfigured {
			t.Fatalf("scheduler mixed observe config with enforce engine: response=%+v error=%+v", response, envelopeErr)
		}
		if envelopeErr == nil || envelopeErr.Code != guard.ErrorCodeModelCooldown || envelopeErr.HTTPStatus != http.StatusTooManyRequests {
			t.Fatalf("scheduler result after enforce switch: response=%+v error=%+v, want local model_cooldown 429", response, envelopeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler remained blocked after reconfigure completed")
	}
}

func TestOfficialUnknownModelFailsClosedAfterAuth(t *testing.T) {
	runtime, _, _ := newOfficialGuardRuntime(t, []guard.Identity{
		{AuthID: "auth-id-1", AuthIndex: "auth-1", IdentityFingerprint: "fp-1", Provider: "antigravity", Priority: 100},
	})
	raw, _ := json.Marshal(requestInterceptRequest{ToFormat: "antigravity", Model: "future-model-without-group"})
	response, envelopeErr := decodeOfficialEnvelope[requestInterceptResponse](t, runtime.Handle(context.Background(), MethodRequestAfter, raw))
	if envelopeErr != nil {
		t.Fatal(envelopeErr)
	}
	if !response.Terminate || response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(response.ResponseBody), guard.ErrorCodeQuotaGroupUnknown) {
		t.Fatalf("unknown model after-auth response = %#v body=%s", response, response.ResponseBody)
	}
}

func TestOfficialManagementAndRemovedFiltersNeverSaveAuth(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	clock := &officialTestClock{now: now}
	hostCallbacks := &officialTestHost{
		files: []host.AuthFile{{
			ID: "auth-id-1", Name: "antigravity-user", AuthIndex: "auth-1", Provider: "antigravity", Type: "antigravity", Priority: 100,
		}},
		documents: map[string]host.AuthDocument{
			"auth-1": {AuthIndex: "auth-1", JSON: json.RawMessage(`{"access_token":"test-token","project_id":"test-project"}`)},
		},
	}
	runtime := New(Options{
		Clock:          clock,
		Host:           hostCallbacks,
		StateCachePath: filepath.Join(t.TempDir(), "quota-cache.json"),
		GuardStatePath: filepath.Join(t.TempDir(), "guard-state.json"),
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	if _, err := runtime.Register(context.Background(), RegisterRequest{ConfigYAML: "enabled: true\nmode: observe\n", SchemaVersion: 3}); err != nil {
		t.Fatal(err)
	}

	requests := []ManagementRequest{
		{Method: http.MethodGet, Path: "/v0/management/cpa-antigravity-quota-guard/status"},
		{Method: http.MethodGet, Path: "/v0/management/cpa-antigravity-quota-guard/config"},
		{Method: http.MethodPost, Path: "/v0/management/cpa-antigravity-quota-guard/actions/probe"},
	}
	for _, request := range requests {
		raw, _ := json.Marshal(request)
		response, envelopeErr := decodeOfficialEnvelope[ManagementResponse](t, runtime.Handle(context.Background(), MethodManagementHandle, raw))
		if envelopeErr != nil || response.StatusCode != http.StatusOK {
			body, _ := base64.StdEncoding.DecodeString(response.Body)
			t.Fatalf("management %s %s failed: status=%d error=%+v body=%s", request.Method, request.Path, response.StatusCode, envelopeErr, body)
		}
	}

	for _, method := range []string{MethodFilterResponse, MethodFilterComplete, MethodFilterError, MethodFilterOutbound, MethodFilterInbound} {
		var envelope Envelope
		if err := json.Unmarshal(runtime.Handle(context.Background(), method, []byte(`{}`)), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.OK || envelope.Error == nil || envelope.Error.Code != "invalid_request" {
			t.Fatalf("removed method %s remained reachable: %#v", method, envelope)
		}
	}
	if hostCallbacks.saves() != 0 {
		t.Fatalf("official management/filter paths called host.auth.save %d times", hostCallbacks.saves())
	}
}
