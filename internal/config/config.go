package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"strings"
	"time"
)

const (
	// PluginID is the identifier recognized by the CPA host.
	PluginID = "cpa-antigravity-quota-guard"
	// DirectoryName is the directory name convention for the plugin.
	DirectoryName = "cpa-antigravity-quota-guard"
	// DynamicLibraryBaseName is the base name of the dynamic shared library.
	DynamicLibraryBaseName = "cpa-antigravity-quota-guard"
	// CPAConfigKey is the configuration key under plugins.configs in CPA.
	CPAConfigKey = "cpa-antigravity-quota-guard"

	// DefaultStateCachePath is the legacy quota/evidence cache path. It must
	// remain separate from DefaultGuardStatePath because their schemas differ.
	DefaultStateCachePath = "data/cpa-antigravity-quota-guard/quota-cache.json"
	// DefaultGuardStatePath is the guard-owned breaker state document.
	DefaultGuardStatePath = "data/cpa-antigravity-quota-guard/state.json"
	// GuardStateDirectory is the only directory in which state_path may live.
	GuardStateDirectory = "data/cpa-antigravity-quota-guard"

	KeyEnabled                  = "enabled"
	KeyAutoApply                = "auto_apply"
	KeyInterval                 = "interval"
	KeyAntigravityModelGroup    = "antigravity_model_group"
	KeyMaxConcurrency           = "max_concurrency"
	KeyMinChange                = "min_change"
	KeyUrgencyTolerance         = "urgency_tolerance"
	KeyRateLimitCooldownMinutes = "rate_limit_cooldown_minutes"
	KeyQuotaSampleCapacity      = "quota_sample_capacity"
	KeyStateCachePath           = "state_cache_path"
	KeyPriorityRules            = "priority_rules"
	KeyBoostStartPriority       = "boost_start_priority"
	KeyNormalStartPriority      = "normal_start_priority"
	KeySchedule                 = "schedule"
	KeyPaused                   = "paused"
	KeyWindowEnabled            = "window_enabled"
	KeyWindowStart              = "window_start"
	KeyWindowEnd                = "window_end"

	KeyMode                   = "mode"
	KeyManagedAuth            = "managed_auth"
	KeyEnforcedGroups         = "enforced_groups"
	KeyRequireUniformPriority = "require_uniform_priority"
	KeyProbeInterval          = "probe_interval"
	KeyEvidenceMaxAge         = "evidence_max_age"
	KeyGeneric429             = "generic_429"
	KeyHalfOpenLease          = "half_open_lease"
	KeyUnknownAuthPolicy      = "unknown_auth_policy"
	KeyUnknownModelPolicy     = "unknown_model_policy"
	KeyStatePath              = "state_path"

	DefaultUrgencyTolerance         = 0.05
	DefaultRateLimitCooldownMinutes = 5
	DefaultQuotaSampleCapacity      = 6
	MinQuotaSampleCapacity          = 2
	MaxQuotaSampleCapacity          = 30
	MinMaxConcurrency               = 1
	MaxMaxConcurrency               = 32
	MaxMinChange                    = 100
	MaxUrgencyTolerance             = 0.5
	MinRateLimitCooldownMinutes     = 1
	MaxRateLimitCooldownMinutes     = 1440
	MinPriorityValue                = 1
	MaxPriorityValue                = 999

	GuardModeObserve = "observe"
	GuardModeEnforce = "enforce"

	ManagedAuthAllAntigravity = "all_antigravity"
	GuardPolicyFailClosed     = "fail_closed"

	MinGeneric429Threshold = 1
	MaxGeneric429Threshold = 10
)

// ErrInvalidConfig indicates configuration parsing or validation failure.
var ErrInvalidConfig = errors.New("config: invalid")

// Config represents the validated complete configuration. Legacy priority
// fields are retained while the new quota guard is introduced incrementally.
type Config struct {
	Enabled                  bool
	AutoApply                bool
	Interval                 time.Duration
	AntigravityModelGroup    AntigravityModelGroup
	MaxConcurrency           int
	MinChange                int
	UrgencyTolerance         float64
	RateLimitCooldownMinutes int
	QuotaSampleCapacity      int
	IgnoreDisabledHost       bool
	StateCachePath           string
	RequiredSchedulerFor     []string
	PriorityRules            PriorityRules
	Schedule                 ScheduleConfig
	Guard                    GuardConfig
}

