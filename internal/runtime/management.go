package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/apply"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/management"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/state"
)

// ManagementRequest represents the HTTP request envelope passed by CPA management.handle.
type ManagementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

// ManagementResponse represents the HTTP response envelope returned to CPA.
type ManagementResponse struct {
	StatusCode  int                 `json:"StatusCode"`
	ContentType string              `json:"content_type"`
	Headers     map[string][]string `json:"Headers"`
	Body        string              `json:"Body"`
}

type managementRoute struct {
	Method string `json:"Method"`
	Path   string `json:"Path"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes"`
	Resources []managementResource `json:"resources"`
}

type managementRunner struct {
	runtime *Runtime
}

func decodeManagementRequest(raw []byte) (ManagementRequest, error) {
	var request managementRequestWire
	if len(raw) == 0 {
		return ManagementRequest{}, fmt.Errorf("%w: management request is required", ErrInvalidRequest)
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return ManagementRequest{}, fmt.Errorf("%w: decode management request: %v", ErrInvalidRequest, err)
	}
	parsed, err := request.toManagementRequest()
	if err != nil {
		return ManagementRequest{}, err
	}
	if parsed.Method == "" || parsed.Path == "" {
		return ManagementRequest{}, fmt.Errorf("%w: management method and path are required", ErrInvalidRequest)
	}
	return parsed, nil
}

type managementRequestWire struct {
	Method      string          `json:"Method"`
	MethodLower string          `json:"method"`
	Path        string          `json:"Path"`
	PathLower   string          `json:"path"`
	Headers     http.Header     `json:"Headers"`
	QueryRaw    json.RawMessage `json:"Query"`
	QueryLower  string          `json:"query"`
	Body        string          `json:"Body"`
	BodyLower   string          `json:"body"`
}

func (w managementRequestWire) toManagementRequest() (ManagementRequest, error) {
	method := firstNonEmpty(w.Method, w.MethodLower)
	path := firstNonEmpty(w.Path, w.PathLower)
	body, err := decodeManagementBody(w.Body, w.BodyLower)
	if err != nil {
		return ManagementRequest{}, err
	}
	var query url.Values
	if len(w.QueryRaw) > 0 {
		var strQuery string
		if err := json.Unmarshal(w.QueryRaw, &strQuery); err == nil {
			values, err := url.ParseQuery(strings.TrimPrefix(strQuery, "?"))
			if err != nil {
				return ManagementRequest{}, fmt.Errorf("%w: decode management query: %v", ErrInvalidRequest, err)
			}
			query = values
		} else {
			var mapQuery map[string][]string
			if err := json.Unmarshal(w.QueryRaw, &mapQuery); err == nil {
				query = mapQuery
			}
		}
	}
	if query == nil && strings.TrimSpace(w.QueryLower) != "" {
		values, err := url.ParseQuery(strings.TrimPrefix(w.QueryLower, "?"))
		if err != nil {
			return ManagementRequest{}, fmt.Errorf("%w: decode management query: %v", ErrInvalidRequest, err)
		}
		query = values
	}
	return ManagementRequest{Method: method, Path: path, Headers: w.Headers, Query: query, Body: body}, nil
}

func decodeManagementBody(official string, legacy string) ([]byte, error) {
	if official != "" {
		decoded, err := base64.StdEncoding.DecodeString(official)
		if err != nil {
			return nil, fmt.Errorf("%w: decode management body: %v", ErrInvalidRequest, err)
		}
		return decoded, nil
	}
	return []byte(legacy), nil
}

