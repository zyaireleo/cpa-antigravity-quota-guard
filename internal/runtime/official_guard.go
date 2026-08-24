package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/evidence"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/guard"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/host"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/priority"
)

const guardPersistInterval = time.Second

const (
	hostFeatureRequiredSchedulerV1       = "required_scheduler_v1"
	hostFeatureSchedulerRequestIDV1      = "scheduler_request_id_v1"
	hostFeatureSchedulerDirectResponseV1 = "scheduler_direct_response_v1"
	hostFeatureAuthInventoryReadyV1      = "auth_inventory_ready_v1"
)

type preparedGuard struct {
	engine         *guard.Engine
	inventoryReady bool
	baselineReady  bool
}

type guardRuntimeState struct {
	mu             sync.RWMutex
	access         sync.RWMutex
	persistMu      sync.Mutex
	workers        sync.WaitGroup
	generation     uint64
	engine         *guard.Engine
	config         config.GuardConfig
	path           string
	persistSignal  chan struct{}
	cancel         context.CancelFunc
	done           chan struct{}
	lastPersistErr string
	lastProbeErr   string
	inventoryReady bool
	baselineReady  bool
}

func newGuardRuntimeState() *guardRuntimeState {
	engine, _ := guard.New(guard.DefaultConfig())
	return &guardRuntimeState{
		engine:        engine,
		config:        config.Default().Guard,
		path:          config.DefaultGuardStatePath,
		persistSignal: make(chan struct{}, 1),
	}
}

// prepareGuard performs every fallible guard operation without changing the
// active runtime generation. Dynamic configuration can therefore validate and
// persist successfully before its in-memory switch becomes visible.
func (r *Runtime) prepareGuard(ctx context.Context, cfg config.Config, hostFeatures map[string]struct{}, requireReady bool) (*preparedGuard, error) {
	if r.guardRuntime == nil {
		r.guardRuntime = newGuardRuntimeState()
	}
	engine, err := guard.Load(cfg.Guard.StatePath, runtimeGuardConfig(cfg.Guard))
	if err != nil {
		return nil, err
	}
	inventoryReady := true
	var inventory host.AuthInventory
	if _, supported := hostFeatures[hostFeatureAuthInventoryReadyV1]; supported && r.hostCallbacks != nil {
		loaded, errInventory := host.NewClient(r.hostCallbacks).ListAuthInventory(ctx)
		if errInventory != nil {
			if cfg.Guard.Mode == config.GuardModeEnforce {
				return nil, errInventory
			}
			inventoryReady = false
		} else {
			inventory = loaded
			inventoryReady = loaded.Ready
		}
	}
	if cfg.Guard.Mode == config.GuardModeEnforce {
		if !containsProvider(cfg.RequiredSchedulerFor, "antigravity") {
			return nil, fmt.Errorf("enforce mode requires host config required-scheduler-for: [antigravity]")
		}
		for _, feature := range []string{
			hostFeatureRequiredSchedulerV1,
			hostFeatureSchedulerRequestIDV1,
			hostFeatureSchedulerDirectResponseV1,
			hostFeatureAuthInventoryReadyV1,
		} {
			if _, ok := hostFeatures[feature]; !ok {
				return nil, fmt.Errorf("enforce mode requires CPA host feature %q", feature)
			}
		}
		if r.hostCallbacks == nil {
			return nil, errMissingHostCallbacks
		}
		if inventoryReady {
			if err := replaceGuardRosterFromFiles(engine, inventory.Files); err != nil {
				return nil, err
			}
		}
		if err := guard.VerifyStatePathWritable(cfg.Guard.StatePath); err != nil {
			return nil, fmt.Errorf("verify writable quota guard state: %w", err)
		}
	}
	prepared := &preparedGuard{engine: engine, inventoryReady: inventoryReady}
	readiness := preparedGuardReadiness(prepared, r.clock.Now().UTC(), cfg.Guard.EvidenceMaxAge)
	prepared.baselineReady = readiness.Ready
	if !prepared.baselineReady && inventoryReady && r.guardRuntime.enforcementIsReady() && readiness.UniformPriority {
		// Once a running enforce generation has established a fresh baseline,
		// later probe failures or newly added uninitialized credentials must not
		// disable healthy existing credentials. Per-entry scheduling remains
		// fail-closed for the new/unknown entries.
		prepared.baselineReady = true
	}
	if cfg.Guard.Mode == config.GuardModeEnforce && requireReady {
		if cfg.Guard.RequireUniformPriority && !readiness.UniformPriority {
			return nil, fmt.Errorf("enforce mode requires uniform priority: %s", strings.Join(readiness.Reasons, ","))
		}
		if !readiness.Ready {
			return nil, fmt.Errorf("enforce mode requires fresh baseline evidence: %s", strings.Join(readiness.Reasons, ","))
		}
	}
	return prepared, nil
}

