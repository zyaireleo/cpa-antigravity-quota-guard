package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/apply"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/evidence"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/host"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/management"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/state"
)

const maxRunHistory = 10

type quotaPreview struct {
	ID              string
	ModelGroup      config.AntigravityModelGroup
	AuthScope       string
	HostFingerprint string
	EvidenceByGroup map[config.AntigravityModelGroup]evidence.Result
}

// Runtime manages plugin lifecycle, configuration, quota probing, and single-flight execution.
type Runtime struct {
	mu                 sync.Mutex
	runMu              sync.Mutex
	configUpdateMu     sync.Mutex
	runner             TaskRunner
	rootCtx            context.Context
	cancel             context.CancelFunc
	cfg                config.Config
	hostCallbacks      host.HostCallbacks
	clock              Clock
	sleeper            Sleeper
	management         *management.Handler
	latestResult       apply.Result
	latestAudit        string
	latestDualSnapshot *apply.DualGroupSnapshot
	latestQuotaPreview *quotaPreview
	scheduleConfig     state.ScheduleConfig
	stateCacheOverride string
	guardStateOverride string
	runHistory         []RunHistoryEntry
	guardRuntime       *guardRuntimeState
	hostFeatures       map[string]struct{}
	schedulerPickHook  func(string)
	shutdown           bool
}

