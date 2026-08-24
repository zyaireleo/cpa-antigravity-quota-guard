// Package guard implements the model-group-aware Antigravity quota circuit
// breaker used by the CPA scheduler and usage-plugin adapters.
package guard

import (
	"errors"
	"net/http"
	"time"
)

const SchemaVersion = 1

var (
	ErrInvalidConfig   = errors.New("guard: invalid config")
	ErrInvalidIdentity = errors.New("guard: invalid identity")
	ErrInvalidEvidence = errors.New("guard: invalid quota evidence")
	ErrUnsafeStatePath = errors.New("guard: unsafe state path")
	ErrInvalidState    = errors.New("guard: invalid persisted state")
)

// ModelGroup is an independently enforced Antigravity quota pool.
type ModelGroup string

const (
	ModelGroupGemini    ModelGroup = "gemini"
	ModelGroupClaudeGPT ModelGroup = "claude_gpt"
)

func (g ModelGroup) Valid() bool {
	return g == ModelGroupGemini || g == ModelGroupClaudeGPT
}

// State is the breaker state for one (auth_index, model_group) tuple.
type State string

const (
	StateUninitialized State = "uninitialized"
	StateClosed        State = "closed"
	StateOpen          State = "open"
	StateHalfOpen      State = "half_open"
)

func (s State) Valid() bool {
	switch s {
	case StateUninitialized, StateClosed, StateOpen, StateHalfOpen:
		return true
	default:
		return false
	}
}

// Reason records why a breaker is not in its normal closed state.
type Reason string

const (
	ReasonNone        Reason = ""
	ReasonQuotaZero   Reason = "quota_zero"
	ReasonExplicit429 Reason = "explicit_429"
	ReasonGeneric429  Reason = "generic_429"
	ReasonManualProbe Reason = "manual_probe"
)

func (r Reason) Valid() bool {
	switch r {
	case ReasonNone, ReasonQuotaZero, ReasonExplicit429, ReasonGeneric429, ReasonManualProbe:
		return true
	default:
		return false
	}
}

// Mode controls whether Pick only reports a projected decision or enforces it.
type Mode string

const (
	ModeObserve Mode = "observe"
	ModeEnforce Mode = "enforce"
)

// Policy controls behavior when a model or auth identity is unknown.
type Policy string

const (
	PolicyFailClosed Policy = "fail_closed"
	PolicyFailOpen   Policy = "fail_open"
)

// Config contains only breaker-domain settings. The runtime is responsible
// for mapping its public configuration into this type.
type Config struct {
	Generic429Threshold       int
	Generic429Window          time.Duration
	Generic429InitialCooldown time.Duration
	Generic429MaxCooldown     time.Duration
	HalfOpenLease             time.Duration
	EvidenceMaxAge            time.Duration
	UnknownAuthPolicy         Policy
	UnknownModelPolicy        Policy
}

func DefaultConfig() Config {
	return Config{
		Generic429Threshold:       2,
		Generic429Window:          time.Minute,
		Generic429InitialCooldown: 15 * time.Minute,
		Generic429MaxCooldown:     30 * time.Minute,
		HalfOpenLease:             30 * time.Second,
		EvidenceMaxAge:            30 * time.Minute,
		UnknownAuthPolicy:         PolicyFailClosed,
		UnknownModelPolicy:        PolicyFailClosed,
	}
}

// Identity is the non-secret roster information needed to fence persisted
// state from a credential that was replaced at the same auth index.
type Identity struct {
	AuthID              string `json:"auth_id"`
	AuthIndex           string `json:"auth_index"`
	IdentityFingerprint string `json:"identity_fingerprint"`
	Provider            string `json:"provider,omitempty"`
	Priority            int    `json:"priority"`
}

// Entry is the persisted state for one credential and model group. It never
// contains tokens, request bodies, or upstream response bodies.
type Entry struct {
	AuthID              string     `json:"auth_id"`
	AuthIndex           string     `json:"auth_index"`
	IdentityFingerprint string     `json:"identity_fingerprint"`
	Provider            string     `json:"provider,omitempty"`
	Priority            int        `json:"priority"`
	ModelGroup          ModelGroup `json:"model_group"`
	State               State      `json:"state"`
	Reason              Reason     `json:"reason,omitempty"`
	OpenedAt            time.Time  `json:"opened_at,omitempty"`
	RecoverAt           time.Time  `json:"recover_at,omitempty"`
	ResetAt             time.Time  `json:"reset_at,omitempty"`
	FailureCount        int        `json:"failure_count,omitempty"`
	LastFailureAt       time.Time  `json:"last_failure_at,omitempty"`
	HalfOpenStartedAt   time.Time  `json:"half_open_started_at,omitempty"`
	HalfOpenLeaseUntil  time.Time  `json:"half_open_lease_until,omitempty"`
	LastEvidenceSource  string     `json:"last_evidence_source,omitempty"`
	LastEvidenceAt      time.Time  `json:"last_evidence_at,omitempty"`
	LastQuotaEvidenceAt time.Time  `json:"last_quota_evidence_at,omitempty"`
	RemainingPercent    *float64   `json:"remaining_percent,omitempty"`
	halfOpenRequestID   string
}

