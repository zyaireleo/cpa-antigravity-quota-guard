package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/guard"
)

const guardManagementPrefix = "cpa-antigravity-quota-guard"

func (r *Runtime) handleSchedulerPick(_ context.Context, raw []byte) []byte {
	var request schedulerPickRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return failure(fmt.Errorf("%w: decode scheduler pick: %v", ErrInvalidRequest, err))
	}
	if r.schedulerPickHook != nil {
		r.schedulerPickHook("entered")
	}
	r.guardRuntime.access.RLock()
	defer r.guardRuntime.access.RUnlock()
	cfg := r.currentConfig()
	if r.schedulerPickHook != nil {
		r.schedulerPickHook("config_read")
	}
	delegateConfigured := r.shouldDelegateConfiguredScheduler(cfg, request)
	engine, _ := r.guardRuntime.currentEngine()
	if engine == nil {
		if delegateConfigured {
			return successResult(schedulerPickResponse{Handled: true, DelegateBuiltin: schedulerDelegateConfigured})
		}
		return successResult(schedulerPickResponse{Handled: false})
	}
	if schedulerRequestRequiresReadyGuard(cfg, request, delegateConfigured) && !r.guardRuntime.enforcementIsReady() {
		if candidate, ok := firstNonAntigravityCandidate(request); ok {
			return successResult(schedulerPickResponse{AuthID: candidate.ID, Handled: true})
		}
		message := "Antigravity quota guard is armed but baseline quota evidence is not ready"
		direct := guardTerminationResponse(groupBlockStatus{
			blocked:    true,
			code:       guard.ErrorCodeGuardNotReady,
			message:    message,
			httpStatus: http.StatusServiceUnavailable,
		}, guardedModelGroup(request.Model))
		return failureDirectResponse(guard.ErrorCodeGuardNotReady, message, http.StatusServiceUnavailable, false, direct.ResponseHeaders, direct.ResponseBody)
	}
	candidates := make([]guard.Candidate, 0, len(request.Candidates))
	for _, candidate := range request.Candidates {
		candidates = append(candidates, guard.Candidate{
			AuthID:   strings.TrimSpace(candidate.ID),
			Provider: candidate.Provider,
			Priority: candidate.Priority,
		})
	}
	result := engine.Pick(guard.PickRequest{
		Now:                r.clock.Now().UTC(),
		RequestID:          request.RequestID,
		Provider:           request.Provider,
		Providers:          request.Providers,
		Model:              request.Model,
		Candidates:         candidates,
		Mode:               guard.Mode(cfg.Guard.Mode),
		EnforcedGroups:     configuredGuardGroups(cfg.Guard),
		UnknownAuthPolicy:  guard.Policy(cfg.Guard.UnknownAuthPolicy),
		UnknownModelPolicy: guard.Policy(cfg.Guard.UnknownModelPolicy),
	})
	if engine.IsDirty() {
		r.guardRuntime.signalPersist()
	}
	if !result.Handled {
		if delegateConfigured {
			return successResult(schedulerPickResponse{Handled: true, DelegateBuiltin: schedulerDelegateConfigured})
		}
		return successResult(schedulerPickResponse{Handled: false})
	}
	if result.SelectedAuthID != "" {
		return successResult(schedulerPickResponse{AuthID: result.SelectedAuthID, Handled: true})
	}
	status := http.StatusServiceUnavailable
	retryable := false
	message := "Antigravity credential selection was rejected by quota guard"
	if result.ErrorCode == guard.ErrorCodeModelCooldown {
		status = http.StatusTooManyRequests
		retryable = true
		message = "all eligible Antigravity credentials are cooling down"
		block := groupBlockStatus{
			blocked:    true,
			code:       result.ErrorCode,
			message:    message,
			httpStatus: status,
			retryAfter: result.RetryAfter,
		}
		direct := guardTerminationResponse(block, result.Group)
		return failureDirectResponse(result.ErrorCode, message, status, retryable, direct.ResponseHeaders, direct.ResponseBody)
	}
	return failureStatus(result.ErrorCode, message, status, retryable)
}