// New creates an initialized Runtime instance.
func New(options Options) *Runtime {
	clock := options.Clock
	if clock == nil {
		clock = realRuntimeClock{}
	}
	sleeper := options.Sleeper
	if sleeper == nil {
		sleeper = realSleeper{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := &Runtime{
		rootCtx:       ctx,
		cancel:        cancel,
		cfg:           config.Default(),
		hostCallbacks: options.Host,
		clock:         clock,
		sleeper:       sleeper,
	}
	if strings.TrimSpace(options.StateCachePath) != "" {
		rt.cfg.StateCachePath = options.StateCachePath
		rt.stateCacheOverride = options.StateCachePath
	}
	if strings.TrimSpace(options.GuardStatePath) != "" {
		rt.cfg.Guard.StatePath = options.GuardStatePath
		rt.guardStateOverride = options.GuardStatePath
	}
	if options.Runner != nil {
		rt.runner = options.Runner
	} else {
		rt.runner = rt.runProductionTask
	}
	rt.management = management.NewHandler(managementRunner{runtime: rt})
	rt.guardRuntime = newGuardRuntimeState()

	// Restore persisted cache, learned rates, and execution snapshot from disk on startup
	cachePath := rt.cfg.StateCachePath
	if strings.TrimSpace(cachePath) == "" {
		cachePath = config.DefaultStateCachePath
	}
	if store, err := state.Load(context.Background(), cachePath); err == nil {
		audit, resJSON, histJSON := store.GetRuntimeSnapshot()
		if len(resJSON) > 0 {
			var res apply.Result
			if err := json.Unmarshal(resJSON, &res); err == nil {
				rt.latestResult = res
			}
		}
		if len(histJSON) > 0 {
			var hist []RunHistoryEntry
			if err := json.Unmarshal(histJSON, &hist); err == nil {
				rt.runHistory = hist
			}
		}
		if audit != "" {
			rt.latestAudit = audit
		}
		rt.scheduleConfig = store.GetScheduleConfig()
		if dynCfg, ok := store.GetDynamicConfig(); ok {
			if merged, err := dynCfg.ApplyTo(rt.cfg); err == nil {
				rt.cfg = merged
				rt.scheduleConfig = merged.Schedule
			}
		}
		// Unconditionally ensure cache file exists on disk upon initialization
		_ = store.SaveAtomic(context.Background())
	}

	return rt
}

// Handle routes CPA JSON-RPC method calls to their respective handlers.
func (r *Runtime) Handle(ctx context.Context, method string, request []byte) []byte {
	switch method {
	case MethodPluginRegister:
		parsed, err := decodeRegisterRequest(request)
		if err != nil {
			return failure(err)
		}
		result, err := r.Register(ctx, parsed)
		return envelopeRegister(result, err)
	case MethodPluginReconfigure:
		parsed, err := decodeReconfigureRequest(request)
		if err != nil {
			return failure(err)
		}
		result, err := r.Reconfigure(ctx, parsed)
		return envelopeRegister(result, err)
	case MethodPluginShutdown:
		return envelopeStatus(r.Shutdown(ctx))
	case MethodManagementRegister:
		return r.registerGuardManagement(request)
	case MethodManagementHandle:
		return r.handleGuardManagement(ctx, request)
	case MethodSchedulerPick:
		return r.handleSchedulerPick(ctx, request)
	case MethodUsageHandle:
		return r.handleUsage(ctx, request)
	case MethodRequestBefore:
		return r.handleRequestBefore(ctx, request)
	case MethodRequestAfter:
		return r.handleRequestAfter(ctx, request)
	default:
		return failure(fmt.Errorf("%w: method %q", ErrInvalidRequest, method))
	}
}

// Register initializes the plugin with configuration received from CPA and starts scheduled workers.
func (r *Runtime) Register(ctx context.Context, req RegisterRequest) (RegisterResult, error) {
	r.configUpdateMu.Lock()
	defer r.configUpdateMu.Unlock()

	cfg, _, err := config.LoadBytes([]byte(req.ConfigYAML))
	if err != nil {
		return RegisterResult{}, fmt.Errorf("load register config: %w", err)
	}
	cfg = r.applyStateCacheOverride(cfg)
	cfg = r.applyGuardStateOverride(cfg)
	cfg = r.mergePersistedDynamicConfig(cfg)
	features := normalizedHostFeatures(req.HostFeatures)
	r.guardRuntime.access.Lock()
	defer r.guardRuntime.access.Unlock()
	preparedGuard, err := r.prepareGuard(ctx, cfg, features, false)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("configure quota guard: %w", err)
	}
	if err := r.replaceConfig(ctx, cfg, features); err != nil {
		return RegisterResult{}, err
	}
	r.installPreparedGuard(preparedGuard, cfg.Guard)
	return registrationResult(), nil
}

// Reconfigure updates runtime configuration dynamically and adjusts scheduled workers.
func (r *Runtime) Reconfigure(ctx context.Context, req ReconfigureRequest) (RegisterResult, error) {
	r.configUpdateMu.Lock()
	defer r.configUpdateMu.Unlock()

	cfg, _, err := config.LoadBytes([]byte(req.ConfigYAML))
	if err != nil {
		return RegisterResult{}, fmt.Errorf("load reconfigure config: %w", err)
	}
	cfg = r.applyStateCacheOverride(cfg)
	cfg = r.applyGuardStateOverride(cfg)
	cfg = r.mergePersistedDynamicConfig(cfg)
	features := normalizedHostFeatures(req.HostFeatures)
	r.guardRuntime.access.Lock()
	defer r.guardRuntime.access.Unlock()
	if err := r.guardRuntime.persist(); err != nil {
		return RegisterResult{}, fmt.Errorf("persist active quota guard before host reconfigure: %w", err)
	}
	preparedGuard, err := r.prepareGuard(ctx, cfg, features, false)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("configure quota guard: %w", err)
	}
	if err := r.replaceConfig(ctx, cfg, features); err != nil {
		return RegisterResult{}, err
	}
	r.installPreparedGuard(preparedGuard, cfg.Guard)
	return registrationResult(), nil
}

func (r *Runtime) applyStateCacheOverride(cfg config.Config) config.Config {
	r.mu.Lock()
	override := r.stateCacheOverride
	r.mu.Unlock()
	if strings.TrimSpace(override) != "" && cfg.StateCachePath == config.DefaultStateCachePath {
		cfg.StateCachePath = override
	}
	return cfg
}

func (r *Runtime) applyGuardStateOverride(cfg config.Config) config.Config {
	r.mu.Lock()
	override := r.guardStateOverride
	r.mu.Unlock()
	if strings.TrimSpace(override) != "" {
		cfg.Guard.StatePath = override
	}
	return cfg
}