func preparedGuardReadiness(prepared *preparedGuard, now time.Time, maxAge time.Duration) guard.Readiness {
	if prepared == nil || prepared.engine == nil {
		return guard.Readiness{Ready: false, UniformPriority: true, Reasons: []string{"guard_not_configured"}}
	}
	readiness := prepared.engine.Ready(now, maxAge)
	if !prepared.inventoryReady {
		readiness.Ready = false
		readiness.Reasons = append(readiness.Reasons, "auth_inventory_not_ready")
		sort.Strings(readiness.Reasons)
	}
	return readiness
}

func (s *guardRuntimeState) readiness(now time.Time, maxAge time.Duration) guard.Readiness {
	if s == nil {
		return guard.Readiness{Ready: false, UniformPriority: true, Reasons: []string{"guard_not_configured"}}
	}
	s.mu.RLock()
	prepared := &preparedGuard{engine: s.engine, inventoryReady: s.inventoryReady}
	s.mu.RUnlock()
	return preparedGuardReadiness(prepared, now, maxAge)
}

func (s *guardRuntimeState) enforcementIsReady() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	ready := s.inventoryReady && s.baselineReady
	s.mu.RUnlock()
	return ready
}

func (s *guardRuntimeState) enforcementInventoryReady() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	ready := s.inventoryReady
	s.mu.RUnlock()
	return ready
}

func (s *guardRuntimeState) promoteEnforcementReady(now time.Time, maxAge time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	engine := s.engine
	inventoryReady := s.inventoryReady
	s.mu.Unlock()
	if engine == nil || !inventoryReady || !engine.Ready(now, maxAge).Ready {
		return
	}
	s.mu.Lock()
	if s.engine == engine && s.inventoryReady {
		s.baselineReady = true
	}
	s.mu.Unlock()
}

func containsProvider(values []string, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, value := range values {
		if strings.ToLower(strings.TrimSpace(value)) == want {
			return true
		}
	}
	return false
}

func (r *Runtime) installPreparedGuard(prepared *preparedGuard, cfg config.GuardConfig) {
	// prepareGuard guarantees both values. Keeping this commit operation
	// infallible is what lets SetDynamicConfig persist before switching without
	// creating a post-persistence partial-failure branch.
	if prepared == nil || prepared.engine == nil || r.guardRuntime == nil {
		return
	}
	r.guardRuntime.replace(r.rootCtx, r, prepared.engine, cfg, prepared.inventoryReady, prepared.baselineReady)
	if prepared.engine.IsDirty() {
		r.guardRuntime.signalPersist()
	}
}

func runtimeGuardConfig(cfg config.GuardConfig) guard.Config {
	return guard.Config{
		Generic429Threshold:       cfg.Generic429.Threshold,
		Generic429Window:          cfg.Generic429.Window,
		Generic429InitialCooldown: cfg.Generic429.InitialCooldown,
		Generic429MaxCooldown:     cfg.Generic429.MaxCooldown,
		HalfOpenLease:             cfg.HalfOpenLease,
		EvidenceMaxAge:            cfg.EvidenceMaxAge,
		UnknownAuthPolicy:         guard.Policy(cfg.UnknownAuthPolicy),
		UnknownModelPolicy:        guard.Policy(cfg.UnknownModelPolicy),
	}
}