func schedulerRequestRequiresReadyGuard(cfg config.Config, request schedulerPickRequest, routeIncludesAntigravity bool) bool {
	if cfg.Guard.Mode != config.GuardModeEnforce || !routeIncludesAntigravity {
		return false
	}
	group, known := guard.ClassifyModel(request.Model)
	if !known {
		return cfg.Guard.UnknownModelPolicy == config.GuardPolicyFailClosed
	}
	return guardGroupEnabled(group, cfg.Guard.EnforcedGroups)
}

func firstNonAntigravityCandidate(request schedulerPickRequest) (schedulerAuthCandidate, bool) {
	for _, candidate := range request.Candidates {
		if strings.TrimSpace(candidate.Provider) != "" && !guard.IsAntigravityProvider(candidate.Provider) {
			return candidate, true
		}
	}
	return schedulerAuthCandidate{}, false
}

func guardedModelGroup(model string) guard.ModelGroup {
	group, _ := guard.ClassifyModel(model)
	return group
}

func (r *Runtime) shouldDelegateConfiguredScheduler(cfg config.Config, request schedulerPickRequest) bool {
	if !containsProvider(cfg.RequiredSchedulerFor, "antigravity") {
		return false
	}
	features := r.currentHostFeatures()
	if _, ok := features[hostFeatureRequiredSchedulerV1]; !ok {
		return false
	}
	if guard.IsAntigravityProvider(request.Provider) {
		return true
	}
	for _, provider := range request.Providers {
		if guard.IsAntigravityProvider(provider) {
			return true
		}
	}
	for _, candidate := range request.Candidates {
		if guard.IsAntigravityProvider(candidate.Provider) {
			return true
		}
	}
	return false
}

func (r *Runtime) handleUsage(_ context.Context, raw []byte) []byte {
	var record usageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return failure(fmt.Errorf("%w: decode usage record: %v", ErrInvalidRequest, err))
	}
	r.guardRuntime.access.RLock()
	defer r.guardRuntime.access.RUnlock()
	engine, _ := r.guardRuntime.currentEngine()
	if engine != nil {
		provider := record.Provider
		if !guard.IsAntigravityProvider(provider) && guardSnapshotContainsAuth(engine.Snapshot(r.clock.Now().UTC()), record.AuthID, record.AuthIndex) {
			provider = "antigravity"
		}
		result := engine.ObserveUsage(guard.UsageObservation{
			RequestID:   record.RequestID,
			Provider:    provider,
			Model:       record.Model,
			AuthID:      record.AuthID,
			AuthIndex:   record.AuthIndex,
			Failed:      record.Failed,
			StatusCode:  record.Failure.StatusCode,
			Body:        record.Failure.Body,
			Headers:     record.ResponseHeaders,
			RequestedAt: record.RequestedAt,
			ObservedAt:  usageObservedAt(record),
		})
		if result.Changed || engine.IsDirty() {
			r.guardRuntime.signalPersist()
		}
	}
	return successResult(struct{}{})
}

func guardSnapshotContainsAuth(snapshot guard.Snapshot, authID, authIndex string) bool {
	for _, entry := range snapshot.Entries {
		if strings.TrimSpace(authIndex) != "" && entry.AuthIndex == strings.TrimSpace(authIndex) {
			return true
		}
		if strings.TrimSpace(authID) != "" && entry.AuthID == strings.TrimSpace(authID) {
			return true
		}
	}
	return false
}

func usageObservedAt(record usageRecord) time.Time {
	if record.RequestedAt.IsZero() {
		return time.Now().UTC()
	}
	observedAt := record.RequestedAt.Add(record.Latency)
	if observedAt.Before(record.RequestedAt) {
		return record.RequestedAt.UTC()
	}
	return observedAt.UTC()
}

func (r *Runtime) handleRequestBefore(_ context.Context, raw []byte) []byte {
	var request requestInterceptRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return failure(fmt.Errorf("%w: decode request interceptor: %v", ErrInvalidRequest, err))
	}
	// Before-auth requests do not expose the selected provider or candidates in
	// CPA v7.2.141. Blocking here based only on a model name could reject a
	// healthy mixed-provider route, so the strict check is performed after auth.
	return successResult(requestInterceptResponse{})
}