// ManualApply is retained as a compatibility stub. Quota Guard never writes
// CPA auth priority or disabled fields.
func (r *Runtime) ManualApply(ctx context.Context, modelGroup config.AntigravityModelGroup, authIndexes []string) error {
	return ErrLegacyMutationDisabled
}

// ManualApplyWithPreview is retained as a compatibility stub.
func (r *Runtime) ManualApplyWithPreview(ctx context.Context, modelGroup config.AntigravityModelGroup, authIndexes []string, previewID string) error {
	return ErrLegacyMutationDisabled
}

// Probe triggers a probe-only execution: fetches fresh quota and updates the cache without planning or applying.
func (r *Runtime) Probe(ctx context.Context, modelGroup config.AntigravityModelGroup, authIndexes []string) error {
	return r.run(ctx, TriggerProbe, modelGroup, authIndexes)
}

// ResetAllPriorities is retained as a compatibility stub.
func (r *Runtime) ResetAllPriorities(ctx context.Context) (map[string]any, error) {
	return nil, ErrLegacyMutationDisabled
}

// SyncHost re-reads credentials from CPA host, re-evaluates cached evidence, and updates the dual-group snapshot.
func (r *Runtime) SyncHost(ctx context.Context, modelGroup config.AntigravityModelGroup) (apply.DualGroupSnapshot, error) {
	if !r.runMu.TryLock() {
		return apply.DualGroupSnapshot{}, ErrRunInProgress
	}
	defer r.runMu.Unlock()

	r.mu.Lock()
	cfg := r.cfg
	r.mu.Unlock()

	if r.hostCallbacks == nil {
		return apply.DualGroupSnapshot{}, errMissingHostCallbacks
	}
	// The dashboard selector is view-only; Dynamic Config is the control authority.
	_ = modelGroup

	client := host.NewClient(r.hostCallbacks)
	files, err := client.ListAuthFiles(ctx)
	if err != nil {
		return apply.DualGroupSnapshot{}, err
	}

	credentials := credentialsFromAuthFiles(files)
	credentials, _, err = enrichCredentialsFromAuthDocuments(ctx, client, credentials)
	if err != nil {
		return apply.DualGroupSnapshot{}, err
	}

	cachePath := cfg.StateCachePath
	if strings.TrimSpace(cachePath) == "" {
		cachePath = config.DefaultStateCachePath
	}
	store, err := state.Load(ctx, cachePath)
	if err != nil {
		return apply.DualGroupSnapshot{}, err
	}
	evidenceByGroup := buildProjectionEvidence(store, credentials)

	now := r.clock.Now().UTC()
	projection, err := ProjectDualModelGroups(ProjectionInput{
		ControlModelGroup: cfg.AntigravityModelGroup,
		Credentials:       credentials,
		EvidenceByGroup:   evidenceByGroup,
		PlanningOptions:   priorityOptions(cfg, store, now),
		ProjectionTime:    now,
	})
	if err != nil {
		return apply.DualGroupSnapshot{}, err
	}
	// Host synchronization only refreshes the projection. It must not consume
	// an unused Fresh Evidence preview; ManualApplyWithPreview still validates
	// the preview's host fingerprint before any transition is executed.
	if preview := r.currentQuotaPreview(); preview != nil {
		projection.Snapshot.PreviewID = preview.ID
	}
	r.setDualSnapshot(projection.Snapshot)
	return cloneDualGroupSnapshot(projection.Snapshot), nil
}

// GetSamples returns the historical quota samples for a specific credential and model group.
func (r *Runtime) GetSamples(ctx context.Context, authIndex, modelGroup string) ([]state.QuotaSample, error) {
	r.mu.Lock()
	cfg := r.cfg
	r.mu.Unlock()

	cachePath := cfg.StateCachePath
	if strings.TrimSpace(cachePath) == "" {
		cachePath = config.DefaultStateCachePath
	}
	store, err := state.Load(ctx, cachePath)
	if err != nil {
		return nil, err
	}
	return store.GetSamples(authIndex, modelGroup), nil
}

