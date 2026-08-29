package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfficialGuardStatusPageUsesCurrentABIAndNoLegacyMutationUI(t *testing.T) {
	page := officialGuardStatusHTML
	for _, required := range []string{
		`<meta name="color-scheme" content="light">`,
		"color-scheme:light",
		"/v0/management/cpa-antigravity-quota-guard/status",
		"/v0/management/cpa-antigravity-quota-guard/actions/probe",
		"/v0/management/cpa-antigravity-quota-guard/actions/half-open",
		"snapshot.entries",
		"snapshot.metrics",
		"remaining_percent",
		"model_group",
		"X-Management-Key",
		"managementKey",
		"clearStatus",
		"statusStale",
		"Half-open 已提交，但状态刷新失败",
		"未知",
	} {
		if !strings.Contains(page, required) {
			t.Errorf("official status page is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"color-scheme:dark",
		"--bg:#07111f",
		"radial-gradient(",
		"antigravity-priority",
		"/snapshot/latest",
		"/runtime-config",
		"host.auth.save",
		"localStorage",
		"sessionStorage",
		"alert(",
		"confirm(",
		"prompt(",
		"<script src=",
		"<link ",
		"http://",
		"https://",
	} {
		if strings.Contains(page, forbidden) {
			t.Errorf("official status page contains forbidden %q", forbidden)
		}
	}
}

func TestOfficialGuardStatusPageEmbeddedJavaScriptSyntax(t *testing.T) {
	start := strings.Index(officialGuardStatusHTML, "<script>")
	end := strings.LastIndex(officialGuardStatusHTML, "</script>")
	if start < 0 || end <= start {
		t.Fatal("official status page does not contain an inline script")
	}
	script := officialGuardStatusHTML[start+len("<script>") : end]
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is unavailable; cannot run embedded JavaScript syntax check: %v", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "official-status-page.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "--check", scriptPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("node --check failed: %v\n%s", err, output)
	}
}

func TestOfficialGuardStatusResourceResponseUsesSelfFramingPolicy(t *testing.T) {
	runtime := New(Options{
		StateCachePath: filepath.Join(t.TempDir(), "cache.json"),
		GuardStatePath: filepath.Join(t.TempDir(), "guard.json"),
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	raw, err := json.Marshal(ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/cpa-antigravity-quota-guard/status",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, envelopeErr := decodeOfficialEnvelope[ManagementResponse](t, runtime.Handle(context.Background(), MethodManagementHandle, raw))
	if envelopeErr != nil {
		t.Fatalf("resource request failed: %+v", envelopeErr)
	}
	if response.StatusCode != http.StatusOK || response.ContentType != "text/html; charset=utf-8" {
		t.Fatalf("resource response = %#v, want HTTP 200 HTML", response)
	}
	body, err := base64.StdEncoding.DecodeString(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != officialGuardStatusHTML {
		t.Fatal("resource response did not return the official status page")
	}
	csp := strings.Join(response.Headers["Content-Security-Policy"], "; ")
	if !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Fatalf("resource CSP = %q, want frame-ancestors 'self'", csp)
	}
	if strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "frame-ancestors *") {
		t.Fatalf("resource CSP has an incompatible frame policy: %q", csp)
	}
	if strings.Contains(string(body), "localStorage") || strings.Contains(string(body), "sessionStorage") {
		t.Fatal("management key must not be persisted by the resource page")
	}
}