func (r *Runtime) handleRequestAfter(_ context.Context, raw []byte) []byte {
	var request requestInterceptRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return failure(fmt.Errorf("%w: decode request interceptor: %v", ErrInvalidRequest, err))
	}
	if !strings.EqualFold(strings.TrimSpace(request.ToFormat), "antigravity") {
		return successResult(requestInterceptResponse{})
	}
	r.guardRuntime.access.RLock()
	defer r.guardRuntime.access.RUnlock()
	cfg := r.currentConfig()
	if cfg.Guard.Mode != config.GuardModeEnforce {
		return successResult(requestInterceptResponse{})
	}
	group, known := guard.ClassifyModel(request.Model)
	if !known {
		if cfg.Guard.UnknownModelPolicy != config.GuardPolicyFailClosed {
			return successResult(requestInterceptResponse{})
		}
		return successResult(guardTerminationResponse(groupBlockStatus{
			blocked:    true,
			code:       guard.ErrorCodeQuotaGroupUnknown,
			message:    "Antigravity model does not map to a protected quota group",
			httpStatus: http.StatusServiceUnavailable,
		}, ""))
	}
	if !guardGroupEnabled(group, cfg.Guard.EnforcedGroups) {
		return successResult(requestInterceptResponse{})
	}
	if !r.guardRuntime.enforcementIsReady() {
		return successResult(guardTerminationResponse(groupBlockStatus{
			blocked:    true,
			code:       guard.ErrorCodeGuardNotReady,
			message:    "Antigravity quota guard is armed but baseline quota evidence is not ready",
			httpStatus: http.StatusServiceUnavailable,
		}, group))
	}
	engine, _ := r.guardRuntime.currentEngine()
	if engine == nil {
		return successResult(requestInterceptResponse{})
	}
	status := evaluateGuardGroup(engine.Snapshot(r.clock.Now().UTC()), group, cfg.Guard.UnknownAuthPolicy, cfg.Guard.EvidenceMaxAge)
	if !status.blocked {
		return successResult(requestInterceptResponse{})
	}
	return successResult(guardTerminationResponse(status, group))
}

func guardTerminationResponse(status groupBlockStatus, group guard.ModelGroup) requestInterceptResponse {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":        "rate_limit_error",
			"code":        status.code,
			"message":     status.message,
			"provider":    "antigravity",
			"model_group": string(group),
		},
	})
	response := requestInterceptResponse{
		Terminate:       true,
		StatusCode:      status.httpStatus,
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		ResponseBody:    body,
	}
	if status.retryAfter > 0 {
		response.ResponseHeaders.Set("Retry-After", strconv.FormatInt(int64(status.retryAfter/time.Second), 10))
	}
	return response
}

type groupBlockStatus struct {
	blocked    bool
	code       string
	message    string
	httpStatus int
	retryAfter time.Duration
}

func evaluateGuardGroup(snapshot guard.Snapshot, group guard.ModelGroup, unknownPolicy string, maxAge time.Duration) groupBlockStatus {
	seen := 0
	unknown := 0
	cooling := 0
	halfOpenReady := false
	for _, entry := range snapshot.Entries {
		if entry.ModelGroup != group {
			continue
		}
		if (entry.State == guard.StateClosed || entry.State == guard.StateUninitialized) &&
			!guard.EvidenceFresh(entry, snapshot.GeneratedAt, maxAge) {
			continue
		}
		seen++
		switch entry.State {
		case guard.StateClosed:
			return groupBlockStatus{}
		case guard.StateUninitialized:
			unknown++
		case guard.StateHalfOpen:
			if entry.HalfOpenLeaseUntil.After(snapshot.GeneratedAt) {
				halfOpenReady = true
			}
			cooling++
		case guard.StateOpen:
			cooling++
		}
	}
	// Scheduler.Pick turns the one selected recovery request into half-open
	// before the after-auth interceptor runs. That leased request must be
	// allowed to reach the provider; usage.handle will close or reopen it.
	if halfOpenReady {
		return groupBlockStatus{}
	}
	if seen == 0 || unknown > 0 {
		if unknownPolicy == config.GuardPolicyFailClosed {
			return groupBlockStatus{
				blocked:    true,
				code:       guard.ErrorCodeAuthUninitialized,
				message:    "Antigravity quota state is not initialized",
				httpStatus: http.StatusServiceUnavailable,
			}
		}
		return groupBlockStatus{}
	}
	if cooling != seen {
		return groupBlockStatus{}
	}
	retryAfter := time.Duration(0)
	if recoverAt := snapshot.EarliestRecover[group]; !recoverAt.IsZero() && recoverAt.After(snapshot.GeneratedAt) {
		retryAfter = recoverAt.Sub(snapshot.GeneratedAt)
		retryAfter = ((retryAfter + time.Second - 1) / time.Second) * time.Second
	}
	return groupBlockStatus{
		blocked:    true,
		code:       guard.ErrorCodeModelCooldown,
		message:    "All eligible Antigravity credentials are cooling down",
		httpStatus: http.StatusTooManyRequests,
		retryAfter: retryAfter,
	}
}