// GetProbeSamples returns only samples appended by one probe round.
func (r *Runtime) GetProbeSamples(ctx context.Context, probeRoundID, modelGroup string) ([]state.ProbeSampleRecord, error) {
	r.mu.Lock()
	cfg := r.cfg
	r.mu.Unlock()

	cachePath := cfg.StateCachePath
	if strings.TrimSpace(cachePath) == "" {
		cachePath = config.DefaultStateCachePath
	}
	store, err := state.Load(ctx, cachePath)
	if err != nil {
		return nil, err
	}
	return store.GetSamplesByProbeRound(probeRoundID, modelGroup), nil
}

// AutoApply is retained as a compatibility stub.
func (r *Runtime) AutoApply(ctx context.Context) error {
	return ErrLegacyMutationDisabled
}

func (r *Runtime) run(ctx context.Context, trigger Trigger, modelGroup config.AntigravityModelGroup, authIndexes []string) error {
	return r.runWithPreview(ctx, trigger, modelGroup, authIndexes, "", false)
}

func (r *Runtime) runWithPreview(ctx context.Context, trigger Trigger, modelGroup config.AntigravityModelGroup, authIndexes []string, previewID string, previewRequired bool) error {
	if !r.runMu.TryLock() {
		return ErrRunInProgress
	}
	defer r.runMu.Unlock()

	taskCtx, cleanup, cfg, runner, err := r.taskContext(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	if !cfg.Enabled && trigger != TriggerManualApply {
		return errors.New("plugin is disabled")
	}

	if err := runner(taskCtx, TaskRequest{
		Config:          cfg,
		Trigger:         trigger,
		AuthIndexes:     append([]string(nil), authIndexes...),
		PreviewID:       strings.TrimSpace(previewID),
		PreviewRequired: previewRequired,
	}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("run %s: %w", trigger, err)
	}
	return nil
}

// Config returns the current configuration snapshot.
func (r *Runtime) Config() (config.Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown {
		return config.Config{}, ErrShutdown
	}
	return r.cfg, nil
}

// Status returns current status summary for UI rendering.
func (r *Runtime) Status(ctx context.Context) (management.StatusInfo, error) {
	cfg, err := r.Config()
	if err != nil {
		return management.StatusInfo{}, err
	}
	latestAudit := "runtime management API ready"
	if !cfg.Enabled {
		latestAudit = "runtime management API disabled by config"
	}
	_, audit := r.currentRunSnapshot()
	if audit != "" {
		latestAudit = audit
	}
	return management.StatusInfo{
		LatestAudit: latestAudit,
	}, nil
}

// LatestSnapshot returns the most recently generated dual-group plan snapshot.
func (r *Runtime) LatestSnapshot(ctx context.Context) (apply.DualGroupSnapshot, error) {
	if _, err := r.Config(); err != nil {
		return apply.DualGroupSnapshot{}, err
	}
	r.mu.Lock()
	snap := r.latestDualSnapshot
	r.mu.Unlock()
	if snap != nil {
		return cloneDualGroupSnapshot(*snap), nil
	}
	// Startup fallback: generate the stable empty shape through the same
	// projection seam until a shared projection is available.
	cfg, _ := r.Config()
	now := r.clock.Now().UTC()
	projection, err := ProjectDualModelGroups(ProjectionInput{
		ControlModelGroup: cfg.AntigravityModelGroup,
		ProjectionTime:    now,
	})
	if err != nil {
		return apply.DualGroupSnapshot{}, err
	}
	r.setDualSnapshot(projection.Snapshot)
	return cloneDualGroupSnapshot(projection.Snapshot), nil
}

// Diagnostics returns a comprehensive diagnostics map.
func (r *Runtime) Diagnostics(ctx context.Context) (map[string]any, error) {
	cfg, err := r.Config()
	if err != nil {
		return nil, err
	}
	result, audit := r.currentRunSnapshot()
	r.mu.Lock()
	sched := r.scheduleConfig
	r.mu.Unlock()

	activeCooldowns := make([]map[string]any, 0)
	cachePath := cfg.StateCachePath
	if strings.TrimSpace(cachePath) == "" {
		cachePath = config.DefaultStateCachePath
	}
	if store, err := state.Load(ctx, cachePath); err == nil {
		now := r.clock.Now().UTC()
		for _, c := range store.GetCooldowns() {
			if now.Before(c.CooldownUntil) {
				activeCooldowns = append(activeCooldowns, map[string]any{
					"auth_index":     redactRuntimeIdentifier(c.AuthIndex),
					"model_group":    c.ModelGroup,
					"triggered_at":   c.TriggeredAt.Format(time.RFC3339),
					"cooldown_until": c.CooldownUntil.Format(time.RFC3339),
					"reason":         c.Reason,
				})
			}
		}
	}

	return map[string]any{
		"management_api": map[string]any{
			"status":     "ready",
			"auto_apply": false,
			"enabled":    cfg.Enabled,
		},
		"scheduler": map[string]any{
			"legacy_auto_apply": false,
			"probe_interval":    cfg.Guard.ProbeInterval.String(),
			"worker_active":     r.guardRuntime != nil,
			"paused":            sched.Paused,
			"window_enabled":    sched.WindowEnabled,
			"window_start":      sched.WindowStart,
			"window_end":        sched.WindowEnd,
		},
		"active_cooldowns": activeCooldowns,
		"latest_audit":     audit,
		"last_result":      result,
		"latest_apply":     r.latestApplyEntry(),
		"run_history":      r.currentRunHistory(),
	}, nil
}

// Shutdown terminates runtime workers and marks runtime as shutdown.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.configUpdateMu.Lock()
	defer r.configUpdateMu.Unlock()

	r.mu.Lock()
	if r.shutdown {
		r.mu.Unlock()
		return nil
	}
	r.shutdown = true
	r.cancel()
	guardRuntime := r.guardRuntime
	r.mu.Unlock()
	return guardRuntime.stop(ctx)
}