func (s *guardRuntimeState) replace(root context.Context, runtime *Runtime, engine *guard.Engine, cfg config.GuardConfig, inventoryReady, baselineReady bool) {
	if s == nil || engine == nil {
		return
	}
	// Serialize the generation swap with state-file writes. An old generation
	// already writing is allowed to finish before the swap; an old generation
	// that reaches persistence afterward observes the new token and is skipped,
	// so it cannot overwrite new state with a stale snapshot.
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	workerCtx, cancel := context.WithCancel(root)
	done := make(chan struct{})
	persistSignal := make(chan struct{}, 1)
	s.mu.Lock()
	oldCancel := s.cancel
	s.generation++
	generation := s.generation
	s.engine = engine
	s.config = cfg
	s.path = cfg.StatePath
	s.persistSignal = persistSignal
	s.cancel = cancel
	s.done = done
	s.lastPersistErr = ""
	s.lastProbeErr = ""
	s.inventoryReady = inventoryReady
	s.baselineReady = baselineReady
	s.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		s.run(workerCtx, runtime, done, generation, engine, cfg, cfg.StatePath, persistSignal)
	}()
	// The active generation is already switched. Retire the previous worker
	// without placing a fallible wait in the configuration commit path.
	if oldCancel != nil {
		oldCancel()
	}
}

func (s *guardRuntimeState) run(
	ctx context.Context,
	runtime *Runtime,
	done chan struct{},
	generation uint64,
	engine *guard.Engine,
	cfg config.GuardConfig,
	statePath string,
	persistSignal <-chan struct{},
) {
	defer close(done)
	persistTicker := time.NewTicker(guardPersistInterval)
	defer persistTicker.Stop()

	probeInterval := cfg.ProbeInterval
	if probeInterval <= 0 {
		probeInterval = 15 * time.Minute
	}
	probeTicker := time.NewTicker(probeInterval)
	defer probeTicker.Stop()
	startupProbe := time.NewTimer(2 * time.Second)
	defer startupProbe.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = s.persistEngine(generation, engine, statePath)
			return
		case <-persistSignal:
			_ = s.persistEngine(generation, engine, statePath)
		case <-persistTicker.C:
			_ = s.persistEngine(generation, engine, statePath)
		case <-startupProbe.C:
			s.runProbe(ctx, runtime, generation)
		case <-probeTicker.C:
			s.runProbe(ctx, runtime, generation)
		}
	}
}

func (s *guardRuntimeState) runProbe(ctx context.Context, runtime *Runtime, generation uint64) {
	if runtime == nil || !s.generationCanProbe(generation) || !runtime.currentConfig().Enabled {
		return
	}
	err := runtime.Probe(ctx, config.AntigravityModelGroupGemini, nil)
	s.mu.Lock()
	if generation != s.generation {
		s.mu.Unlock()
		return
	}
	if err != nil {
		s.lastProbeErr = err.Error()
	} else {
		s.lastProbeErr = ""
	}
	s.mu.Unlock()
}

func (s *guardRuntimeState) generationCanProbe(generation uint64) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	ready := generation == s.generation && s.inventoryReady
	s.mu.RUnlock()
	return ready
}

func (s *guardRuntimeState) stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.done = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	allDone := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(allDone)
	}()
	select {
	case <-allDone:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait quota guard worker: %w", ctx.Err())
	}
}

func (s *guardRuntimeState) persist() error {
	engine, path, generation := s.currentEngineGeneration()
	return s.persistEngine(generation, engine, path)
}