func envelopeManagement(result any, err error) []byte {
	if err != nil {
		return failure(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return failure(fmt.Errorf("encode management result: %w", err))
	}
	return mustMarshal(Envelope{OK: true, Result: encoded})
}

func (r managementRunner) Run(ctx context.Context, request management.RunRequest) (apply.Result, error) {
	if request.Mode == "apply" {
		if err := r.runtime.ManualApplyWithPreview(ctx, request.AntigravityModelGroup, request.AuthIndexes, request.PreviewID); err != nil {
			return apply.Result{}, err
		}
		result, _ := r.runtime.currentRunSnapshot()
		return result, nil
	}
	if request.Mode == "probe" {
		if err := r.runtime.Probe(ctx, request.AntigravityModelGroup, request.AuthIndexes); err != nil {
			return apply.Result{}, err
		}
		result, _ := r.runtime.currentRunSnapshot()
		return result, nil
	}
	return apply.Result{}, fmt.Errorf("unsupported run mode: %s", request.Mode)
}

func (r managementRunner) Reset(ctx context.Context) (map[string]any, error) {
	return r.runtime.ResetAllPriorities(ctx)
}

func (r managementRunner) Status(ctx context.Context) (management.StatusInfo, error) {
	return r.runtime.Status(ctx)
}

func (r managementRunner) LatestSnapshot(ctx context.Context) (apply.DualGroupSnapshot, error) {
	return r.runtime.LatestSnapshot(ctx)
}

func (r managementRunner) SyncHost(ctx context.Context, modelGroup config.AntigravityModelGroup) (apply.DualGroupSnapshot, error) {
	return r.runtime.SyncHost(ctx, modelGroup)
}

func (r managementRunner) Diagnostics(ctx context.Context) (map[string]any, error) {
	diagnostics, err := r.runtime.Diagnostics(ctx)
	if err != nil {
		return nil, err
	}
	diagnostics["run_history"] = r.runtime.managementRunHistory()
	overlayManagementCooldownEmails(diagnostics["active_cooldowns"], r.runtime.currentSnapshotEmails())
	return diagnostics, nil
}

// managementRunHistory projects full display emails only at the authenticated
// management adapter boundary. Persisted run history remains redacted.
func (r *Runtime) managementRunHistory() []map[string]any {
	history := r.currentRunHistory()
	emailByMask := uniqueEmailByMask(r.currentSnapshotEmails(), func(_ string, email string) string {
		return redactRuntimeIdentifier(email)
	})
	emailByAuthMask := uniqueEmailByMask(r.currentSnapshotEmails(), func(authIndex, _ string) string {
		return redactRuntimeIdentifier(authIndex)
	})
	result := make([]map[string]any, 0, len(history))
	for _, entry := range history {
		encoded, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(encoded, &item); err != nil {
			continue
		}
		if entry.Snapshot != nil {
			if snapshot, ok := item["snapshot"].(map[string]any); ok {
				overlayManagementItemEmails(snapshot["items"], entry.Snapshot.Items, emailByMask, emailByAuthMask)
				overlayManagementChangeEmails(snapshot["changes"], entry.Snapshot.Changes, emailByMask, emailByAuthMask)
			}
		}
		result = append(result, item)
	}
	return result
}

func overlayManagementItemEmails(raw any, identities []apply.SnapshotItem, emailByMask, emailByAuthMask map[string]string) {
	items, ok := raw.([]any)
	if !ok {
		return
	}
	for index, identity := range identities {
		if index >= len(items) {
			continue
		}
		item, ok := items[index].(map[string]any)
		if !ok {
			continue
		}
		overlayManagementEmail(item, identity.Identity.Email, emailByMask, emailByAuthMask)
	}
}

func overlayManagementChangeEmails(raw any, identities []apply.SnapshotChange, emailByMask, emailByAuthMask map[string]string) {
	items, ok := raw.([]any)
	if !ok {
		return
	}
	for index, identity := range identities {
		if index >= len(items) {
			continue
		}
		item, ok := items[index].(map[string]any)
		if !ok {
			continue
		}
		overlayManagementEmail(item, identity.Identity.Email, emailByMask, emailByAuthMask)
	}
}

func overlayManagementEmail(item map[string]any, identityEmail string, emailByMask, emailByAuthMask map[string]string) {
	if identityEmail != "" {
		item["email"] = identityEmail
		return
	}
	if masked, ok := item["email"].(string); ok {
		if email := emailByMask[masked]; email != "" {
			item["email"] = email
			return
		}
	}
	if maskedAuthIndex, ok := item["auth_index"].(string); ok {
		if email := emailByAuthMask[maskedAuthIndex]; email != "" {
			item["email"] = email
			return
		}
	}
	item["email"] = ""
}

func overlayManagementCooldownEmails(raw any, emailsByAuthIndex map[string]string) {
	cooldowns, ok := raw.([]map[string]any)
	if !ok {
		return
	}
	emailByAuthMask := uniqueEmailByMask(emailsByAuthIndex, func(authIndex string, _ string) string {
		return redactRuntimeIdentifier(authIndex)
	})
	for _, cooldown := range cooldowns {
		maskedAuthIndex, _ := cooldown["auth_index"].(string)
		if email := emailByAuthMask[maskedAuthIndex]; email != "" {
			cooldown["email"] = email
		}
	}
}

func uniqueEmailByMask(emailsByAuthIndex map[string]string, mask func(authIndex, email string) string) map[string]string {
	result := make(map[string]string)
	ambiguous := make(map[string]struct{})
	for authIndex, email := range emailsByAuthIndex {
		masked := mask(authIndex, email)
		if masked == "" {
			continue
		}
		if _, exists := ambiguous[masked]; exists {
			continue
		}
		if previous, exists := result[masked]; exists && previous != email {
			delete(result, masked)
			ambiguous[masked] = struct{}{}
			continue
		}
		result[masked] = email
	}
	return result
}

func (r managementRunner) GetScheduleConfig(ctx context.Context) (config.ScheduleConfig, error) {
	return r.runtime.GetScheduleConfig(ctx)
}

func (r managementRunner) SetScheduleConfig(ctx context.Context, cfg config.ScheduleConfig) error {
	return r.runtime.SetScheduleConfig(ctx, cfg)
}

func (r managementRunner) GetDynamicConfig(ctx context.Context) (config.DynamicConfig, error) {
	return r.runtime.GetDynamicConfig(ctx)
}

func (r managementRunner) SetDynamicConfig(ctx context.Context, cfg config.DynamicConfig) error {
	return r.runtime.SetDynamicConfig(ctx, cfg)
}

func (r managementRunner) GetSamples(ctx context.Context, authIndex, modelGroup string) ([]state.QuotaSample, error) {
	return r.runtime.GetSamples(ctx, authIndex, modelGroup)
}

func (r managementRunner) GetProbeSamples(ctx context.Context, probeRoundID, modelGroup string) ([]state.ProbeSampleRecord, error) {
	return r.runtime.GetProbeSamples(ctx, probeRoundID, modelGroup)
}

var _ management.Runner = managementRunner{}