func (r *Runtime) replaceConfig(ctx context.Context, cfg config.Config, hostFeatures map[string]struct{}) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("runtime configure context: %w", err)
	}

	r.mu.Lock()
	if r.shutdown {
		r.mu.Unlock()
		return ErrShutdown
	}
	r.cfg = cfg
	r.hostFeatures = cloneFeatureSet(hostFeatures)
	r.mu.Unlock()
	return nil
}

func normalizedHostFeatures(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func cloneFeatureSet(values map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for value := range values {
		out[value] = struct{}{}
	}
	return out
}

func (r *Runtime) currentHostFeatures() map[string]struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneFeatureSet(r.hostFeatures)
}

func (r *Runtime) taskContext(ctx context.Context) (context.Context, func(), config.Config, TaskRunner, error) {
	r.mu.Lock()
	if r.shutdown {
		r.mu.Unlock()
		return nil, nil, config.Config{}, nil, ErrShutdown
	}
	rootCtx, cfg, runner := r.rootCtx, r.cfg, r.runner
	r.mu.Unlock()

	taskCtx, cancel := context.WithCancel(rootCtx)
	stop := context.AfterFunc(ctx, cancel)
	cleanup := func() {
		stop()
		cancel()
	}
	return taskCtx, cleanup, cfg, runner, nil
}

func (r *Runtime) snapshotRunEntry(result apply.Result, audit string, entry RunHistoryEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latestResult = result
	r.latestAudit = audit
	if entry.At.IsZero() {
		entry.At = r.clock.Now().UTC()
	}
	if entry.Kind == "" {
		entry.Kind = KindApply
	}
	history := make([]RunHistoryEntry, 0, maxRunHistory)
	history = append(history, entry)
	for i := 0; i < len(r.runHistory) && len(history) < maxRunHistory; i++ {
		history = append(history, r.runHistory[i])
	}
	r.runHistory = history
}

func (r *Runtime) snapshotLatestResult(result apply.Result, audit string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latestResult = result
	r.latestAudit = audit
}

func (r *Runtime) setDualSnapshot(snap apply.DualGroupSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cloned := cloneDualGroupSnapshot(snap)
	r.latestDualSnapshot = &cloned
}

