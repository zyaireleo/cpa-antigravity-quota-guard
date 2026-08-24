package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/host"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/provider/antigravity"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/runtime"
)

type devTestClock struct {
	now time.Time
}

func (c *devTestClock) Now() time.Time {
	return c.now
}

func TestDevHostSeedsAndReadsCPAAuthFiles(t *testing.T) {
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	hostAdapter, err := newDevHost(devHostOptions{
		AuthDir:        t.TempDir(),
		AccountCount:   10,
		QuotaStatePath: filepath.Join(t.TempDir(), "quota.json"),
		Seed:           7,
		NowFn:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	files, err := hostAdapter.ListAuthFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 10 {
		t.Fatalf("auth file count = %d, want 10", len(files))
	}
	if files[0].AuthIndex == "" || files[0].Email == "" || files[0].Type != "antigravity" {
		t.Fatalf("auth file identity = %+v", files[0])
	}

	document, err := hostAdapter.GetAuth(context.Background(), files[0].AuthIndex)
	if err != nil {
		t.Fatal(err)
	}
	if document.Path == "" || !strings.HasSuffix(document.Path, ".json") {
		t.Fatalf("auth document path = %q", document.Path)
	}
	var raw map[string]any
	if err := json.Unmarshal(document.JSON, &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"access_token", "disabled", "email", "expired", "expires_in", "priority", "project_id", "refresh_token", "timestamp", "type"} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("auth file is missing %q: %s", field, document.JSON)
		}
	}

	data, err := os.ReadFile(document.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(document.JSON) {
		t.Fatalf("GetAuth did not expose the physical document")
	}
	raw["disabled"] = true
	raw["priority"] = float64(12)
	updated, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(document.Path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	updatedFiles, err := hostAdapter.ListAuthFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !updatedFiles[0].Disabled || updatedFiles[0].Priority != 12 {
		t.Fatalf("host list did not reread disabled/priority edits: %+v", updatedFiles[0])
	}
	runtimeAuth, err := hostAdapter.GetRuntime(context.Background(), updatedFiles[0].AuthIndex)
	if err != nil {
		t.Fatal(err)
	}
	if !runtimeAuth.Disabled {
		t.Fatalf("runtime auth did not reflect disabled edit: %+v", runtimeAuth)
	}
}

func TestDevHostReturnsBothModelGroupsAndQuotaLifecycle(t *testing.T) {
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	hostAdapter, err := newDevHost(devHostOptions{
		AuthDir:        t.TempDir(),
		AccountCount:   1,
		QuotaStatePath: filepath.Join(t.TempDir(), "quota.json"),
		Seed:           1,
		NowFn:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := hostAdapter.ListAuthFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	readQuota := func() map[antigravity.ModelGroup]antigravity.ProbeResult {
		t.Helper()
		response, err := hostAdapter.HTTPDo(context.Background(), host.HTTPRequest{
			AuthIndex: files[0].AuthIndex,
			Method:    http.MethodPost,
			URL:       antigravity.RetrieveUserQuotaSummaryURL,
			Body:      []byte(`{"project":"dev-project-001"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("simulated quota status = %d", response.StatusCode)
		}
		var contract struct {
			Groups []struct {
				DisplayName string `json:"displayName"`
				Buckets     []struct {
					Window string `json:"window"`
				} `json:"buckets"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(response.Body, &contract); err != nil {
			t.Fatalf("decode simulated quota contract: %v", err)
		}
		if len(contract.Groups) != 2 || len(contract.Groups[0].Buckets) != 2 || len(contract.Groups[1].Buckets) != 2 {
			t.Fatalf("simulated quota response must expose two official group/bucket pairs: %+v", contract.Groups)
		}
		return antigravity.ParseAllModelGroups(response.Body, now)
	}

	previous := readQuota()
	if previous[antigravity.ModelGroupGemini].ShortWindowRemaining == nil || previous[antigravity.ModelGroupClaudeGPT].LongWindowRemaining == nil {
		t.Fatalf("simulated response did not contain both complete groups: %+v", previous)
	}
	depleted := false
	for round := 0; round < 40; round++ {
		now = now.Add(time.Minute)
		current := readQuota()
		previouslyExhausted := false
		for _, group := range simulatedModelGroups {
			old := previous[antigravity.ModelGroup(group)]
			if old.ShortWindowRemaining != nil && old.LongWindowRemaining != nil && (*old.ShortWindowRemaining == 0 || *old.LongWindowRemaining == 0) {
				previouslyExhausted = true
			}
		}
		for _, group := range []antigravity.ModelGroup{antigravity.ModelGroupGemini, antigravity.ModelGroupClaudeGPT} {
			old := previous[group]
			item := current[group]
			if old.ShortWindowRemaining == nil || old.LongWindowRemaining == nil || item.ShortWindowRemaining == nil || item.LongWindowRemaining == nil {
				t.Fatalf("round %d group %s lost a window", round, group)
			}
			if previouslyExhausted {
				if *item.ShortWindowRemaining != 100 || *item.LongWindowRemaining != 100 {
					t.Fatalf("round %d group %s did not recover after credential exhaustion: short=%d/%d long=%d/%d", round, group, *old.ShortWindowRemaining, *item.ShortWindowRemaining, *old.LongWindowRemaining, *item.LongWindowRemaining)
				}
				depleted = true
			} else if *item.ShortWindowRemaining >= *old.ShortWindowRemaining || *item.LongWindowRemaining >= *old.LongWindowRemaining {
				t.Fatalf("round %d group %s did not consume both windows: old=%+v current=%+v", round, group, old, item)
			}
		}
		previous = current
		if depleted {
			break
		}
	}
	if !depleted {
		t.Fatal("deterministic quota simulation did not reach exhaustion")
	}
}

func TestDevRuntimeUsesProductionPathForProbeAndGuardManagement(t *testing.T) {
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	clock := &devTestClock{now: now}
	dev, err := newDevServer(devServerOptions{
		AuthDir:        t.TempDir(),
		QuotaStatePath: filepath.Join(t.TempDir(), "quota.json"),
		StateCachePath: filepath.Join(t.TempDir(), "cache.json"),
		AccountCount:   10,
		Seed:           3,
		Clock:          clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := dev.runtime.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown runtime: %v", err)
		}
	}()

	before := make(map[string]string)
	files, err := dev.host.ListAuthFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		document, err := dev.host.GetAuth(context.Background(), file.AuthIndex)
		if err != nil {
			t.Fatal(err)
		}
		before[file.AuthIndex] = string(document.JSON)
	}

	if err := dev.runtime.Probe(context.Background(), config.AntigravityModelGroupGemini, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := dev.runtime.LatestSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"gemini", "claude_gpt"} {
		if len(snapshot.Groups[group].Items) != 10 {
			t.Fatalf("%s item count = %d, want 10", group, len(snapshot.Groups[group].Items))
		}
	}
	if err := dev.runtime.ManualApplyWithPreview(context.Background(), config.AntigravityModelGroupGemini, nil, snapshot.PreviewID); !errors.Is(err, runtime.ErrLegacyMutationDisabled) {
		t.Fatalf("manual auth mutation was not disabled: %v", err)
	}
	dynamic, err := dev.runtime.GetDynamicConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dynamic.AutoApply = true
	if err := dev.runtime.SetDynamicConfig(context.Background(), dynamic); err == nil {
		t.Fatal("legacy auto_apply was accepted")
	}

	files, err = dev.host.ListAuthFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		document, err := dev.host.GetAuth(context.Background(), file.AuthIndex)
		if err != nil {
			t.Fatal(err)
		}
		if string(document.JSON) != before[file.AuthIndex] {
			t.Fatalf("quota guard changed auth document %s", file.AuthIndex)
		}
	}

	const devManagementKey = "dev-test-management-key"
	server := httptest.NewServer(guardDevHandler(dev.runtime, devManagementKey))
	defer server.Close()
	resourceResponse, err := http.Get(server.URL + "/v0/resource/plugins/cpa-antigravity-quota-guard/status")
	if err != nil {
		t.Fatal(err)
	}
	if resourceResponse.StatusCode != http.StatusOK {
		t.Fatalf("resource status = %d", resourceResponse.StatusCode)
	}
	_ = resourceResponse.Body.Close()

	managementURL := server.URL + "/v0/management/cpa-antigravity-quota-guard/status"
	unauthorized, err := http.Get(managementURL)
	if err != nil {
		t.Fatal(err)
	}
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("management without key status = %d", unauthorized.StatusCode)
	}
	_ = unauthorized.Body.Close()
	authorizedRequest, err := http.NewRequest(http.MethodGet, managementURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	authorizedRequest.Header.Set("X-Management-Key", devManagementKey)
	authorized, err := http.DefaultClient.Do(authorizedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if authorized.StatusCode != http.StatusOK {
		t.Fatalf("management with key status = %d", authorized.StatusCode)
	}
	_ = authorized.Body.Close()

	samples, err := dev.runtime.GetSamples(context.Background(), "dev-auth-001", "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("initial Gemini samples = %d, want 1", len(samples))
	}
	clock.now = clock.now.Add(time.Minute)
	if err := dev.runtime.Probe(context.Background(), config.AntigravityModelGroupGemini, nil); err != nil {
		t.Fatal(err)
	}
	updatedSamples, err := dev.runtime.GetSamples(context.Background(), "dev-auth-001", "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if len(updatedSamples) < 2 {
		t.Fatalf("second probe did not append changed quota sample: %+v", updatedSamples)
	}
}

func TestValidateDevListenAddressDefaultsToLoopbackSafety(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if err := validateDevListenAddress(address, false); err != nil {
			t.Fatalf("loopback address %q rejected: %v", address, err)
		}
	}
	for _, address := range []string{":8080", "0.0.0.0:8080", "192.0.2.10:8080"} {
		if err := validateDevListenAddress(address, false); err == nil {
			t.Fatalf("non-loopback address %q accepted without unsafe flag", address)
		}
		if err := validateDevListenAddress(address, true); err != nil {
			t.Fatalf("non-loopback address %q rejected with unsafe flag: %v", address, err)
		}
	}
}