// Document is the on-disk representation. Entries are sorted before saving so
// releases and support bundles produce stable, reviewable JSON.
type Document struct {
	SchemaVersion int       `json:"schema_version"`
	UpdatedAt     time.Time `json:"updated_at"`
	Entries       []Entry   `json:"entries"`
}

type UsageObservation struct {
	RequestID   string
	Provider    string
	Model       string
	AuthID      string
	AuthIndex   string
	Failed      bool
	StatusCode  int
	Body        string
	Headers     http.Header
	RequestedAt time.Time
	ObservedAt  time.Time
}

type ObservationResult struct {
	Handled bool
	Changed bool
	Group   ModelGroup
	State   State
	Reason  Reason
}

type QuotaEvidence struct {
	AuthIndex           string
	AuthID              string
	IdentityFingerprint string
	Group               ModelGroup
	Remaining           float64
	ResetAt             time.Time
	ObservedAt          time.Time
	Source              string
}

type EvidenceResult struct {
	Changed bool
	Ignored bool
	State   State
	Reason  Reason
}

type Candidate struct {
	AuthID    string
	AuthIndex string
	Provider  string
	Priority  int
}

type PickRequest struct {
	Now                time.Time
	RequestID          string
	Provider           string
	Providers          []string
	Model              string
	Candidates         []Candidate
	Mode               Mode
	EnforcedGroups     []ModelGroup
	UnknownAuthPolicy  Policy
	UnknownModelPolicy Policy
}

type PickResult struct {
	SelectedAuthID    string        `json:"selected_auth_id,omitempty"`
	SelectedAuthIndex string        `json:"selected_auth_index,omitempty"`
	Handled           bool          `json:"handled"`
	ObserveOnly       bool          `json:"observe_only,omitempty"`
	AllCooling        bool          `json:"all_cooling,omitempty"`
	RetryAfter        time.Duration `json:"retry_after,omitempty"`
	ErrorCode         string        `json:"error_code,omitempty"`
	Group             ModelGroup    `json:"model_group,omitempty"`
	Excluded          int           `json:"excluded,omitempty"`
}

const (
	ErrorCodeModelCooldown     = "model_cooldown"
	ErrorCodeQuotaGroupUnknown = "quota_group_unknown"
	ErrorCodeAuthUnknown       = "auth_unknown"
	ErrorCodeAuthUninitialized = "auth_uninitialized"
	ErrorCodeNoEligibleAuth    = "no_eligible_auth"
	ErrorCodeGuardNotReady     = "quota_guard_not_ready"
)

type Metrics struct {
	CooldownOpen           int     `json:"cooldown_open"`
	CooldownOpenedTotal    uint64  `json:"cooldown_opened_total"`
	SchedulerExcludedTotal uint64  `json:"scheduler_excluded_total"`
	SchedulerPickTotal     uint64  `json:"scheduler_pick_total"`
	FailClosedTotal        uint64  `json:"fail_closed_total"`
	HalfOpenAttemptTotal   uint64  `json:"half_open_attempt_total"`
	HalfOpenSuccessTotal   uint64  `json:"half_open_success_total"`
	HalfOpenFailureTotal   uint64  `json:"half_open_failure_total"`
	QuotaProbeSuccessTotal uint64  `json:"quota_probe_success_total"`
	QuotaProbeErrorTotal   uint64  `json:"quota_probe_error_total"`
	QuotaEvidenceStale     int     `json:"quota_evidence_stale"`
	StatePersistErrorTotal uint64  `json:"state_persist_error_total"`
	UnknownAuthTotal       uint64  `json:"unknown_auth_total"`
	UnknownModelTotal      uint64  `json:"unknown_model_total"`
	UsageEventDirtyBacklog uint64  `json:"usage_event_dirty_backlog"`
	SchedulerLatencyMillis float64 `json:"scheduler_latency_ms"`
}

// SelectionEvent is the latest Antigravity-aware scheduling decision exposed
// for diagnostics. It contains only non-secret routing metadata and is not
// persisted across process restarts.
type SelectionEvent struct {
	At                time.Time  `json:"at"`
	RequestID         string     `json:"request_id,omitempty"`
	Provider          string     `json:"provider,omitempty"`
	Providers         []string   `json:"providers,omitempty"`
	Model             string     `json:"model,omitempty"`
	ModelGroup        ModelGroup `json:"model_group,omitempty"`
	Mode              Mode       `json:"mode"`
	SelectedAuthID    string     `json:"selected_auth_id,omitempty"`
	SelectedAuthIndex string     `json:"selected_auth_index,omitempty"`
	Handled           bool       `json:"handled"`
	ObserveOnly       bool       `json:"observe_only,omitempty"`
	ErrorCode         string     `json:"error_code,omitempty"`
	Excluded          int        `json:"excluded,omitempty"`
}

type Snapshot struct {
	GeneratedAt     time.Time                `json:"generated_at"`
	Entries         []Entry                  `json:"entries"`
	EarliestRecover map[ModelGroup]time.Time `json:"earliest_recover,omitempty"`
	Metrics         Metrics                  `json:"metrics"`
	LastSelection   *SelectionEvent          `json:"last_selection,omitempty"`
}
