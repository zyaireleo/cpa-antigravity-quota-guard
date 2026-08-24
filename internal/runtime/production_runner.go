package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/apply"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/core"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/evidence"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/guard"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/host"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/priority"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/state"
)

var errMissingHostCallbacks = errors.New("runtime: host callbacks are required")
var errAuthInventoryNotReady = errors.New("runtime: host auth inventory is not ready")

const (
	autoQuotaProbeAttempts = 3
	autoQuotaProbeDelay    = 5 * time.Second
	defaultProbeCacheTTL   = 15 * time.Minute
)

func (r *Runtime) runProductionTask(ctx context.Context, request TaskRequest) error {
	if request.Trigger != TriggerProbe {
		return ErrLegacyMutationDisabled
	}
	if !request.Config.Enabled {
		return nil
	}
	if r.hostCallbacks == nil {
		return errMissingHostCallbacks
	}
	now := r.clock.Now().UTC()
	client := host.NewClient(r.hostCallbacks)
	inventory, err := client.ListAuthInventory(ctx)
	if err != nil {
		return err
	}
	if !inventory.Ready {
		return errAuthInventoryNotReady
	}
	files := inventory.Files
	probedIdentities := guardIdentitiesByIndex(files)
	ignoredGuardEvidence := disabledGuardAuthIndexes(files)
	if err := r.updateGuardRosterFromFiles(files); err != nil {
		return fmt.Errorf("refresh quota guard roster before probe: %w", err)
	}

	credentials := credentialsFromAuthFiles(files)
	credentials = filterCredentialsByAuthIndex(credentials, request.AuthIndexes)
	cachePath := request.Config.StateCachePath
	if strings.TrimSpace(cachePath) == "" {
		cachePath = config.DefaultStateCachePath
	}

	credentials, authMaterials, err := enrichCredentialsFromAuthDocuments(ctx, client, credentials)
	if err != nil {
		return err
	}

	store, err := state.Load(ctx, cachePath)
	if err != nil {
		return err
	}

	evidence, err := r.collectEvidenceForTrigger(ctx, collectInput{
		client:         client,
		store:          store,
		credentials:    credentials,
		authMaterials:  authMaterials,
		now:            now,
		cacheTTL:       defaultProbeCacheTTL,
		forceProbe:     true,
		maxConcurrency: request.Config.MaxConcurrency,
		modelGroup:     request.Config.AntigravityModelGroup,
		sampleCapacity: request.Config.QuotaSampleCapacity,
	}, TriggerProbe)
	if err != nil {
		return err
	}
	// Re-read the authoritative roster after the network round. A credential may
	// have been replaced at the same auth_index while its old token was in
	// flight. ReplaceRoster resets the new identity to uninitialized and the
	// captured AuthID/fingerprint below prevents old evidence from crossing it.
	postProbeInventory, err := client.ListAuthInventory(ctx)
	if err != nil {
		return fmt.Errorf("refresh quota guard roster after probe: %w", err)
	}
	if !postProbeInventory.Ready {
		return errAuthInventoryNotReady
	}
	postProbeFiles := postProbeInventory.Files
	if err := r.updateGuardRosterFromFiles(postProbeFiles); err != nil {
		return fmt.Errorf("reconcile quota guard roster after probe: %w", err)
	}
	r.applyGuardEvidence(evidence.ByGroup, probedIdentities, ignoredGuardEvidence)

	if err := store.SaveAtomic(ctx); err != nil {
		return err
	}

	// Reconcile against the authoritative Host inventory after the potentially
	// slow Google requests. Credentials may have been added, removed, disabled,
	// or reprioritized while probing was in flight.
	projection, err := projectCurrentHost(ctx, client, postProbeFiles, request, evidence.ByGroup, store, now)
	if err != nil {
		return err
	}
	primarySnapshot := projection.ControlSnapshot
	previewID := evidence.RoundID
	r.setQuotaPreview(quotaPreview{
		ID:              previewID,
		ModelGroup:      request.Config.AntigravityModelGroup,
		AuthScope:       authScopeKey(request.AuthIndexes),
		HostFingerprint: hostFingerprint(primarySnapshot.Items),
		EvidenceByGroup: evidence.ByGroup,
	})
	projection.Snapshot.PreviewID = previewID
	r.setDualSnapshot(projection.Snapshot)
	result := apply.Result{Snapshot: primarySnapshot}
	audit := fmt.Sprintf("probe completed: %d probe observations", evidence.Probed)
	_, projectErr := r.projectRun(ctx, store, result, audit, RunHistoryEntry{
		Kind:         KindProbe,
		Trigger:      string(TriggerProbe),
		ProbeRoundID: evidence.RoundID,
		Attempted:    evidence.Probed,
		Succeeded:    len(evidence.ByGroup[request.Config.AntigravityModelGroup].Eligible),
		Message:      audit,
		Snapshot:     &primarySnapshot,
	})
	return projectErr
}