func guardGroupEnabled(group guard.ModelGroup, configured []config.AntigravityModelGroup) bool {
	if len(configured) == 0 {
		return true
	}
	for _, candidate := range configured {
		if guard.ModelGroup(candidate) == group {
			return true
		}
	}
	return false
}

func (r *Runtime) registerGuardManagement(raw []byte) []byte {
	var request managementRegistrationRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &request)
	}
	result := managementRegistration{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: guardManagementPrefix + "/status"},
			{Method: http.MethodGet, Path: guardManagementPrefix + "/config"},
			{Method: http.MethodPut, Path: guardManagementPrefix + "/config"},
			{Method: http.MethodPost, Path: guardManagementPrefix + "/actions/probe"},
			{Method: http.MethodPost, Path: guardManagementPrefix + "/actions/half-open"},
		},
		Resources: []managementResource{{
			Path:        "/status",
			Menu:        "Antigravity Quota Guard",
			Description: "Model-group quota breaker status and controls.",
		}},
	}
	return envelopeManagement(result, nil)
}

func (r *Runtime) handleGuardManagement(ctx context.Context, raw []byte) []byte {
	request, err := decodeManagementRequest(raw)
	if err != nil {
		return failure(err)
	}
	path := strings.TrimSuffix(request.Path, "/")
	switch {
	case strings.HasSuffix(path, "/status") && request.Method == http.MethodGet:
		if strings.Contains(path, "/resource/") {
			return envelopeManagement(guardManagementBytes(http.StatusOK, "text/html; charset=utf-8", []byte(officialGuardStatusHTML)), nil)
		}
		return r.guardStatusResponse()
	case strings.HasSuffix(path, "/config") && request.Method == http.MethodGet:
		return envelopeManagement(guardManagementJSON(http.StatusOK, r.currentConfig().Guard.Dynamic()), nil)
	case strings.HasSuffix(path, "/config") && request.Method == http.MethodPut:
		var update config.GuardDynamic
		if err := json.Unmarshal(request.Body, &update); err != nil {
			return envelopeManagement(guardManagementJSON(http.StatusBadRequest, map[string]string{"error": err.Error()}), nil)
		}
		dynamic := r.currentConfig().Dynamic()
		dynamic.Guard = update
		if err := r.SetDynamicConfig(ctx, dynamic); err != nil {
			return envelopeManagement(guardManagementJSON(http.StatusBadRequest, map[string]string{"error": err.Error()}), nil)
		}
		return r.guardStatusResponse()
	case strings.HasSuffix(path, "/actions/probe") && request.Method == http.MethodPost:
		if !r.guardRuntime.enforcementInventoryReady() {
			return envelopeManagement(guardManagementJSON(http.StatusServiceUnavailable, map[string]string{"error": errAuthInventoryNotReady.Error()}), nil)
		}
		err = r.refreshGuardRoster(ctx)
		if err == nil {
			err = r.Probe(ctx, config.AntigravityModelGroupGemini, nil)
		}
		if err != nil {
			return envelopeManagement(guardManagementJSON(http.StatusBadGateway, map[string]string{"error": err.Error()}), nil)
		}
		return r.guardStatusResponse()
	case strings.HasSuffix(path, "/actions/half-open") && request.Method == http.MethodPost:
		var action struct {
			AuthIndex  string `json:"auth_index"`
			ModelGroup string `json:"model_group"`
		}
		if err := json.Unmarshal(request.Body, &action); err != nil {
			return envelopeManagement(guardManagementJSON(http.StatusBadRequest, map[string]string{"error": err.Error()}), nil)
		}
		r.guardRuntime.access.RLock()
		defer r.guardRuntime.access.RUnlock()
		engine, _ := r.guardRuntime.currentEngine()
		readyAt, err := engine.ManualHalfOpen(action.AuthIndex, guard.ModelGroup(action.ModelGroup), r.clock.Now().UTC())
		if err != nil {
			return envelopeManagement(guardManagementJSON(http.StatusBadRequest, map[string]string{"error": err.Error()}), nil)
		}
		r.guardRuntime.signalPersist()
		return envelopeManagement(guardManagementJSON(http.StatusOK, map[string]any{"ok": true, "ready_at": readyAt}), nil)
	default:
		return envelopeManagement(guardManagementJSON(http.StatusNotFound, map[string]string{"error": "route not found"}), nil)
	}
}