func (r *Runtime) currentSnapshotEmails() map[string]string {
	r.mu.Lock()
	snapshot := r.latestDualSnapshot
	r.mu.Unlock()
	if snapshot == nil {
		return nil
	}
	emails := make(map[string]string)
	for _, group := range snapshot.Groups {
		for _, item := range group.Items {
			if item.Identity.AuthIndex == "" || item.Identity.Email == "" {
				continue
			}
			emails[item.Identity.AuthIndex] = item.Identity.Email
		}
	}
	return emails
}

func (r *Runtime) setQuotaPreview(preview quotaPreview) {
	cloned := cloneQuotaPreview(preview)
	r.mu.Lock()
	r.latestQuotaPreview = &cloned
	r.mu.Unlock()
}

func (r *Runtime) currentQuotaPreview() *quotaPreview {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latestQuotaPreview == nil {
		return nil
	}
	cloned := cloneQuotaPreview(*r.latestQuotaPreview)
	return &cloned
}

func cloneQuotaPreview(preview quotaPreview) quotaPreview {
	cloned := preview
	cloned.EvidenceByGroup = make(map[config.AntigravityModelGroup]evidence.Result, len(preview.EvidenceByGroup))
	for group, result := range preview.EvidenceByGroup {
		cloned.EvidenceByGroup[group] = cloneEvidence(result)
	}
	return cloned
}

