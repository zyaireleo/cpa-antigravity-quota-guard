package runtime_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/core"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/guard"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/host"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/runtime"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/state"
)

type mockHost struct {
	mu            sync.Mutex
	files         []host.AuthFile
	listResponses [][]host.AuthFile
	authDocs      map[string]host.AuthDocument
	saveCalls     int
	httpResponse  host.HTTPResponse
	httpErr       error
	httpCalls     []host.HTTPRequest
	operations    []string
	afterHTTP     func(*mockHost)
}

func newMockHost() *mockHost {
	return &mockHost{
		authDocs: make(map[string]host.AuthDocument),
		httpResponse: host.HTTPResponse{
			StatusCode: http.StatusOK,
			Body: []byte(`{
				"models": {
					"gemini-2.5-pro": {
						"quotaInfo": {
							"windows": [{
								"name": "5h",
								"remainingFraction": 0.8,
								"resetTime": "2026-08-18T17:00:00Z"
							}]
						}
					},
					"gemini-2.5-flash": {
						"quotaInfo": {
							"windows": [{
								"name": "7d",
								"remainingFraction": 0.9,
								"resetTime": "2026-08-25T12:00:00Z"
							}]
						}
					}
				}
			}`),
		},
	}
}

func (m *mockHost) ListAuthFiles(ctx context.Context) ([]host.AuthFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.operations = append(m.operations, "host-sync")
	if len(m.listResponses) > 0 {
		files := m.listResponses[0]
		m.listResponses = m.listResponses[1:]
		return append([]host.AuthFile(nil), files...), nil
	}
	return append([]host.AuthFile(nil), m.files...), nil
}

func (m *mockHost) GetAuth(ctx context.Context, authIndex string) (host.AuthDocument, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if doc, ok := m.authDocs[authIndex]; ok {
		return doc, nil
	}
	return host.AuthDocument{
		AuthIndex: authIndex,
		JSON:      json.RawMessage(`{"access_token":"mock_token_123","project_id":"mock-project","account":"test@example.com"}`),
	}, nil
}

func (m *mockHost) GetRuntime(ctx context.Context, authIndex string) (host.RuntimeAuth, error) {
	return host.RuntimeAuth{AuthIndex: authIndex}, nil
}

func (m *mockHost) SaveAuth(ctx context.Context, name string, doc json.RawMessage) error {
	m.mu.Lock()
	m.saveCalls++
	m.mu.Unlock()
	return nil
}

