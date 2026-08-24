package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	pluginruntime "github.com/zyaireleo/cpa-antigravity-quota-guard/internal/runtime"
)

type pluginEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Version     string   `json:"version"`
	Repository  string   `json:"repository"`
	Homepage    string   `json:"homepage"`
	License     string   `json:"license"`
	Tags        []string `json:"tags"`
}

type storeRegistry struct {
	SchemaVersion int           `json:"schema_version"`
	Plugins       []pluginEntry `json:"plugins"`
}

func TestRegistryJSON_SchemaValidation(t *testing.T) {
	data, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatalf("failed to read registry.json: %v", err)
	}

	var reg storeRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		t.Fatalf("registry.json is not valid JSON: %v", err)
	}

	if reg.SchemaVersion != 1 {
		t.Errorf("expected schema_version 1, got %d", reg.SchemaVersion)
	}

	if len(reg.Plugins) == 0 {
		t.Fatal("expected at least one plugin in registry.json")
	}

	found := false
	for _, p := range reg.Plugins {
		if p.ID == "cpa-antigravity-quota-guard" {
			found = true
			if p.Name == "" {
				t.Error("plugin name must not be empty")
			}
			if p.Description == "" {
				t.Error("plugin description must not be empty")
			}
			if p.Author != "zyaireleo" {
				t.Errorf("expected author zyaireleo, got %q", p.Author)
			}
			if p.Version == "" {
				t.Error("plugin version must not be empty")
			}
			if p.Repository != "https://github.com/zyaireleo/cpa-antigravity-quota-guard" {
				t.Errorf("unexpected repository %q", p.Repository)
			}
			if p.Homepage != "https://github.com/zyaireleo/cpa-antigravity-quota-guard" {
				t.Errorf("unexpected homepage %q", p.Homepage)
			}
			if p.License != "MIT" {
				t.Errorf("expected MIT license, got %q", p.License)
			}
			if len(p.Tags) < 3 {
				t.Errorf("expected at least 3 tags, got %v", p.Tags)
			}
		}
	}

	if !found {
		t.Error("plugin id cpa-antigravity-quota-guard not found in registry.json")
	}
}

func TestWorkflows_CI_Validation(t *testing.T) {
	ciPath := filepath.Join(".github", "workflows", "ci.yml")
	data, err := os.ReadFile(ciPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", ciPath, err)
	}
	content := string(data)

	expectedSnippets := []string{
		"name: CI",
		"push:",
		"pull_request:",
		"golangci-lint",
		"-race",
		"ubuntu-latest",
		"macos-latest",
		"windows-latest",
	}

	for _, snippet := range expectedSnippets {
		if !strings.Contains(content, snippet) {
			t.Errorf("ci.yml missing required snippet %q", snippet)
		}
	}
}

func TestWorkflows_Release_MatrixValidation(t *testing.T) {
	releasePath := filepath.Join(".github", "workflows", "release.yml")
	data, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", releasePath, err)
	}
	content := string(data)

	// Verify all 7 target matrix platforms are handled:
	// 1. linux_amd64
	// 2. linux_arm64
	// 3. darwin_arm64
	// 4. darwin_amd64
	// 5. windows_amd64
	// 6. windows_arm64
	// 7. freebsd_amd64
	requiredSnippets := []string{
		"goos: linux",
		"goarch: amd64",
		"goarch: arm64",
		"goos: darwin",
		"windows_amd64",
		"windows_arm64",
		"freebsd_amd64",
	}

	for _, snippet := range requiredSnippets {
		if !strings.Contains(content, snippet) {
			t.Errorf("release.yml missing matrix snippet %q", snippet)
		}
	}

	// Verify required release steps and toolchains
	releaseRequirements := []string{
		"PLUGIN_NAME: cpa-antigravity-quota-guard",
		"checksums.txt",
		"sha256sum",
		"vmactions/freebsd-vm",
		"mlugg/setup-zig",
		"msys2/setup-msys2",
	}

	for _, req := range releaseRequirements {
		if !strings.Contains(content, req) {
			t.Errorf("release.yml missing requirement %q", req)
		}
	}
}

func TestDocumentation_BilingualCompleteness(t *testing.T) {
	docFiles := []string{"README.md", "README.en.md"}

	for _, docFile := range docFiles {
		data, err := os.ReadFile(docFile)
		if err != nil {
			t.Fatalf("failed to read %s: %v", docFile, err)
		}
		content := string(data)

		// Core sections and standardized CPA paths check
		keywords := []string{
			"cpa-antigravity-quota-guard",
			"gemini",
			"claude_gpt",
			"observe",
			"enforce",
			"registry.json",
			"/v0/resource/plugins/cpa-antigravity-quota-guard/status",
			"/v0/management/cpa-antigravity-quota-guard/status",
			"/v0/management/cpa-antigravity-quota-guard/config",
			"/v0/management/cpa-antigravity-quota-guard/actions/probe",
			"/v0/management/cpa-antigravity-quota-guard/actions/half-open",
		}

		for _, kw := range keywords {
			if !strings.Contains(strings.ToLower(content), strings.ToLower(kw)) {
				t.Errorf("%s missing keyword %q", docFile, kw)
			}
		}
	}
}

func TestReleaseIdentityAndVersionConsistency(t *testing.T) {
	data, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var registry storeRegistry
	if err := json.Unmarshal(data, &registry); err != nil || len(registry.Plugins) != 1 {
		t.Fatalf("invalid registry: err=%v plugins=%d", err, len(registry.Plugins))
	}
	entry := registry.Plugins[0]
	if entry.ID != config.PluginID {
		t.Fatalf("registry id=%q, config plugin id=%q", entry.ID, config.PluginID)
	}
	if config.PluginID != config.DynamicLibraryBaseName {
		t.Fatalf("plugin id=%q, library basename=%q", config.PluginID, config.DynamicLibraryBaseName)
	}
	if config.PluginID != config.CPAConfigKey {
		t.Fatalf("plugin id=%q, config key=%q", config.PluginID, config.CPAConfigKey)
	}

	runtime := pluginruntime.New(pluginruntime.Options{
		StateCachePath: filepath.Join(t.TempDir(), "quota-cache.json"),
		GuardStatePath: filepath.Join(t.TempDir(), "guard-state.json"),
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	result, err := runtime.Register(context.Background(), pluginruntime.RegisterRequest{ConfigYAML: "mode: observe\n", SchemaVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata.Version != entry.Version || result.Metadata.Name != entry.Name || result.Metadata.Author != entry.Author || result.Metadata.GitHubRepository != entry.Repository {
		t.Fatalf("runtime metadata and registry differ: metadata=%+v registry=%+v", result.Metadata, entry)
	}

	checks := map[string][]string{
		filepath.Join(".github", "workflows", "release.yml"):               {"PLUGIN_NAME: " + entry.ID},
		filepath.Join("internal", "management", "feature_shell_assets.go"): {"v" + entry.Version},
		filepath.Join(".github", "release-notes", "v"+entry.Version+".md"): {"# " + entry.ID + " v" + entry.Version, "### 中文", "### English"},
		"README.md":    {entry.ID},
		"README.en.md": {entry.ID},
	}
	for path, snippets := range checks {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, snippet := range snippets {
			if !strings.Contains(string(content), snippet) {
				t.Errorf("%s missing %q", path, snippet)
			}
		}
	}
}