// GuardConfig contains the validated quota-breaker configuration.
type GuardConfig struct {
	Mode                   string
	ManagedAuth            string
	EnforcedGroups         []AntigravityModelGroup
	RequireUniformPriority bool
	ProbeInterval          time.Duration
	EvidenceMaxAge         time.Duration
	Generic429             Generic429Config
	HalfOpenLease          time.Duration
	UnknownAuthPolicy      string
	UnknownModelPolicy     string
	StatePath              string
}

// Generic429Config defines the transient breaker policy for unstructured 429s.
type Generic429Config struct {
	Threshold       int
	Window          time.Duration
	InitialCooldown time.Duration
	MaxCooldown     time.Duration
}

// GuardDynamic is the JSON-facing form of GuardConfig. Durations remain
// strings so management API payloads stay explicit and human-readable.
type GuardDynamic struct {
	Mode                   string            `json:"mode"`
	ManagedAuth            string            `json:"managed_auth"`
	EnforcedGroups         []string          `json:"enforced_groups"`
	RequireUniformPriority bool              `json:"require_uniform_priority"`
	ProbeInterval          string            `json:"probe_interval"`
	EvidenceMaxAge         string            `json:"evidence_max_age"`
	Generic429             Generic429Dynamic `json:"generic_429"`
	HalfOpenLease          string            `json:"half_open_lease"`
	UnknownAuthPolicy      string            `json:"unknown_auth_policy"`
	UnknownModelPolicy     string            `json:"unknown_model_policy"`
	StatePath              string            `json:"state_path"`
}

// Generic429Dynamic is the JSON-facing form of Generic429Config.
type Generic429Dynamic struct {
	Threshold       int    `json:"threshold"`
	Window          string `json:"window"`
	InitialCooldown string `json:"initial_cooldown"`
	MaxCooldown     string `json:"max_cooldown"`
}

// PriorityRules contains the legacy priority scoring configuration.
type PriorityRules struct {
	BoostStartPriority  int
	NormalStartPriority int
}

// PriorityRulesConfig holds priority rule settings for DynamicConfig.
type PriorityRulesConfig struct {
	BoostStartPriority  int `json:"boost_start_priority"`
	NormalStartPriority int `json:"normal_start_priority"`
}