func (r *Runtime) guardStatusResponse() []byte {
	r.guardRuntime.access.RLock()
	defer r.guardRuntime.access.RUnlock()
	engine, _ := r.guardRuntime.currentEngine()
	payload := map[string]any{"configured": false}
	if engine != nil {
		cfg := r.currentConfig()
		persistErr, probeErr := r.guardRuntime.errors()
		hostFeatures := r.currentHostFeatures()
		payload = map[string]any{
			"configured":             true,
			"mode":                   cfg.Guard.Mode,
			"config":                 cfg.Guard.Dynamic(),
			"required_scheduler_for": append([]string(nil), cfg.RequiredSchedulerFor...),
			"host_features":          sortedFeatureNames(hostFeatures),
			"required_scheduler_abi": requiredSchedulerABIReady(cfg, hostFeatures),
			"readiness":              r.guardRuntime.readiness(r.clock.Now().UTC(), cfg.Guard.EvidenceMaxAge),
			"enforcement_ready":      r.guardRuntime.enforcementIsReady(),
			"snapshot":               engine.Snapshot(r.clock.Now().UTC()),
			"last_persist_error":     persistErr,
			"last_probe_error":       probeErr,
		}
	}
	return envelopeManagement(guardManagementJSON(http.StatusOK, payload), nil)
}

func sortedFeatureNames(features map[string]struct{}) []string {
	out := make([]string, 0, len(features))
	for feature := range features {
		out = append(out, feature)
	}
	sort.Strings(out)
	return out
}

func requiredSchedulerABIReady(cfg config.Config, features map[string]struct{}) bool {
	if !containsProvider(cfg.RequiredSchedulerFor, "antigravity") {
		return false
	}
	for _, feature := range []string{
		hostFeatureRequiredSchedulerV1,
		hostFeatureSchedulerRequestIDV1,
		hostFeatureSchedulerDirectResponseV1,
		hostFeatureAuthInventoryReadyV1,
	} {
		if _, ok := features[feature]; !ok {
			return false
		}
	}
	return true
}

func guardManagementJSON(status int, value any) ManagementResponse {
	body, _ := json.Marshal(value)
	return guardManagementBytes(status, "application/json", body)
}

func guardManagementBytes(status int, contentType string, body []byte) ManagementResponse {
	headers := map[string][]string{
		"Content-Type":           {contentType},
		"Cache-Control":          {"no-store"},
		"Pragma":                 {"no-cache"},
		"Referrer-Policy":        {"no-referrer"},
		"X-Content-Type-Options": {"nosniff"},
	}
	if strings.HasPrefix(strings.ToLower(contentType), "text/html") {
		headers["Content-Security-Policy"] = []string{"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'"}
	}
	return ManagementResponse{
		StatusCode:  status,
		ContentType: contentType,
		Headers:     headers,
		Body:        base64.StdEncoding.EncodeToString(body),
	}
}