func authScopeKey(authIndexes []string) string {
	if len(authIndexes) == 0 {
		return "*"
	}
	unique := make(map[string]struct{}, len(authIndexes))
	for _, authIndex := range authIndexes {
		if trimmed := strings.TrimSpace(authIndex); trimmed != "" {
			unique[trimmed] = struct{}{}
		}
	}
	values := make([]string, 0, len(unique))
	for authIndex := range unique {
		values = append(values, authIndex)
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func hostFingerprint(items []apply.SnapshotItem) string {
	type hostState struct {
		AuthIndex       string
		Priority        int
		PriorityMissing bool
		Disabled        bool
	}
	states := make([]hostState, 0, len(items))
	for _, item := range items {
		states = append(states, hostState{
			AuthIndex:       item.Identity.AuthIndex,
			Priority:        item.Current.Priority,
			PriorityMissing: item.Current.PriorityMissing,
			Disabled:        item.Current.Disabled,
		})
	}
	sort.Slice(states, func(left, right int) bool {
		return states[left].AuthIndex < states[right].AuthIndex
	})
	hash := sha256.New()
	for _, state := range states {
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%t\x00%t\n", state.AuthIndex, state.Priority, state.PriorityMissing, state.Disabled)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func projectCurrentHost(
	ctx context.Context,
	client *host.Client,
	files []host.AuthFile,
	request TaskRequest,
	evidenceByGroup map[config.AntigravityModelGroup]evidence.Result,
	store *state.Store,
	now time.Time,
) (DualModelGroupProjection, error) {
	credentials := credentialsFromAuthFiles(files)
	credentials = filterCredentialsByAuthIndex(credentials, request.AuthIndexes)
	credentials, _, err := enrichCredentialsFromAuthDocuments(ctx, client, credentials)
	if err != nil {
		return DualModelGroupProjection{}, err
	}
	return ProjectDualModelGroups(ProjectionInput{
		ControlModelGroup: request.Config.AntigravityModelGroup,
		Credentials:       credentials,
		EvidenceByGroup:   evidenceByGroup,
		PlanningOptions:   priorityOptions(request.Config, store, now),
		ProjectionTime:    now,
	})
}

func (r *Runtime) collectEvidenceForTrigger(ctx context.Context, input collectInput, trigger Trigger) (collectedEvidence, error) {
	if trigger != TriggerAutoApply {
		return collectFreshEvidence(ctx, input)
	}
	var collected collectedEvidence
	for attempt := 1; attempt <= autoQuotaProbeAttempts; attempt++ {
		current, err := collectFreshEvidence(ctx, input)
		if err != nil {
			return collectedEvidence{}, err
		}
		collected = current
		if !hasProbeFailure(current, input.modelGroup) || attempt == autoQuotaProbeAttempts {
			return collected, nil
		}
		input.forceProbe = true
		if err := r.sleeper.Sleep(ctx, autoQuotaProbeDelay); err != nil {
			return collectedEvidence{}, err
		}
	}
	return collected, nil
}

func hasProbeFailure(collected collectedEvidence, modelGroup config.AntigravityModelGroup) bool {
	for _, observation := range collected.Observations {
		if observation.ModelGroup == modelGroup && (observation.Kind == evidence.ObservationFailed || observation.Kind == evidence.ObservationInvalid) {
			return true
		}
	}
	return false
}

func credentialsFromAuthFiles(files []host.AuthFile) []core.Credential {
	credentials := make([]core.Credential, 0, len(files))
	for _, file := range files {
		if isAntigravityAuthFile(file) {
			credentials = append(credentials, core.Credential{
				Name:            file.Name,
				AuthIndex:       file.AuthIndex,
				Provider:        core.ProviderAntigravity,
				Type:            core.CredentialTypeAntigravity,
				Status:          core.CredentialStatus(file.Status),
				Disabled:        file.Disabled,
				Unavailable:     file.Unavailable,
				Priority:        file.Priority,
				PriorityMissing: file.PriorityMissing,
				Account:         file.Account,
				Email:           file.Email,
				PlanType:        core.PlanTypeUnknown,
			})
		}
	}
	return credentials
}

func isAntigravityAuthFile(file host.AuthFile) bool {
	provider := strings.ToLower(strings.TrimSpace(file.Provider))
	credType := strings.ToLower(strings.TrimSpace(file.Type))
	name := strings.ToLower(strings.TrimSpace(file.Name))
	return guard.IsAntigravityProvider(provider) || strings.Contains(credType, "antigravity") ||
		strings.Contains(name, "antigravity")
}

func filterCredentialsByAuthIndex(credentials []core.Credential, authIndexes []string) []core.Credential {
	if len(authIndexes) == 0 {
		return credentials
	}
	allowed := make(map[string]struct{}, len(authIndexes))
	for _, authIndex := range authIndexes {
		allowed[authIndex] = struct{}{}
	}
	filtered := make([]core.Credential, 0, len(credentials))
	for _, credential := range credentials {
		if _, ok := allowed[credential.AuthIndex]; ok {
			filtered = append(filtered, credential)
		}
	}
	return filtered
}

func priorityOptions(cfg config.Config, store *state.Store, now time.Time) priority.Options {
	var cooldowns map[string]time.Time
	if store != nil {
		cooldowns = store.GetActiveCooldowns(now)
	}
	return priority.Options{
		Now:                 now,
		BoostStartPriority:  cfg.PriorityRules.BoostStartPriority,
		NormalStartPriority: cfg.PriorityRules.NormalStartPriority,
		MinChange:           cfg.MinChange,
		UrgencyTolerance:    cfg.UrgencyTolerance,
		IgnoreDisabledHost:  cfg.IgnoreDisabledHost,
		CooldownAuthIndexes: cooldowns,
	}
}

func buildProjectionEvidence(store *state.Store, credentials []core.Credential) map[config.AntigravityModelGroup]evidence.Result {
	historical := historicalObservations(store, credentials)
	result := make(map[config.AntigravityModelGroup]evidence.Result, 2)
	for _, group := range []config.AntigravityModelGroup{
		config.AntigravityModelGroupGemini,
		config.AntigravityModelGroupClaudeGPT,
	} {
		classified := evidence.Classify(evidence.Input{
			Round:       evidence.Round{ID: "historical", ModelGroup: group},
			Credentials: credentials,
			Historical:  historical,
		})
		result[group] = classified
	}
	return result
}