// UnmarshalJSON migrates legacy priority_rules.enabled documents. Custom
// values from enabled=false configurations were previously inactive, so they
// resolve to the canonical defaults instead of becoming active unexpectedly.
func (cfg *PriorityRulesConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		Enabled             *bool `json:"enabled"`
		BoostStartPriority  *int  `json:"boost_start_priority"`
		NormalStartPriority *int  `json:"normal_start_priority"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	defaults := defaultPriorityRules()
	cfg.BoostStartPriority = defaults.BoostStartPriority
	cfg.NormalStartPriority = defaults.NormalStartPriority
	if raw.Enabled != nil && !*raw.Enabled {
		return nil
	}
	if raw.BoostStartPriority != nil {
		cfg.BoostStartPriority = *raw.BoostStartPriority
	}
	if raw.NormalStartPriority != nil {
		cfg.NormalStartPriority = *raw.NormalStartPriority
	}
	return nil
}

func defaultPriorityRules() PriorityRules {
	return PriorityRules{BoostStartPriority: 999, NormalStartPriority: 100}
}

// DynamicConfig contains all runtime-customizable configuration parameters.
// Guard is nested in management JSON to keep the legacy and guard surfaces
// clearly separated; startup YAML/JSON accepts the guard keys at plugin level.
type DynamicConfig struct {
	AutoApply                bool                `json:"auto_apply"`
	Interval                 string              `json:"interval"`
	AntigravityModelGroup    string              `json:"antigravity_model_group"`
	MaxConcurrency           int                 `json:"max_concurrency"`
	MinChange                int                 `json:"min_change"`
	UrgencyTolerance         float64             `json:"urgency_tolerance"`
	RateLimitCooldownMinutes int                 `json:"rate_limit_cooldown_minutes"`
	QuotaSampleCapacity      int                 `json:"quota_sample_capacity"`
	IgnoreDisabledHost       bool                `json:"ignore_disabled_host"`
	PriorityRules            PriorityRulesConfig `json:"priority_rules"`
	Schedule                 ScheduleConfig      `json:"schedule"`
	Guard                    GuardDynamic        `json:"guard"`
}

// UnmarshalJSON seeds omitted fields from canonical defaults before applying
// persisted overrides. This preserves old dynamic documents without guard.
func (dyn *DynamicConfig) UnmarshalJSON(data []byte) error {
	type dynamicConfigAlias DynamicConfig
	seeded := dynamicConfigAlias(Default().Dynamic())
	if err := json.Unmarshal(data, &seeded); err != nil {
		return err
	}
	*dyn = DynamicConfig(seeded)
	return nil
}

type rawConfig struct {
	Enabled              *bool           `json:"enabled"`
	AutoApply            *bool           `json:"auto_apply"`
	StateCachePath       *string         `json:"state_cache_path"`
	RequiredSchedulerFor json.RawMessage `json:"required-scheduler-for"`

	Mode                   *string         `json:"mode"`
	ManagedAuth            *string         `json:"managed_auth"`
	EnforcedGroups         json.RawMessage `json:"enforced_groups"`
	RequireUniformPriority *bool           `json:"require_uniform_priority"`
	ProbeInterval          *string         `json:"probe_interval"`
	EvidenceMaxAge         *string         `json:"evidence_max_age"`
	Generic429             *rawGeneric429  `json:"generic_429"`
	HalfOpenLease          *string         `json:"half_open_lease"`
	UnknownAuthPolicy      *string         `json:"unknown_auth_policy"`
	UnknownModelPolicy     *string         `json:"unknown_model_policy"`
	StatePath              *string         `json:"state_path"`

	// Flat aliases keep the simple startup configuration compatible with early
	// Phase 0 drafts. Nested generic_429 is the canonical serialized form.
	Generic429Threshold       *int    `json:"generic_429_threshold"`
	Generic429Window          *string `json:"generic_429_window"`
	Generic429InitialCooldown *string `json:"generic_429_initial_cooldown"`
	Generic429MaxCooldown     *string `json:"generic_429_max_cooldown"`
}

type rawGeneric429 struct {
	Threshold       *int    `json:"threshold"`
	Window          *string `json:"window"`
	InitialCooldown *string `json:"initial_cooldown"`
	MaxCooldown     *string `json:"max_cooldown"`
}

// Default returns the standard default configuration values.
func Default() Config {
	return Config{
		Enabled:                  true,
		AutoApply:                false,
		Interval:                 15 * time.Minute,
		AntigravityModelGroup:    AntigravityModelGroupGemini,
		MaxConcurrency:           6,
		MinChange:                1,
		UrgencyTolerance:         DefaultUrgencyTolerance,
		RateLimitCooldownMinutes: DefaultRateLimitCooldownMinutes,
		QuotaSampleCapacity:      DefaultQuotaSampleCapacity,
		IgnoreDisabledHost:       true,
		StateCachePath:           DefaultStateCachePath,
		PriorityRules:            defaultPriorityRules(),
		Schedule: ScheduleConfig{
			Paused:        false,
			WindowEnabled: false,
			WindowStart:   "00:00",
			WindowEnd:     "23:59",
		},
		Guard: GuardConfig{
			Mode:                   GuardModeObserve,
			ManagedAuth:            ManagedAuthAllAntigravity,
			EnforcedGroups:         []AntigravityModelGroup{AntigravityModelGroupGemini, AntigravityModelGroupClaudeGPT},
			RequireUniformPriority: true,
			ProbeInterval:          15 * time.Minute,
			EvidenceMaxAge:         30 * time.Minute,
			Generic429: Generic429Config{
				Threshold:       2,
				Window:          time.Minute,
				InitialCooldown: 15 * time.Minute,
				MaxCooldown:     30 * time.Minute,
			},
			HalfOpenLease:      30 * time.Second,
			UnknownAuthPolicy:  GuardPolicyFailClosed,
			UnknownModelPolicy: GuardPolicyFailClosed,
			StatePath:          DefaultGuardStatePath,
		},
	}
}

// Dynamic returns the DynamicConfig view of the current Config.
func (cfg Config) Dynamic() DynamicConfig {
	return DynamicConfig{
		AutoApply:                cfg.AutoApply,
		Interval:                 cfg.Interval.String(),
		AntigravityModelGroup:    string(cfg.AntigravityModelGroup),
		MaxConcurrency:           cfg.MaxConcurrency,
		MinChange:                cfg.MinChange,
		UrgencyTolerance:         cfg.UrgencyTolerance,
		RateLimitCooldownMinutes: cfg.RateLimitCooldownMinutes,
		QuotaSampleCapacity:      cfg.QuotaSampleCapacity,
		IgnoreDisabledHost:       cfg.IgnoreDisabledHost,
		PriorityRules: PriorityRulesConfig{
			BoostStartPriority:  cfg.PriorityRules.BoostStartPriority,
			NormalStartPriority: cfg.PriorityRules.NormalStartPriority,
		},
		Schedule: cfg.Schedule,
		Guard:    cfg.Guard.Dynamic(),
	}
}

// Dynamic returns the management/API representation of the guard config.
func (cfg GuardConfig) Dynamic() GuardDynamic {
	groups := make([]string, 0, len(cfg.EnforcedGroups))
	for _, group := range cfg.EnforcedGroups {
		groups = append(groups, string(group))
	}
	return GuardDynamic{
		Mode:                   cfg.Mode,
		ManagedAuth:            cfg.ManagedAuth,
		EnforcedGroups:         groups,
		RequireUniformPriority: cfg.RequireUniformPriority,
		ProbeInterval:          cfg.ProbeInterval.String(),
		EvidenceMaxAge:         cfg.EvidenceMaxAge.String(),
		Generic429: Generic429Dynamic{
			Threshold:       cfg.Generic429.Threshold,
			Window:          cfg.Generic429.Window.String(),
			InitialCooldown: cfg.Generic429.InitialCooldown.String(),
			MaxCooldown:     cfg.Generic429.MaxCooldown.String(),
		},
		HalfOpenLease:      cfg.HalfOpenLease.String(),
		UnknownAuthPolicy:  cfg.UnknownAuthPolicy,
		UnknownModelPolicy: cfg.UnknownModelPolicy,
		StatePath:          cfg.StatePath,
	}
}

// Validate validates all legacy and guard dynamic field boundaries.
func (dyn DynamicConfig) Validate() error {
	if dyn.AutoApply {
		return fmt.Errorf("auto_apply is no longer supported: quota guard never writes host priority or disabled state")
	}
	interval, err := time.ParseDuration(dyn.Interval)
	if err != nil || interval <= 0 {
		return fmt.Errorf("invalid interval %q: must be positive duration (e.g. 15m)", dyn.Interval)
	}
	if interval < time.Minute {
		return fmt.Errorf("interval %s too short: minimum is 1m", dyn.Interval)
	}
	if _, err := ParseAntigravityModelGroup(dyn.AntigravityModelGroup); err != nil {
		return fmt.Errorf("invalid antigravity_model_group %q: must be 'gemini' or 'claude_gpt'", dyn.AntigravityModelGroup)
	}
	if dyn.MaxConcurrency < MinMaxConcurrency || dyn.MaxConcurrency > MaxMaxConcurrency {
		return fmt.Errorf("max_concurrency must be between %d and %d, got %d", MinMaxConcurrency, MaxMaxConcurrency, dyn.MaxConcurrency)
	}
	if dyn.MinChange < 0 || dyn.MinChange > MaxMinChange {
		return fmt.Errorf("min_change must be between 0 and %d, got %d", MaxMinChange, dyn.MinChange)
	}
	if math.IsNaN(dyn.UrgencyTolerance) || math.IsInf(dyn.UrgencyTolerance, 0) || dyn.UrgencyTolerance < 0 || dyn.UrgencyTolerance > MaxUrgencyTolerance {
		return fmt.Errorf("urgency_tolerance must be between 0 and %.1f, got %v", MaxUrgencyTolerance, dyn.UrgencyTolerance)
	}
	if dyn.RateLimitCooldownMinutes < MinRateLimitCooldownMinutes || dyn.RateLimitCooldownMinutes > MaxRateLimitCooldownMinutes {
		return fmt.Errorf("rate_limit_cooldown_minutes must be between %d and %d, got %d", MinRateLimitCooldownMinutes, MaxRateLimitCooldownMinutes, dyn.RateLimitCooldownMinutes)
	}
	if dyn.PriorityRules.BoostStartPriority < MinPriorityValue || dyn.PriorityRules.BoostStartPriority > MaxPriorityValue {
		return fmt.Errorf("boost_start_priority must be between %d and %d, got %d", MinPriorityValue, MaxPriorityValue, dyn.PriorityRules.BoostStartPriority)
	}
	if dyn.PriorityRules.NormalStartPriority < MinPriorityValue || dyn.PriorityRules.NormalStartPriority > MaxPriorityValue {
		return fmt.Errorf("normal_start_priority must be between %d and %d, got %d", MinPriorityValue, MaxPriorityValue, dyn.PriorityRules.NormalStartPriority)
	}
	if dyn.PriorityRules.NormalStartPriority > dyn.PriorityRules.BoostStartPriority {
		return fmt.Errorf("normal_start_priority must not exceed boost_start_priority, got %d > %d", dyn.PriorityRules.NormalStartPriority, dyn.PriorityRules.BoostStartPriority)
	}
	if dyn.QuotaSampleCapacity < MinQuotaSampleCapacity || dyn.QuotaSampleCapacity > MaxQuotaSampleCapacity {
		return fmt.Errorf("quota_sample_capacity must be between %d and %d, got %d", MinQuotaSampleCapacity, MaxQuotaSampleCapacity, dyn.QuotaSampleCapacity)
	}
	if err := ValidateScheduleWindow(dyn.Schedule.WindowStart, dyn.Schedule.WindowEnd); err != nil {
		return err
	}
	guard := dyn.Guard
	if guard.isZero() {
		guard = Default().Guard.Dynamic()
	}
	_, err = guard.toConfig()
	return err
}

// ApplyTo validates and applies dynamic configuration overrides on top of a base Config.
func (dyn DynamicConfig) ApplyTo(base Config) (Config, error) {
	if err := dyn.Validate(); err != nil {
		return base, err
	}

	interval, _ := time.ParseDuration(dyn.Interval)
	modelGroup, _ := ParseAntigravityModelGroup(dyn.AntigravityModelGroup)
	guard := dyn.Guard
	if guard.isZero() {
		guard = Default().Guard.Dynamic()
	}
	parsedGuard, _ := guard.toConfig()

	res := base
	res.AutoApply = dyn.AutoApply
	res.Interval = interval
	res.AntigravityModelGroup = modelGroup
	res.MaxConcurrency = dyn.MaxConcurrency
	res.MinChange = dyn.MinChange
	res.UrgencyTolerance = dyn.UrgencyTolerance
	res.RateLimitCooldownMinutes = dyn.RateLimitCooldownMinutes
	res.QuotaSampleCapacity = dyn.QuotaSampleCapacity
	res.IgnoreDisabledHost = dyn.IgnoreDisabledHost
	res.PriorityRules.BoostStartPriority = dyn.PriorityRules.BoostStartPriority
	res.PriorityRules.NormalStartPriority = dyn.PriorityRules.NormalStartPriority
	res.Schedule = dyn.Schedule
	res.Guard = parsedGuard
	return res, nil
}

func (dyn GuardDynamic) isZero() bool {
	return dyn.Mode == "" && dyn.ManagedAuth == "" && len(dyn.EnforcedGroups) == 0 &&
		!dyn.RequireUniformPriority && dyn.ProbeInterval == "" && dyn.EvidenceMaxAge == "" &&
		dyn.Generic429 == (Generic429Dynamic{}) && dyn.HalfOpenLease == "" &&
		dyn.UnknownAuthPolicy == "" && dyn.UnknownModelPolicy == "" && dyn.StatePath == ""
}

func (dyn GuardDynamic) toConfig() (GuardConfig, error) {
	groups := make([]AntigravityModelGroup, 0, len(dyn.EnforcedGroups))
	for _, rawGroup := range dyn.EnforcedGroups {
		group, err := ParseAntigravityModelGroup(rawGroup)
		if err != nil || strings.TrimSpace(rawGroup) == "" {
			return GuardConfig{}, fmt.Errorf("invalid enforced_groups value %q: must be 'gemini' or 'claude_gpt'", rawGroup)
		}
		groups = append(groups, group)
	}
	probeInterval, err := parsePositiveDuration(KeyProbeInterval, dyn.ProbeInterval)
	if err != nil {
		return GuardConfig{}, err
	}
	evidenceMaxAge, err := parsePositiveDuration(KeyEvidenceMaxAge, dyn.EvidenceMaxAge)
	if err != nil {
		return GuardConfig{}, err
	}
	window, err := parsePositiveDuration("generic_429.window", dyn.Generic429.Window)
	if err != nil {
		return GuardConfig{}, err
	}
	initialCooldown, err := parsePositiveDuration("generic_429.initial_cooldown", dyn.Generic429.InitialCooldown)
	if err != nil {
		return GuardConfig{}, err
	}
	maxCooldown, err := parsePositiveDuration("generic_429.max_cooldown", dyn.Generic429.MaxCooldown)
	if err != nil {
		return GuardConfig{}, err
	}
	halfOpenLease, err := parsePositiveDuration(KeyHalfOpenLease, dyn.HalfOpenLease)
	if err != nil {
		return GuardConfig{}, err
	}
	cfg := GuardConfig{
		Mode:                   strings.TrimSpace(strings.ToLower(dyn.Mode)),
		ManagedAuth:            strings.TrimSpace(strings.ToLower(dyn.ManagedAuth)),
		EnforcedGroups:         groups,
		RequireUniformPriority: dyn.RequireUniformPriority,
		ProbeInterval:          probeInterval,
		EvidenceMaxAge:         evidenceMaxAge,
		Generic429: Generic429Config{
			Threshold:       dyn.Generic429.Threshold,
			Window:          window,
			InitialCooldown: initialCooldown,
			MaxCooldown:     maxCooldown,
		},
		HalfOpenLease:      halfOpenLease,
		UnknownAuthPolicy:  strings.TrimSpace(strings.ToLower(dyn.UnknownAuthPolicy)),
		UnknownModelPolicy: strings.TrimSpace(strings.ToLower(dyn.UnknownModelPolicy)),
		StatePath:          strings.TrimSpace(dyn.StatePath),
	}
	if err := cfg.Validate(); err != nil {
		return GuardConfig{}, err
	}
	return cfg, nil
}

// Validate validates the complete configuration.
func (cfg Config) Validate() error {
	if cfg.AutoApply {
		return fmt.Errorf("auto_apply is no longer supported: quota guard never writes host priority or disabled state")
	}
	if err := ValidateStateCachePath(cfg.StateCachePath); err != nil {
		return err
	}
	legacyPath := path.Clean(strings.ReplaceAll(strings.TrimSpace(cfg.StateCachePath), "\\", "/"))
	guardPath := path.Clean(strings.ReplaceAll(strings.TrimSpace(cfg.Guard.StatePath), "\\", "/"))
	if legacyPath != "." && strings.EqualFold(legacyPath, guardPath) {
		return fmt.Errorf("state_cache_path and state_path must reference different files")
	}
	return cfg.Guard.Validate()
}

// ValidateStateCachePath keeps the legacy quota/evidence cache inside the same
// plugin-owned data directory as breaker state. Runtime test overrides are
// applied after startup config validation and are not part of the public YAML.
func ValidateStateCachePath(value string) error {
	if err := validatePluginDataPath(KeyStateCachePath, value); err != nil {
		return err
	}
	return nil
}

// Validate enforces all guard safety invariants.
func (cfg GuardConfig) Validate() error {
	if cfg.Mode != GuardModeObserve && cfg.Mode != GuardModeEnforce {
		return fmt.Errorf("mode must be %q or %q, got %q", GuardModeObserve, GuardModeEnforce, cfg.Mode)
	}
	if cfg.ManagedAuth != ManagedAuthAllAntigravity {
		return fmt.Errorf("managed_auth must be %q, got %q", ManagedAuthAllAntigravity, cfg.ManagedAuth)
	}
	if len(cfg.EnforcedGroups) == 0 {
		return fmt.Errorf("enforced_groups must contain at least one model group")
	}
	seen := make(map[AntigravityModelGroup]struct{}, len(cfg.EnforcedGroups))
	for _, group := range cfg.EnforcedGroups {
		parsed, err := ParseAntigravityModelGroup(string(group))
		if err != nil || string(group) == "" {
			return fmt.Errorf("invalid enforced_groups value %q: must be 'gemini' or 'claude_gpt'", group)
		}
		if _, ok := seen[parsed]; ok {
			return fmt.Errorf("enforced_groups contains duplicate %q", parsed)
		}
		seen[parsed] = struct{}{}
	}
	if cfg.Mode == GuardModeEnforce && !cfg.RequireUniformPriority {
		return fmt.Errorf("require_uniform_priority must be true in enforce mode")
	}
	if cfg.ProbeInterval < time.Minute {
		return fmt.Errorf("probe_interval must be at least 1m, got %s", cfg.ProbeInterval)
	}
	if cfg.EvidenceMaxAge < cfg.ProbeInterval {
		return fmt.Errorf("evidence_max_age must be at least probe_interval (%s), got %s", cfg.ProbeInterval, cfg.EvidenceMaxAge)
	}
	if cfg.Generic429.Threshold < MinGeneric429Threshold || cfg.Generic429.Threshold > MaxGeneric429Threshold {
		return fmt.Errorf("generic_429.threshold must be between %d and %d, got %d", MinGeneric429Threshold, MaxGeneric429Threshold, cfg.Generic429.Threshold)
	}
	if cfg.Generic429.Window <= 0 {
		return fmt.Errorf("generic_429.window must be positive, got %s", cfg.Generic429.Window)
	}
	if cfg.Generic429.InitialCooldown <= 0 {
		return fmt.Errorf("generic_429.initial_cooldown must be positive, got %s", cfg.Generic429.InitialCooldown)
	}
	if cfg.Generic429.MaxCooldown < cfg.Generic429.InitialCooldown {
		return fmt.Errorf("generic_429.max_cooldown must be at least initial_cooldown (%s), got %s", cfg.Generic429.InitialCooldown, cfg.Generic429.MaxCooldown)
	}
	if cfg.HalfOpenLease <= 0 {
		return fmt.Errorf("half_open_lease must be positive, got %s", cfg.HalfOpenLease)
	}
	if cfg.UnknownAuthPolicy != GuardPolicyFailClosed {
		return fmt.Errorf("unknown_auth_policy must be %q, got %q", GuardPolicyFailClosed, cfg.UnknownAuthPolicy)
	}
	if cfg.UnknownModelPolicy != GuardPolicyFailClosed {
		return fmt.Errorf("unknown_model_policy must be %q, got %q", GuardPolicyFailClosed, cfg.UnknownModelPolicy)
	}
	return ValidateGuardStatePath(cfg.StatePath)
}

func parsePositiveDuration(field, value string) (time.Duration, error) {
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", field, value)
	}
	return parsed, nil
}

// ValidateGuardStatePath ensures state stays in the plugin-owned data
// directory. The persistence layer must additionally reject symlink escapes.
func ValidateGuardStatePath(value string) error {
	return validatePluginDataPath(KeyStatePath, value)
}

func validatePluginDataPath(field, value string) error {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	normalized := strings.ReplaceAll(raw, "\\", "/")
	if strings.HasPrefix(normalized, "/") || (len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/') {
		return fmt.Errorf("%s must be relative, got %q", field, value)
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return fmt.Errorf("%s must not contain '..', got %q", field, value)
		}
	}
	cleaned := path.Clean(normalized)
	prefix := GuardStateDirectory + "/"
	if !strings.HasPrefix(cleaned, prefix) || cleaned == GuardStateDirectory {
		return fmt.Errorf("%s must be inside %q, got %q", field, GuardStateDirectory, value)
	}
	parts := strings.Split(cleaned, "/")
	for _, part := range parts[:len(parts)-1] {
		lower := strings.ToLower(part)
		if lower == "auth" || lower == "auths" || lower == "credential" || lower == "credentials" ||
			strings.HasPrefix(lower, "auth-") || strings.HasPrefix(lower, "auth_") ||
			strings.HasPrefix(lower, "credentials-") || strings.HasPrefix(lower, "credentials_") {
			return fmt.Errorf("%s must not target an auth or credentials directory, got %q", field, value)
		}
	}
	return nil
}

// LoadBytes parses raw YAML or JSON bytes into a validated Config. Startup
// configuration keeps legacy fields while accepting guard fields at top level.
func LoadBytes(data []byte) (Config, []string, error) {
	raw, err := decodeRaw(data)
	if err != nil {
		return Config{}, nil, fmt.Errorf("parse config: %w", err)
	}
	cfg, warnings, err := raw.applyTolerant(Default())
	if err != nil {
		return Config{}, warnings, fmt.Errorf("validate config: %w", err)
	}
	return cfg, warnings, nil
}

func decodeRaw(data []byte) (rawConfig, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return rawConfig{}, nil
	}
	var raw rawConfig
	if trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return rawConfig{}, invalid("config", err.Error(), "must match config schema")
		}
		return raw, nil
	}
	yamlMap, err := parseYAMLMap(extractPluginConfigYAML(string(trimmed)))
	if err != nil {
		return rawConfig{}, err
	}
	encoded, err := json.Marshal(yamlMap)
	if err != nil {
		return rawConfig{}, invalid("config", "yaml", "must be encodable")
	}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return rawConfig{}, invalid("config", err.Error(), "must match config schema")
	}
	return raw, nil
}

func (raw rawConfig) applyTolerant(cfg Config) (Config, []string, error) {
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.AutoApply != nil && *raw.AutoApply {
		return Config{}, nil, invalid(KeyAutoApply, "true", "is no longer supported; quota guard never mutates host auth state")
	}
	if raw.StateCachePath != nil && strings.TrimSpace(*raw.StateCachePath) != "" {
		cfg.StateCachePath = strings.TrimSpace(*raw.StateCachePath)
	}
	if len(raw.RequiredSchedulerFor) > 0 && string(raw.RequiredSchedulerFor) != "null" {
		providers, err := decodeEnforcedGroups(raw.RequiredSchedulerFor)
		if err != nil {
			return Config{}, nil, invalid("required-scheduler-for", string(raw.RequiredSchedulerFor), "must be an array or comma-separated string")
		}
		cfg.RequiredSchedulerFor = normalizeProviders(providers)
	}

	guard := cfg.Guard.Dynamic()
	if raw.Mode != nil {
		guard.Mode = *raw.Mode
	}
	if raw.ManagedAuth != nil {
		guard.ManagedAuth = *raw.ManagedAuth
	}
	if len(raw.EnforcedGroups) > 0 && string(raw.EnforcedGroups) != "null" {
		groups, err := decodeEnforcedGroups(raw.EnforcedGroups)
		if err != nil {
			return Config{}, nil, err
		}
		guard.EnforcedGroups = groups
	}
	if raw.RequireUniformPriority != nil {
		guard.RequireUniformPriority = *raw.RequireUniformPriority
	}
	if raw.ProbeInterval != nil {
		guard.ProbeInterval = *raw.ProbeInterval
	}
	if raw.EvidenceMaxAge != nil {
		guard.EvidenceMaxAge = *raw.EvidenceMaxAge
	}
	if raw.Generic429 != nil {
		if raw.Generic429.Threshold != nil {
			guard.Generic429.Threshold = *raw.Generic429.Threshold
		}
		if raw.Generic429.Window != nil {
			guard.Generic429.Window = *raw.Generic429.Window
		}
		if raw.Generic429.InitialCooldown != nil {
			guard.Generic429.InitialCooldown = *raw.Generic429.InitialCooldown
		}
		if raw.Generic429.MaxCooldown != nil {
			guard.Generic429.MaxCooldown = *raw.Generic429.MaxCooldown
		}
	}
	if raw.Generic429Threshold != nil {
		guard.Generic429.Threshold = *raw.Generic429Threshold
	}
	if raw.Generic429Window != nil {
		guard.Generic429.Window = *raw.Generic429Window
	}
	if raw.Generic429InitialCooldown != nil {
		guard.Generic429.InitialCooldown = *raw.Generic429InitialCooldown
	}
	if raw.Generic429MaxCooldown != nil {
		guard.Generic429.MaxCooldown = *raw.Generic429MaxCooldown
	}
	if raw.HalfOpenLease != nil {
		guard.HalfOpenLease = *raw.HalfOpenLease
	}
	if raw.UnknownAuthPolicy != nil {
		guard.UnknownAuthPolicy = *raw.UnknownAuthPolicy
	}
	if raw.UnknownModelPolicy != nil {
		guard.UnknownModelPolicy = *raw.UnknownModelPolicy
	}
	if raw.StatePath != nil {
		guard.StatePath = *raw.StatePath
	}
	parsedGuard, err := guard.toConfig()
	if err != nil {
		return Config{}, nil, invalid("guard", "configuration", err.Error())
	}
	cfg.Guard = parsedGuard
	if err := cfg.Validate(); err != nil {
		return Config{}, nil, invalid("config", "configuration", err.Error())
	}
	return cfg, nil, nil
}

func normalizeProviders(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func decodeEnforcedGroups(raw json.RawMessage) ([]string, error) {
	var groups []string
	if err := json.Unmarshal(raw, &groups); err == nil {
		return groups, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, invalid(KeyEnforcedGroups, string(raw), "must be an array or comma-separated string")
	}
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "[")
	text = strings.TrimSuffix(text, "]")
	if text == "" {
		return []string{}, nil
	}
	for _, item := range strings.Split(text, ",") {
		item = strings.Trim(strings.TrimSpace(item), "\"'")
		if item != "" {
			groups = append(groups, item)
		}
	}
	return groups, nil
}