func (s *guardRuntimeState) persistEngine(generation uint64, engine *guard.Engine, path string) error {
	if engine == nil || !engine.IsDirty() {
		return nil
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.RLock()
	current := generation == s.generation
	s.mu.RUnlock()
	if !current {
		return nil
	}
	err := engine.Save(path)
	s.mu.Lock()
	if generation != s.generation {
		s.mu.Unlock()
		return err
	}
	if err != nil {
		s.lastPersistErr = err.Error()
	} else {
		s.lastPersistErr = ""
	}
	s.mu.Unlock()
	return err
}

func (s *guardRuntimeState) signalPersist() {
	if s == nil {
		return
	}
	s.mu.RLock()
	persistSignal := s.persistSignal
	s.mu.RUnlock()
	if persistSignal == nil {
		return
	}
	select {
	case persistSignal <- struct{}{}:
	default:
	}
}

func (s *guardRuntimeState) currentEngine() (*guard.Engine, string) {
	if s == nil {
		return nil, ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.engine, s.path
}

func (s *guardRuntimeState) currentEngineGeneration() (*guard.Engine, string, uint64) {
	if s == nil {
		return nil, "", 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.engine, s.path, s.generation
}

func (s *guardRuntimeState) currentConfig() config.GuardConfig {
	if s == nil {
		return config.Default().Guard
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *guardRuntimeState) errors() (persistErr, probeErr string) {
	if s == nil {
		return "", ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastPersistErr, s.lastProbeErr
}

func (r *Runtime) currentConfig() config.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

func (r *Runtime) refreshGuardRosterInto(ctx context.Context, engine *guard.Engine) error {
	if engine == nil {
		return errors.New("quota guard engine is nil")
	}
	if r.hostCallbacks == nil {
		return errMissingHostCallbacks
	}
	inventory, err := host.NewClient(r.hostCallbacks).ListAuthInventory(ctx)
	if err != nil {
		return err
	}
	if !inventory.Ready {
		return errAuthInventoryNotReady
	}
	return replaceGuardRosterFromFiles(engine, inventory.Files)
}

func replaceGuardRosterFromFiles(engine *guard.Engine, files []host.AuthFile) error {
	if engine == nil {
		return errors.New("quota guard engine is nil")
	}
	identities := make([]guard.Identity, 0, len(files))
	for _, file := range files {
		identity, ok := guardIdentityFromFile(file)
		if !ok {
			continue
		}
		identities = append(identities, identity)
	}
	return engine.ReplaceRoster(identities)
}

func guardIdentityFromFile(file host.AuthFile) (guard.Identity, bool) {
	if !isAntigravityAuthFile(file) || file.Disabled {
		return guard.Identity{}, false
	}
	authID := strings.TrimSpace(file.ID)
	if authID == "" {
		authID = strings.TrimSpace(file.Name)
	}
	if authID == "" || strings.TrimSpace(file.AuthIndex) == "" {
		return guard.Identity{}, false
	}
	return guard.Identity{
		AuthID:              authID,
		AuthIndex:           strings.TrimSpace(file.AuthIndex),
		IdentityFingerprint: guardIdentityFingerprint(file),
		Provider:            "antigravity",
		Priority:            file.Priority,
	}, true
}

func guardIdentitiesByIndex(files []host.AuthFile) map[string]guard.Identity {
	identities := make(map[string]guard.Identity, len(files))
	for _, file := range files {
		identity, ok := guardIdentityFromFile(file)
		if ok {
			identities[identity.AuthIndex] = identity
		}
	}
	return identities
}

func (r *Runtime) updateGuardRosterFromFiles(files []host.AuthFile) error {
	if r.guardRuntime == nil {
		return nil
	}
	r.guardRuntime.access.RLock()
	defer r.guardRuntime.access.RUnlock()
	engine, _ := r.guardRuntime.currentEngine()
	if engine == nil {
		return nil
	}
	if err := replaceGuardRosterFromFiles(engine, files); err != nil {
		return err
	}
	if engine.IsDirty() {
		r.guardRuntime.signalPersist()
	}
	return nil
}

func (r *Runtime) refreshGuardRoster(ctx context.Context) error {
	r.guardRuntime.access.RLock()
	defer r.guardRuntime.access.RUnlock()
	engine, _ := r.guardRuntime.currentEngine()
	if engine == nil {
		return nil
	}
	if err := r.refreshGuardRosterInto(ctx, engine); err != nil {
		return err
	}
	r.guardRuntime.signalPersist()
	return nil
}

func guardIdentityFingerprint(file host.AuthFile) string {
	plain := strings.Join([]string{
		strings.TrimSpace(file.ID),
		strings.TrimSpace(file.AuthIndex),
		strings.TrimSpace(file.Name),
		strings.ToLower(strings.TrimSpace(file.Provider)),
		strings.TrimSpace(file.Account),
		strings.ToLower(strings.TrimSpace(file.Email)),
	}, "\x00")
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

func configuredGuardGroups(cfg config.GuardConfig) []guard.ModelGroup {
	groups := make([]guard.ModelGroup, 0, len(cfg.EnforcedGroups))
	for _, group := range cfg.EnforcedGroups {
		groups = append(groups, guard.ModelGroup(group))
	}
	return groups
}

func (r *Runtime) applyGuardEvidence(byGroup map[config.AntigravityModelGroup]evidence.Result, probedIdentities map[string]guard.Identity) {
	if r.guardRuntime == nil {
		return
	}
	r.guardRuntime.access.RLock()
	defer r.guardRuntime.access.RUnlock()
	engine, _ := r.guardRuntime.currentEngine()
	if engine == nil {
		return
	}
	changed := false
	for group, result := range byGroup {
		for _, item := range result.Eligible {
			identity, ok := probedIdentities[strings.TrimSpace(item.AuthIndex)]
			if !ok {
				engine.RecordQuotaProbeError()
				continue
			}
			remaining, resetAt, ok := normalizedGuardQuota(item)
			if !ok {
				continue
			}
			outcome, err := engine.ApplyQuotaEvidence(guard.QuotaEvidence{
				AuthIndex:           item.AuthIndex,
				AuthID:              identity.AuthID,
				IdentityFingerprint: identity.IdentityFingerprint,
				Group:               guard.ModelGroup(group),
				Remaining:           remaining,
				ResetAt:             resetAt,
				ObservedAt:          item.ObservedAt,
				Source:              "quota_probe",
			})
			if err == nil && outcome.Changed {
				changed = true
			}
		}
		for _, observation := range result.Observations {
			if observation.Kind == evidence.ObservationFailed || observation.Kind == evidence.ObservationInvalid {
				engine.RecordQuotaProbeError()
			}
		}
	}
	if changed || engine.IsDirty() {
		r.guardRuntime.signalPersist()
	}
	r.guardRuntime.promoteEnforcementReady(r.clock.Now().UTC(), r.currentConfig().Guard.EvidenceMaxAge)
}

func normalizedGuardQuota(item priority.QuotaEvidence) (float64, time.Time, bool) {
	remainingValues := make([]int64, 0, 3)
	if item.Remaining != nil {
		remainingValues = append(remainingValues, *item.Remaining)
	}
	if item.ShortWindowRemaining != nil {
		remainingValues = append(remainingValues, *item.ShortWindowRemaining)
	}
	if item.LongWindowRemaining != nil {
		remainingValues = append(remainingValues, *item.LongWindowRemaining)
	}
	if len(remainingValues) == 0 {
		return 0, time.Time{}, false
	}
	remaining := remainingValues[0]
	for _, value := range remainingValues[1:] {
		if value < remaining {
			remaining = value
		}
	}
	resetAt := time.Time{}
	if item.ResetAt != nil {
		resetAt = item.ResetAt.UTC()
	}
	if item.ShortWindowRemaining != nil && *item.ShortWindowRemaining <= 0 && item.ShortWindowResetAt != nil {
		resetAt = laterTime(resetAt, item.ShortWindowResetAt.UTC())
	}
	if item.LongWindowRemaining != nil && *item.LongWindowRemaining <= 0 && item.LongWindowResetAt != nil {
		resetAt = laterTime(resetAt, item.LongWindowResetAt.UTC())
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 100 {
		remaining = 100
	}
	return float64(remaining), resetAt, true
}

func laterTime(current, candidate time.Time) time.Time {
	if current.IsZero() || candidate.After(current) {
		return candidate
	}
	return current
}