// GetScheduleConfig returns the current dynamic schedule configuration.
func (r *Runtime) GetScheduleConfig(ctx context.Context) (config.ScheduleConfig, error) {
	if _, err := r.Config(); err != nil {
		return config.ScheduleConfig{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.scheduleConfig, nil
}

// SetScheduleConfig updates the dynamic schedule configuration and persists it to the state store.
func (r *Runtime) SetScheduleConfig(ctx context.Context, cfg config.ScheduleConfig) error {
	if err := config.ValidateScheduleWindow(cfg.WindowStart, cfg.WindowEnd); err != nil {
		return err
	}
	runtimeCfg, err := r.Config()
	if err != nil {
		return err
	}
	cachePath := runtimeCfg.StateCachePath
	if strings.TrimSpace(cachePath) == "" {
		cachePath = config.DefaultStateCachePath
	}
	store, err := state.Load(ctx, cachePath)
	if err != nil {
		return fmt.Errorf("load state for schedule config: %w", err)
	}
	store.SetScheduleConfig(cfg)
	if err := store.SaveAtomic(ctx); err != nil {
		return fmt.Errorf("persist schedule config: %w", err)
	}
	r.mu.Lock()
	r.scheduleConfig = cfg
	r.cfg.Schedule = cfg
	r.mu.Unlock()
	return nil
}

// GetDynamicConfig returns the active dynamic configuration.
func (r *Runtime) GetDynamicConfig(ctx context.Context) (config.DynamicConfig, error) {
	r.mu.Lock()
	cfg := r.cfg
	sched := r.scheduleConfig
	r.mu.Unlock()

	cachePath := cfg.StateCachePath
	if strings.TrimSpace(cachePath) == "" {
		cachePath = config.DefaultStateCachePath
	}
	store, err := state.Load(ctx, cachePath)
	if err == nil {
		if dyn, ok := store.GetDynamicConfig(); ok {
			// Never expose a persisted legacy document that the current runtime
			// would reject (notably auto_apply=true). Return its canonical,
			// validated form or fall back to the active in-memory configuration.
			if merged, applyErr := dyn.ApplyTo(cfg); applyErr == nil {
				canonical := merged.Dynamic()
				canonical.Schedule = sched
				return canonical, nil
			}
		}
	}

	dyn := cfg.Dynamic()
	dyn.Schedule = sched
	return dyn, nil
}

// SetDynamicConfig validates, persists, and hot-applies new dynamic configuration without restarting.
func (r *Runtime) SetDynamicConfig(ctx context.Context, dyn config.DynamicConfig) error {
	r.configUpdateMu.Lock()
	defer r.configUpdateMu.Unlock()

	r.mu.Lock()
	baseCfg := r.cfg
	cachePath := r.cfg.StateCachePath
	if r.shutdown {
		r.mu.Unlock()
		return ErrShutdown
	}
	r.mu.Unlock()

	newCfg, err := dyn.ApplyTo(baseCfg)
	if err != nil {
		return err
	}
	// Exclude scheduler, usage, probe evidence, roster, and management mutations
	// across the final flush/load/swap transaction. Otherwise an event accepted
	// after the flush could land in the retiring generation and be lost.
	r.guardRuntime.access.Lock()
	defer r.guardRuntime.access.Unlock()

	// Flush any dirty breaker state before loading the prepared generation so a
	// configuration-only update cannot regress to an older on-disk snapshot.
	if r.guardRuntime != nil {
		if err := r.guardRuntime.persist(); err != nil {
			return fmt.Errorf("persist active quota guard before reconfigure: %w", err)
		}
	}
	preparedGuard, err := r.prepareGuard(ctx, newCfg, r.currentHostFeatures(), true)
	if err != nil {
		return fmt.Errorf("configure quota guard: %w", err)
	}

	if strings.TrimSpace(cachePath) == "" {
		cachePath = config.DefaultStateCachePath
	}

	store, err := state.Load(ctx, cachePath)
	if err != nil {
		return fmt.Errorf("load state for save: %w", err)
	}
	// Persist the canonical validated document before changing any active
	// runtime state. From this point onward the in-memory commit path is
	// deliberately non-cancellable and has no expected validation or IO errors.
	persisted := newCfg.Dynamic()
	store.SetDynamicConfig(persisted)
	store.SetScheduleConfig(persisted.Schedule)
	if err := store.SaveAtomic(ctx); err != nil {
		return fmt.Errorf("save dynamic config: %w", err)
	}

	// All remaining steps are in-memory, non-fallible commits. Shutdown and
	// competing configuration updates are excluded by configUpdateMu.
	r.installPreparedGuard(preparedGuard, newCfg.Guard)
	r.mu.Lock()
	r.cfg = newCfg
	r.scheduleConfig = persisted.Schedule
	r.mu.Unlock()
	return nil
}

func (r *Runtime) mergePersistedDynamicConfig(baseCfg config.Config) config.Config {
	cachePath := baseCfg.StateCachePath
	if strings.TrimSpace(cachePath) == "" {
		cachePath = config.DefaultStateCachePath
	}
	store, err := state.Load(context.Background(), cachePath)
	if err != nil {
		return baseCfg
	}
	dynCfg, ok := store.GetDynamicConfig()
	if !ok {
		return baseCfg
	}
	merged, err := dynCfg.ApplyTo(baseCfg)
	if err != nil {
		return baseCfg
	}
	return merged
}

func (r *Runtime) currentRunSnapshot() (apply.Result, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latestResult, r.latestAudit
}

func (r *Runtime) currentRunHistory() []RunHistoryEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RunHistoryEntry, len(r.runHistory))
	copy(out, r.runHistory)
	return out
}

func (r *Runtime) latestApplyEntry() *RunHistoryEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.runHistory {
		if entry.Kind == KindApply {
			copy := entry
			return &copy
		}
	}
	return nil
}

func registrationResult() RegisterResult {
	return RegisterResult{
		SchemaVersion: 3,
		Metadata:      buildMetadata(),
		Capabilities: map[string]bool{
			"management_api":      true,
			"scheduler":           true,
			"usage_plugin":        true,
			"request_interceptor": true,
		},
	}
}

func redactRuntimeIdentifier(value string) string {
	if len(value) <= 4 {
		return "***"
	}
	return value[:2] + "***" + value[len(value)-2:]
}

func buildMetadata() Metadata {
	return Metadata{
		Name:             "CPA Antigravity Quota Guard",
		Version:          "0.1.1",
		Author:           "zyaireleo",
		GitHubRepository: "https://github.com/zyaireleo/cpa-antigravity-quota-guard",
		Description:      "Per-account, per-model-group quota circuit breaker for Google Antigravity credentials in CLIProxyAPI.",
	}
}

type realRuntimeClock struct{}

func (realRuntimeClock) Now() time.Time {
	return time.Now()
}

type realSleeper struct{}

func (realSleeper) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