func (m *mockHost) HTTPDo(ctx context.Context, req host.HTTPRequest) (host.HTTPResponse, error) {
	m.mu.Lock()
	m.httpCalls = append(m.httpCalls, req)
	m.operations = append(m.operations, "google-probe")
	hook := m.afterHTTP
	m.afterHTTP = nil
	m.mu.Unlock()
	if hook != nil {
		hook(m)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.httpErr != nil {
		return host.HTTPResponse{}, m.httpErr
	}
	return m.httpResponse, nil
}

type testClock struct {
	now time.Time
}

func (t *testClock) Now() time.Time {
	return t.now
}

type testSleeper struct{}

func (testSleeper) Sleep(ctx context.Context, d time.Duration) error {
	return nil
}

type mockTicker struct {
	c    chan time.Time
	stop bool
}

func (m *mockTicker) Chan() <-chan time.Time {
	return m.c
}

func (m *mockTicker) Stop() {
	m.stop = true
}

type mockTickerFactory struct {
	lastTicker *mockTicker
}

func newTestRuntime(t *testing.T, options runtime.Options) *runtime.Runtime {
	t.Helper()
	if options.StateCachePath == "" {
		options.StateCachePath = filepath.Join(t.TempDir(), "startup-cache.json")
	}
	r := runtime.New(options)
	t.Cleanup(func() {
		_ = r.Shutdown(context.Background())
	})
	return r
}

func (m *mockTickerFactory) NewTicker(interval time.Duration) runtime.Ticker {
	t := &mockTicker{c: make(chan time.Time, 1)}
	m.lastTicker = t
	return t
}

func TestRuntime_Handle_Register(t *testing.T) {
	r := newTestRuntime(t, runtime.Options{GuardStatePath: filepath.Join(t.TempDir(), "guard.json")})
	req, err := json.Marshal(runtime.RegisterRequest{
		ConfigYAML:    "enabled: true\nmode: observe\n",
		SchemaVersion: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	respBytes := r.Handle(context.Background(), runtime.MethodPluginRegister, req)

	var envelope struct {
		OK     bool                   `json:"ok"`
		Result runtime.RegisterResult `json:"result"`
		Error  *runtime.EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		t.Fatalf("unmarshal response envelope failed: %v", err)
	}
	if !envelope.OK {
		t.Fatalf("expected OK=true, got error: %+v", envelope.Error)
	}
	if envelope.Result.SchemaVersion != 3 {
		t.Errorf("expected schema_version=3, got %d", envelope.Result.SchemaVersion)
	}
	if envelope.Result.Metadata.Name != "CPA Antigravity Quota Guard" {
		t.Errorf("unexpected metadata name %q", envelope.Result.Metadata.Name)
	}
	for _, capability := range []string{"management_api", "scheduler", "usage_plugin", "request_interceptor"} {
		if !envelope.Result.Capabilities[capability] {
			t.Errorf("expected %s capability", capability)
		}
	}
	for _, unsupported := range []string{runtime.MethodFilterResponse, runtime.MethodFilterComplete, runtime.MethodFilterError} {
		if envelope.Result.Capabilities[unsupported] {
			t.Errorf("unsupported capability %s must not be registered", unsupported)
		}
	}
}

func TestRuntime_Handle_Reconfigure(t *testing.T) {
	r := newTestRuntime(t, runtime.Options{})
	req := []byte(`{"config_yaml":"enabled: false\nantigravity_model_group: claude_gpt\n"}`)

	respBytes := r.Handle(context.Background(), "plugin.reconfigure", req)

	var envelope struct {
		OK     bool                   `json:"ok"`
		Result runtime.RegisterResult `json:"result"`
		Error  *runtime.EnvelopeError `json:"error"`
	}

	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		t.Fatalf("unmarshal response envelope failed: %v", err)
	}

	if !envelope.OK {
		t.Fatalf("expected OK=true, got error: %+v", envelope.Error)
	}
}

func TestRuntime_Handle_Shutdown(t *testing.T) {
	r := newTestRuntime(t, runtime.Options{})

	respBytes := r.Handle(context.Background(), "plugin.shutdown", nil)

	var envelope struct {
		OK     bool                   `json:"ok"`
		Result map[string]any         `json:"result"`
		Error  *runtime.EnvelopeError `json:"error"`
	}

	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		t.Fatalf("unmarshal response envelope failed: %v", err)
	}

	if !envelope.OK {
		t.Fatalf("expected OK=true, got error: %+v", envelope.Error)
	}

	afterShutdownResp := r.Handle(context.Background(), "plugin.register", nil)
	var afterEnvelope struct {
		OK    bool                   `json:"ok"`
		Error *runtime.EnvelopeError `json:"error"`
	}
	_ = json.Unmarshal(afterShutdownResp, &afterEnvelope)
	if afterEnvelope.OK {
		t.Errorf("expected failure after shutdown, got OK=true")
	}
	if afterEnvelope.Error == nil || afterEnvelope.Error.Code != "shutdown" {
		t.Errorf("expected error code 'shutdown', got %+v", afterEnvelope.Error)
	}
}

func TestRuntime_Handle_UnknownMethod(t *testing.T) {
	r := newTestRuntime(t, runtime.Options{})
	respBytes := r.Handle(context.Background(), "unknown.method", nil)

	var envelope struct {
		OK    bool                   `json:"ok"`
		Error *runtime.EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		t.Fatalf("unmarshal response envelope failed: %v", err)
	}
	if envelope.OK {
		t.Errorf("expected OK=false for unknown method")
	}
	if envelope.Error == nil || envelope.Error.Code != "invalid_request" {
		t.Errorf("expected error code 'invalid_request', got %+v", envelope.Error)
	}
}

func TestRuntime_Handle_InvalidConfig(t *testing.T) {
	r := newTestRuntime(t, runtime.Options{})
	// Only structurally unparseable input should produce hard errors
	req := []byte(`{"config_yaml":"{invalid-json"}`)

	respBytes := r.Handle(context.Background(), "plugin.register", req)

	var envelope struct {
		OK    bool                   `json:"ok"`
		Error *runtime.EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		t.Fatalf("unmarshal response envelope failed: %v", err)
	}
	if envelope.OK {
		t.Errorf("expected OK=false for invalid config")
	}
	if envelope.Error == nil || envelope.Error.Code != "invalid_config" {
		t.Errorf("expected error code 'invalid_config', got %+v", envelope.Error)
	}
}

func TestRuntime_Handle_Diagnostics(t *testing.T) {
	r := newTestRuntime(t, runtime.Options{})
	req := []byte(`{"config_yaml":"enabled: true\n"}`)

	respBytes := r.Handle(context.Background(), "plugin.register", req)

	var envelope struct {
		OK     bool                   `json:"ok"`
		Result runtime.RegisterResult `json:"result"`
		Error  *runtime.EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		t.Fatalf("unmarshal response envelope failed: %v", err)
	}
	if !envelope.OK {
		t.Fatalf("expected OK=true, got error: %+v", envelope.Error)
	}

	// Diagnostics should surface runtime status
	diag, err := r.Diagnostics(context.Background())
	if err != nil {
		t.Fatalf("diagnostics failed: %v", err)
	}
	mgmt, ok := diag["management_api"].(map[string]any)
	if !ok || mgmt["status"] != "ready" {
		t.Errorf("expected management_api status ready, got %+v", diag["management_api"])
	}
}

func TestRuntime_Handle_ManagementRegister(t *testing.T) {
	r := newTestRuntime(t, runtime.Options{GuardStatePath: filepath.Join(t.TempDir(), "guard.json")})
	respBytes := r.Handle(context.Background(), runtime.MethodManagementRegister, []byte(`{"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/cpa-antigravity-quota-guard"}`))

	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Routes []struct {
				Method string `json:"Method"`
				Path   string `json:"Path"`
			} `json:"routes"`
			Resources []struct {
				Path string `json:"Path"`
				Menu string `json:"Menu"`
			} `json:"resources"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if !envelope.OK {
		t.Fatalf("expected OK=true")
	}
	want := map[string]bool{
		"cpa-antigravity-quota-guard/status":            false,
		"cpa-antigravity-quota-guard/config":            false,
		"cpa-antigravity-quota-guard/actions/probe":     false,
		"cpa-antigravity-quota-guard/actions/half-open": false,
	}
	for _, route := range envelope.Result.Routes {
		if _, ok := want[route.Path]; ok {
			want[route.Path] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("missing management route %q", path)
		}
	}
	if len(envelope.Result.Resources) != 1 || envelope.Result.Resources[0].Path != "/status" {
		t.Fatalf("unexpected resources: %#v", envelope.Result.Resources)
	}
}

func TestRuntime_Handle_ManagementHandle(t *testing.T) {
	mock := newMockHost()
	mock.files = []host.AuthFile{{ID: "auth-id-1", Name: "test-auth", AuthIndex: "auth_1", Provider: "antigravity", Type: "antigravity", Priority: 100}}
	clock := &testClock{now: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)}
	r := newTestRuntime(t, runtime.Options{
		Host: mock, Clock: clock, Sleeper: testSleeper{},
		StateCachePath: filepath.Join(t.TempDir(), "quota.json"),
		GuardStatePath: filepath.Join(t.TempDir(), "guard.json"),
	})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{ConfigYAML: "mode: observe\n", SchemaVersion: 3}); err != nil {
		t.Fatal(err)
	}

	for _, request := range []map[string]any{
		{"Method": "GET", "Path": "/v0/management/cpa-antigravity-quota-guard/status"},
		{"Method": "POST", "Path": "/v0/management/cpa-antigravity-quota-guard/actions/probe"},
		{"Method": "GET", "Path": "/v0/resource/plugins/cpa-antigravity-quota-guard/status"},
	} {
		raw, _ := json.Marshal(request)
		response := r.Handle(context.Background(), runtime.MethodManagementHandle, raw)
		var envelope struct {
			OK     bool                       `json:"ok"`
			Result runtime.ManagementResponse `json:"result"`
		}
		if err := json.Unmarshal(response, &envelope); err != nil || !envelope.OK || envelope.Result.StatusCode != http.StatusOK {
			t.Fatalf("management request %#v failed: %s", request, response)
		}
	}
	if mock.saveCalls != 0 {
		t.Fatalf("guard management path wrote auth files %d times", mock.saveCalls)
	}
}

func TestRuntime_SingleFlight_Conflict(t *testing.T) {
	blockChan := make(chan struct{})
	enteredChan := make(chan struct{})

	r := newTestRuntime(t, runtime.Options{
		Runner: func(ctx context.Context, request runtime.TaskRequest) error {
			close(enteredChan)
			<-blockChan
			return nil
		},
	})

	go func() {
		_ = r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil)
	}()

	<-enteredChan

	if err := r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil); !errors.Is(err, runtime.ErrRunInProgress) {
		t.Errorf("expected ErrRunInProgress on Probe, got %v", err)
	}
	if err := r.ManualApply(context.Background(), config.AntigravityModelGroupGemini, nil); !errors.Is(err, runtime.ErrLegacyMutationDisabled) {
		t.Errorf("expected ErrLegacyMutationDisabled on ManualApply, got %v", err)
	}
	if err := r.AutoApply(context.Background()); !errors.Is(err, runtime.ErrLegacyMutationDisabled) {
		t.Errorf("expected ErrLegacyMutationDisabled on AutoApply, got %v", err)
	}

	close(blockChan)
}

func TestRuntime_ProductionRunner_ConcurrentProbes(t *testing.T) {
	mock := newMockHost()
	mock.files = []host.AuthFile{
		{
			Name:      "test-account-1",
			AuthIndex: "auth_1",
			Provider:  string(core.ProviderAntigravity),
			Type:      string(core.CredentialTypeAntigravity),
			Priority:  100,
		},
		{
			Name:      "test-account-2",
			AuthIndex: "auth_2",
			Provider:  string(core.ProviderAntigravity),
			Type:      string(core.CredentialTypeAntigravity),
			Priority:  90,
		},
		{
			Name:      "test-account-3",
			AuthIndex: "auth_3",
			Provider:  string(core.ProviderAntigravity),
			Type:      string(core.CredentialTypeAntigravity),
			Priority:  80,
		},
	}

	clock := &testClock{now: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)}
	r := newTestRuntime(t, runtime.Options{
		Host:    mock,
		Clock:   clock,
		Sleeper: testSleeper{},
	})

	err := r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	snap, err := r.LatestSnapshot(context.Background())
	if err != nil {
		t.Fatalf("latest snapshot failed: %v", err)
	}
	if len(snap.Groups[snap.ActiveModelGroup].Items) != 3 {
		t.Errorf("expected 3 total items, got %d", len(snap.Groups[snap.ActiveModelGroup].Items))
	}
}

func TestRuntime_ProbeProjectsBothGroupsFromOneQuotaResponse(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	mock := newMockHost()
	mock.files = []host.AuthFile{{
		Name:      "dual-group-account",
		AuthIndex: "dual-group-auth",
		Provider:  string(core.ProviderAntigravity),
		Type:      string(core.CredentialTypeAntigravity),
		Priority:  100,
	}}
	mock.httpResponse.Body = []byte(`{
		"models": {
			"gemini-2.0-flash": {"modelProvider":"google","quotaInfo":{"windows":[
				{"name":"5h","remainingFraction":0.85,"resetTime":"2026-08-22T15:00:00Z"},
				{"name":"weekly","remainingFraction":0.70,"resetTime":"2026-08-29T00:00:00Z"}
			]}},
			"claude-3-5-sonnet": {"modelProvider":"anthropic","quotaInfo":{"windows":[
				{"name":"5hr","remainingFraction":0.40,"resetTime":"2026-08-22T16:00:00Z"},
				{"name":"7d","remainingFraction":0.60,"resetTime":"2026-08-29T00:00:00Z"}
			]}}
		}
	}`)
	r := newTestRuntime(t, runtime.Options{Host: mock, Clock: &testClock{now: time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)}, Sleeper: testSleeper{}, StateCachePath: cachePath})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := r.LatestSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gemini := snapshot.Groups[string(config.AntigravityModelGroupGemini)]
	claudeGPT := snapshot.Groups[string(config.AntigravityModelGroupClaudeGPT)]
	if len(mock.httpCalls) != 1 {
		t.Fatalf("quota HTTP calls = %d, want one response for both groups", len(mock.httpCalls))
	}
	if len(gemini.Items) != 1 || !gemini.Items[0].EvidenceFresh || gemini.Items[0].R7d != 0.7 {
		t.Fatalf("gemini projection = %#v, want fresh quota data", gemini)
	}
	if len(claudeGPT.Items) != 1 || !claudeGPT.Items[0].EvidenceFresh || claudeGPT.Items[0].R7d != 0.6 || !claudeGPT.Items[0].IsPredicted {
		t.Fatalf("claude/gpt projection = %#v, want fresh predicted quota data", claudeGPT)
	}
	if mock.saveCalls != 0 {
		t.Fatalf("Probe wrote Host documents: %d saves", mock.saveCalls)
	}
}

func TestRuntime_ProbeReconcilesHostChangesBeforePlanning(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	mock := newMockHost()
	mock.files = []host.AuthFile{{Name: "deleted", AuthIndex: "auth-deleted", Provider: "antigravity", Priority: 50}}
	mock.afterHTTP = func(m *mockHost) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.files = []host.AuthFile{{Name: "added", AuthIndex: "auth-added", Provider: "antigravity", Priority: 77}}
	}
	r := newTestRuntime(t, runtime.Options{Host: mock, Clock: &testClock{now: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}, Sleeper: testSleeper{}, StateCachePath: cachePath})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := r.LatestSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	items := snapshot.Groups["gemini"].Items
	if len(items) != 1 || items[0].AuthIndex != "au***ed" || items[0].Target.Priority != 77 {
		t.Fatalf("post-probe snapshot items = %#v; want newly added credential unchanged", items)
	}
}

func TestRuntime_ProbeDoesNotApplyOldQuotaAfterSameIndexIdentityReplacement(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	mock := newMockHost()
	mock.files = []host.AuthFile{{ID: "old-auth-id", Name: "account", AuthIndex: "auth-shared", Provider: "antigravity", Priority: 100}}
	mock.afterHTTP = func(m *mockHost) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.files = []host.AuthFile{{ID: "new-auth-id", Name: "account", AuthIndex: "auth-shared", Provider: "antigravity", Priority: 100}}
	}
	r := newTestRuntime(t, runtime.Options{
		Host:           mock,
		Clock:          &testClock{now: now},
		Sleeper:        testSleeper{},
		StateCachePath: filepath.Join(t.TempDir(), "cache.json"),
		GuardStatePath: filepath.Join(t.TempDir(), "guard.json"),
	})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil); err != nil {
		t.Fatal(err)
	}

	request, _ := json.Marshal(runtime.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/cpa-antigravity-quota-guard/status",
	})
	var envelope runtime.Envelope
	if err := json.Unmarshal(r.Handle(context.Background(), runtime.MethodManagementHandle, request), &envelope); err != nil || !envelope.OK {
		t.Fatalf("management envelope = %#v error=%v", envelope, err)
	}
	var response runtime.ManagementResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatal(err)
	}
	body, err := base64.StdEncoding.DecodeString(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Snapshot guard.Snapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, entry := range payload.Snapshot.Entries {
		if entry.AuthIndex == "auth-shared" && entry.ModelGroup == guard.ModelGroupGemini {
			if entry.AuthID != "new-auth-id" || entry.State != guard.StateUninitialized || entry.RemainingPercent != nil {
				t.Fatalf("old quota crossed identity replacement: %#v", entry)
			}
			return
		}
	}
	t.Fatal("replacement guard entry not found")
}

func TestRuntime_ProbeUsesPostProbePriorityAndDisabledState(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	mock := newMockHost()
	mock.files = []host.AuthFile{{Name: "changed", AuthIndex: "auth-changed", Provider: "antigravity", Priority: 50}}
	mock.afterHTTP = func(m *mockHost) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.files[0].Priority = 77
		m.files[0].Disabled = true
	}
	r := newTestRuntime(t, runtime.Options{Host: mock, Clock: &testClock{now: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}, Sleeper: testSleeper{}, StateCachePath: cachePath})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil); err != nil {
		t.Fatal(err)
	}

	snapshot, err := r.LatestSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	items := snapshot.Groups["gemini"].Items
	if len(items) != 1 || items[0].Current.Priority != 77 || !items[0].Current.Disabled {
		t.Fatalf("post-probe current state = %#v; want priority=77 disabled=true", items)
	}
	if mock.saveCalls != 0 {
		t.Fatalf("Probe must not replace Host documents: %d saves", mock.saveCalls)
	}
}

func TestRuntime_ProbeStillPerformsSecondHostSyncWhenInitialInventoryIsEmpty(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	mock := newMockHost()
	mock.listResponses = [][]host.AuthFile{
		{},
		{{Name: "late-addition", AuthIndex: "auth-added", Provider: "antigravity", Priority: 77}},
	}
	r := newTestRuntime(t, runtime.Options{Host: mock, Clock: &testClock{now: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}, Sleeper: testSleeper{}, StateCachePath: cachePath})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil); err != nil {
		t.Fatal(err)
	}

	snapshot, err := r.LatestSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	items := snapshot.Groups["gemini"].Items
	if len(items) != 1 || items[0].AuthIndex != "au***ed" || items[0].Target.Priority != 77 {
		t.Fatalf("post-probe snapshot items = %#v; want late addition unchanged", items)
	}
	if got := strings.Join(mock.operations, ","); got != "host-sync,host-sync" {
		t.Fatalf("operation order = %s; want two host syncs even with empty initial inventory", got)
	}
}

func TestRuntime_ProductionRunner_UsesHostJSONAndIgnoresPhysicalPath(t *testing.T) {
	tempDir := t.TempDir()
	jsonFilePath := filepath.Join(tempDir, "auth.json")
	_ = os.WriteFile(jsonFilePath, []byte(`{"access_token":"from_file_token","project_id":"file_proj"}`), 0o600)

	mock := newMockHost()
	mock.files = []host.AuthFile{
		{
			Name:      "test-file-auth",
			AuthIndex: "auth_file_1",
			Provider:  string(core.ProviderAntigravity),
			Type:      string(core.CredentialTypeAntigravity),
			Priority:  100,
		},
	}
	mock.authDocs["auth_file_1"] = host.AuthDocument{
		AuthIndex: "auth_file_1",
		Path:      jsonFilePath,
		JSON:      json.RawMessage(`{"access_token":"from_host_token","project_id":"host_proj"}`),
	}

	r := newTestRuntime(t, runtime.Options{
		Host:    mock,
		Clock:   &testClock{now: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)},
		Sleeper: testSleeper{},
	})

	err := r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil)
	if err != nil {
		t.Fatalf("probe with host JSON failed: %v", err)
	}
	if len(mock.httpCalls) == 0 || mock.httpCalls[0].Headers.Get("Authorization") != "Bearer from_host_token" {
		t.Fatalf("quota probe did not use host.auth.get JSON: %#v", mock.httpCalls)
	}
}

func TestRuntime_DiagnosticsWithProbeOnlyHistoryHasNoLatestApply(t *testing.T) {
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDir) })

	cachePath := filepath.Join(t.TempDir(), "cache.json")
	mock := newMockHost()
	mock.files = []host.AuthFile{{Name: "probe-only", AuthIndex: "auth-probe", Provider: "antigravity", Priority: 100}}
	r := newTestRuntime(t, runtime.Options{Host: mock, Clock: &testClock{now: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}, Sleeper: testSleeper{}, StateCachePath: cachePath})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil); err != nil {
		t.Fatal(err)
	}
	diagnostics, _ := r.Diagnostics(context.Background())
	latest, ok := diagnostics["latest_apply"].(*runtime.RunHistoryEntry)
	if !ok || latest != nil {
		t.Fatalf("probe-only latest_apply = %#v; want typed nil", diagnostics["latest_apply"])
	}
}

func TestRuntime_SyncHostPreservesConfiguredControlGroup(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	mock := newMockHost()
	mock.files = []host.AuthFile{{Name: "control", AuthIndex: "auth-control", Provider: "antigravity", Priority: 100}}
	r := newTestRuntime(t, runtime.Options{Host: mock, Clock: &testClock{now: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}, StateCachePath: cachePath})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{}); err != nil {
		t.Fatal(err)
	}
	httpCallsBefore := len(mock.httpCalls)
	snapshot, err := r.SyncHost(context.Background(), config.AntigravityModelGroupClaudeGPT)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveModelGroup != "gemini" {
		t.Fatalf("active_model_group = %q; want configured gemini", snapshot.ActiveModelGroup)
	}
	if len(mock.httpCalls) != httpCallsBefore {
		t.Fatal("overview synchronization unexpectedly called Google")
	}
}

func TestRuntime_SyncHostUsesUpdatedDynamicControlGroup(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	mock := newMockHost()
	mock.files = []host.AuthFile{{Name: "control", AuthIndex: "auth-control", Provider: "antigravity", Priority: 100}}
	r := newTestRuntime(t, runtime.Options{Host: mock, Clock: &testClock{now: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}, StateCachePath: cachePath})
	if _, err := r.Register(context.Background(), runtime.RegisterRequest{}); err != nil {
		t.Fatal(err)
	}
	dynamic, err := r.GetDynamicConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dynamic.AntigravityModelGroup = string(config.AntigravityModelGroupClaudeGPT)
	if err := r.SetDynamicConfig(context.Background(), dynamic); err != nil {
		t.Fatal(err)
	}

	snapshot, err := r.SyncHost(context.Background(), config.AntigravityModelGroupGemini)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveModelGroup != string(config.AntigravityModelGroupClaudeGPT) {
		t.Fatalf("active_model_group = %q, want updated Dynamic Config control", snapshot.ActiveModelGroup)
	}
	if len(snapshot.Groups[string(config.AntigravityModelGroupGemini)].Items) != 1 || len(snapshot.Groups[string(config.AntigravityModelGroupClaudeGPT)].Items) != 1 {
		t.Fatalf("updated control projection = %#v, want both canonical groups", snapshot.Groups)
	}
	if mock.saveCalls != 0 {
		t.Fatalf("SyncHost or config change wrote Host documents: %d saves", mock.saveCalls)
	}
}

func TestRuntime_ProductionRunner_ProbeFailure(t *testing.T) {
	mock := newMockHost()
	mock.files = []host.AuthFile{
		{
			Name:      "test-account-fail",
			AuthIndex: "auth_fail_1",
			Provider:  string(core.ProviderAntigravity),
			Type:      string(core.CredentialTypeAntigravity),
			Priority:  100,
			Disabled:  false,
		},
	}
	mock.httpResponse = host.HTTPResponse{
		StatusCode: http.StatusUnauthorized,
		Body:       []byte(`{"error": "unauthorized"}`),
	}

	r := newTestRuntime(t, runtime.Options{
		Host:    mock,
		Clock:   &testClock{now: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)},
		Sleeper: testSleeper{},
	})

	err := r.Probe(context.Background(), config.AntigravityModelGroupGemini, nil)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	snap, err := r.LatestSnapshot(context.Background())
	if err != nil {
		t.Fatalf("latest snapshot failed: %v", err)
	}

	activeGroup := snap.Groups[snap.ActiveModelGroup]
	if len(activeGroup.Items) != 1 {
		t.Fatalf("expected 1 item in snapshot, got %d", len(activeGroup.Items))
	}
	if activeGroup.Items[0].Target.Disabled {
		t.Errorf("failing probe must preserve the Host disabled state")
	}
	if activeGroup.Items[0].Target.Priority != 100 || !strings.Contains(activeGroup.Items[0].Reason, "probe failed") {
		t.Errorf("failing probe changed the Host target or lost its diagnostic: %#v", activeGroup.Items[0])
	}
}

func TestRuntime_RejectsLegacyAutoApplyWithoutStartingWorker(t *testing.T) {
	mockFactory := &mockTickerFactory{}
	r := newTestRuntime(t, runtime.Options{
		TickerFactory: mockFactory,
		Clock:         &testClock{now: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)},
		Sleeper:       testSleeper{},
	})

	// Legacy auto_apply must be rejected before any ticker can start.
	req := []byte(`{"config_yaml":"enabled: true\nauto_apply: true\ninterval: 10m\n"}`)
	respBytes := r.Handle(context.Background(), "plugin.register", req)
	var env struct {
		OK    bool                   `json:"ok"`
		Error *runtime.EnvelopeError `json:"error"`
	}
	_ = json.Unmarshal(respBytes, &env)
	if env.OK || env.Error == nil || !strings.Contains(env.Error.Message, "auto_apply") || !strings.Contains(env.Error.Message, "no longer supported") {
		t.Fatalf("unexpected register response: %s", respBytes)
	}
	if mockFactory.lastTicker != nil {
		t.Fatal("legacy auto_apply started a ticker")
	}
}

func TestRuntime_Diagnostics_And_Status(t *testing.T) {
	r := newTestRuntime(t, runtime.Options{})

	status, err := r.Status(context.Background())
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if status.LatestAudit == "" {
		t.Errorf("expected non-empty latest audit")
	}

	diag, err := r.Diagnostics(context.Background())
	if err != nil {
		t.Fatalf("diagnostics failed: %v", err)
	}
	mgmtAPI, ok := diag["management_api"].(map[string]any)
	if !ok || mgmtAPI["status"] != "ready" {
		t.Errorf("expected management_api status 'ready', got %+v", diag["management_api"])
	}
}

func TestRuntime_GetSetScheduleConfig(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	r := newTestRuntime(t, runtime.Options{StateCachePath: cachePath})

	_, err := r.Register(context.Background(), runtime.RegisterRequest{
		ConfigYAML: "",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Default schedule config should be zero-value
	cfg, err := r.GetScheduleConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Paused || cfg.WindowEnabled {
		t.Errorf("expected zero schedule config, got %+v", cfg)
	}

	// Set and read back
	if err := r.SetScheduleConfig(context.Background(), state.ScheduleConfig{
		Paused:        true,
		WindowEnabled: true,
		WindowStart:   "08:30",
		WindowEnd:     "23:30",
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err = r.GetScheduleConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Paused {
		t.Error("expected Paused=true")
	}
	if !cfg.WindowEnabled {
		t.Error("expected WindowEnabled=true")
	}
	if cfg.WindowStart != "08:30" || cfg.WindowEnd != "23:30" {
		t.Errorf("unexpected window: %q-%q", cfg.WindowStart, cfg.WindowEnd)
	}

	// Verify persisted to disk and survives reload
	store, err := state.Load(context.Background(), cachePath)
	if err != nil {
		t.Fatal(err)
	}
	diskCfg := store.GetScheduleConfig()
	if !diskCfg.Paused || diskCfg.WindowStart != "08:30" {
		t.Errorf("expected disk config to match, got %+v", diskCfg)
	}

	// Verify schedule config shows in diagnostics
	diag, err := r.Diagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sched, ok := diag["scheduler"].(map[string]any)
	if !ok {
		t.Fatal("expected scheduler in diagnostics")
	}
	if sched["paused"] != true {
		t.Errorf("expected paused=true in diagnostics, got %v", sched["paused"])
	}
	if sched["window_start"] != "08:30" {
		t.Errorf("expected window_start=08:30 in diagnostics, got %v", sched["window_start"])
	}
}

func TestRuntime_GetSetDynamicConfig(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	r := newTestRuntime(t, runtime.Options{StateCachePath: cachePath})

	_, err := r.Register(context.Background(), runtime.RegisterRequest{
		ConfigYAML: "",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Read default dynamic config
	dynCfg, err := r.GetDynamicConfig(context.Background())
	if err != nil {
		t.Fatalf("get dynamic config failed: %v", err)
	}
	if dynCfg.Interval != "15m0s" {
		t.Errorf("expected default Interval=15m0s, got %q", dynCfg.Interval)
	}
	if dynCfg.AntigravityModelGroup != "gemini" {
		t.Errorf("expected default model group=gemini, got %q", dynCfg.AntigravityModelGroup)
	}
	if dynCfg.QuotaSampleCapacity != 6 {
		t.Errorf("expected default QuotaSampleCapacity=6, got %d", dynCfg.QuotaSampleCapacity)
	}

	// 2. Set updated dynamic config
	update := state.DynamicConfig{
		AutoApply:                false,
		Interval:                 "30m",
		AntigravityModelGroup:    "claude_gpt",
		MaxConcurrency:           8,
		MinChange:                3,
		UrgencyTolerance:         0.08,
		RateLimitCooldownMinutes: 5,
		QuotaSampleCapacity:      10,
		PriorityRules: state.PriorityRulesConfig{
			BoostStartPriority:  990,
			NormalStartPriority: 120,
		},
		Schedule: state.ScheduleConfig{
			Paused:        false,
			WindowEnabled: true,
			WindowStart:   "08:00",
			WindowEnd:     "22:00",
		},
	}
	if err := r.SetDynamicConfig(context.Background(), update); err != nil {
		t.Fatalf("set dynamic config failed: %v", err)
	}

	// 3. Verify in-memory config updated
	runtimeCfg, err := r.Config()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeCfg.AutoApply {
		t.Error("expected AutoApply=false")
	}
	if runtimeCfg.Interval != 30*time.Minute {
		t.Errorf("expected Interval=30m, got %v", runtimeCfg.Interval)
	}
	if runtimeCfg.AntigravityModelGroup != config.AntigravityModelGroupClaudeGPT {
		t.Errorf("expected claude_gpt, got %v", runtimeCfg.AntigravityModelGroup)
	}
	if runtimeCfg.MaxConcurrency != 8 {
		t.Errorf("expected MaxConcurrency=8, got %d", runtimeCfg.MaxConcurrency)
	}
	if runtimeCfg.QuotaSampleCapacity != 10 {
		t.Errorf("expected QuotaSampleCapacity=10, got %d", runtimeCfg.QuotaSampleCapacity)
	}

	// 4. Verify disk persistence and survival across reloads
	store, err := state.Load(context.Background(), cachePath)
	if err != nil {
		t.Fatal(err)
	}
	diskDyn, ok := store.GetDynamicConfig()
	if !ok {
		t.Fatal("expected dynamic config on disk")
	}
	if diskDyn.Interval != "30m0s" || diskDyn.AntigravityModelGroup != "claude_gpt" || diskDyn.QuotaSampleCapacity != 10 {
		t.Errorf("unexpected disk dynamic config: %+v", diskDyn)
	}

	// 5. Test validation failures
	badInterval := update
	badInterval.Interval = "invalid"
	if err := r.SetDynamicConfig(context.Background(), badInterval); err == nil {
		t.Error("expected error for invalid interval")
	}

	badGroup := update
	badGroup.AntigravityModelGroup = "unknown_group"
	if err := r.SetDynamicConfig(context.Background(), badGroup); err == nil {
		t.Error("expected error for invalid model group")
	}

	invalidCases := []struct {
		name   string
		mutate func(*state.DynamicConfig)
	}{
		{"max_concurrency=33", func(cfg *state.DynamicConfig) { cfg.MaxConcurrency = 33 }},
		{"min_change=101", func(cfg *state.DynamicConfig) { cfg.MinChange = 101 }},
		{"urgency_tolerance=0.51", func(cfg *state.DynamicConfig) { cfg.UrgencyTolerance = 0.51 }},
		{"cooldown=1441", func(cfg *state.DynamicConfig) { cfg.RateLimitCooldownMinutes = 1441 }},
		{"inverted priorities", func(cfg *state.DynamicConfig) {
			cfg.PriorityRules.BoostStartPriority, cfg.PriorityRules.NormalStartPriority = 100, 101
		}},
	}
	for _, testCase := range invalidCases {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := update
			testCase.mutate(&candidate)
			if err := r.SetDynamicConfig(context.Background(), candidate); err == nil {
				t.Fatal("expected backend validation error")
			}
		})
	}

	zeroTolerance := update
	zeroTolerance.UrgencyTolerance = 0
	if err := r.SetDynamicConfig(context.Background(), zeroTolerance); err != nil {
		t.Fatalf("zero urgency tolerance rejected: %v", err)
	}
	gotZero, _ := r.GetDynamicConfig(context.Background())
	if gotZero.UrgencyTolerance != 0 {
		t.Fatalf("zero urgency tolerance was normalized to %v", gotZero.UrgencyTolerance)
	}
}

func TestRuntime_DynamicConfig_SurvivesReconfigure(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	r := newTestRuntime(t, runtime.Options{StateCachePath: cachePath})

	_, err := r.Register(context.Background(), runtime.RegisterRequest{
		ConfigYAML: "",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Set dynamic config from UI
	if err := r.SetDynamicConfig(context.Background(), state.DynamicConfig{
		AutoApply:                false,
		Interval:                 "45m",
		AntigravityModelGroup:    "claude_gpt",
		MaxConcurrency:           10,
		MinChange:                5,
		UrgencyTolerance:         0.05,
		RateLimitCooldownMinutes: 5,
		QuotaSampleCapacity:      6,
		IgnoreDisabledHost:       false,
		PriorityRules: state.PriorityRulesConfig{
			BoostStartPriority:  980,
			NormalStartPriority: 200,
		},
		Schedule: state.ScheduleConfig{
			Paused: false,
		},
	}); err != nil {
		t.Fatal(err)
	}

	// CPA calls reconfigure with minimal YAML (e.g. enabled: true)
	_, err = r.Reconfigure(context.Background(), runtime.ReconfigureRequest{
		ConfigYAML: "enabled: true\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify runtime config preserved dynamic settings from disk
	cfg, err := r.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoApply {
		t.Error("expected AutoApply=false to be preserved after Reconfigure")
	}
	if cfg.Interval != 45*time.Minute {
		t.Errorf("expected Interval=45m, got %v", cfg.Interval)
	}
	if cfg.AntigravityModelGroup != config.AntigravityModelGroupClaudeGPT {
		t.Errorf("expected claude_gpt, got %v", cfg.AntigravityModelGroup)
	}
	if cfg.MaxConcurrency != 10 {
		t.Errorf("expected MaxConcurrency=10, got %d", cfg.MaxConcurrency)
	}
	if cfg.IgnoreDisabledHost {
		t.Fatal("expected ignore disabled host=false to survive Reconfigure")
	}

	if err := r.SetScheduleConfig(context.Background(), state.ScheduleConfig{
		Paused:        false,
		WindowEnabled: true,
		WindowStart:   "09:00",
		WindowEnd:     "23:00",
	}); err != nil {
		t.Fatalf("set schedule config failed: %v", err)
	}
	gotDynamic, err := r.GetDynamicConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotDynamic.IgnoreDisabledHost {
		t.Fatal("schedule persistence must not re-enable disabled-host inclusion filtering")
	}

	reloaded := runtime.New(runtime.Options{StateCachePath: cachePath})
	defer func() { _ = reloaded.Shutdown(context.Background()) }()
	reloadedDynamic, err := reloaded.GetDynamicConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reloadedDynamic.IgnoreDisabledHost {
		t.Fatal("ignore disabled host=false must survive runtime reload")
	}
}
