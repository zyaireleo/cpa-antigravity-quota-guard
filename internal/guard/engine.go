package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	evidenceSourceQuotaProbe = "quota_probe"
	evidenceSourceUsage429   = "usage_429"
	evidenceSourceUsageOK    = "usage_success"
)

type Engine struct {
	mu         sync.Mutex
	config     Config
	entries    map[string]*Entry
	authByID   map[string]string
	cursors    map[ModelGroup]uint64
	metrics    Metrics
	lastPick   *SelectionEvent
	updated    time.Time
	dirty      uint64
	generation uint64
}

func New(config Config) (*Engine, error) {
	config = withConfigDefaults(config)
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Engine{
		config:   config,
		entries:  make(map[string]*Entry),
		authByID: make(map[string]string),
		cursors:  make(map[ModelGroup]uint64),
	}, nil
}

func withConfigDefaults(config Config) Config {
	defaults := DefaultConfig()
	if config.Generic429Threshold == 0 {
		config.Generic429Threshold = defaults.Generic429Threshold
	}
	if config.Generic429Window == 0 {
		config.Generic429Window = defaults.Generic429Window
	}
	if config.Generic429InitialCooldown == 0 {
		config.Generic429InitialCooldown = defaults.Generic429InitialCooldown
	}
	if config.Generic429MaxCooldown == 0 {
		config.Generic429MaxCooldown = defaults.Generic429MaxCooldown
	}
	if config.HalfOpenLease == 0 {
		config.HalfOpenLease = defaults.HalfOpenLease
	}
	if config.EvidenceMaxAge == 0 {
		config.EvidenceMaxAge = defaults.EvidenceMaxAge
	}
	if config.UnknownAuthPolicy == "" {
		config.UnknownAuthPolicy = defaults.UnknownAuthPolicy
	}
	if config.UnknownModelPolicy == "" {
		config.UnknownModelPolicy = defaults.UnknownModelPolicy
	}
	return config
}

func validateConfig(config Config) error {
	if config.Generic429Threshold < 1 || config.Generic429Window <= 0 ||
		config.Generic429InitialCooldown <= 0 || config.Generic429MaxCooldown <= 0 ||
		config.Generic429MaxCooldown < config.Generic429InitialCooldown ||
		config.HalfOpenLease <= 0 || config.EvidenceMaxAge <= 0 {
		return ErrInvalidConfig
	}
	if !validPolicy(config.UnknownAuthPolicy) || !validPolicy(config.UnknownModelPolicy) {
		return ErrInvalidConfig
	}
	return nil
}

func validPolicy(policy Policy) bool {
	return policy == PolicyFailClosed || policy == PolicyFailOpen
}

func entryKey(authIndex string, group ModelGroup) string {
	return authIndex + "\x00" + string(group)
}

// FingerprintIdentity returns a stable non-secret fencing value when the host
// does not already expose one. Callers should include immutable account data in
// IdentityFingerprint when it is available.
func FingerprintIdentity(identity Identity) string {
	value := strings.Join([]string{
		strings.TrimSpace(identity.AuthIndex),
		strings.TrimSpace(identity.AuthID),
		strings.ToLower(strings.TrimSpace(identity.Provider)),
	}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func normalizeIdentity(identity Identity) (Identity, error) {
	identity.AuthID = strings.TrimSpace(identity.AuthID)
	identity.AuthIndex = strings.TrimSpace(identity.AuthIndex)
	identity.Provider = strings.TrimSpace(identity.Provider)
	identity.IdentityFingerprint = strings.TrimSpace(identity.IdentityFingerprint)
	if identity.AuthIndex == "" || identity.AuthID == "" {
		return Identity{}, ErrInvalidIdentity
	}
	if identity.IdentityFingerprint == "" {
		identity.IdentityFingerprint = FingerprintIdentity(identity)
	}
	return identity, nil
}

// ReplaceRoster atomically reconciles the current non-secret auth roster.
// Missing identities are removed. A fingerprint or AuthID change resets both
// model groups to uninitialized instead of inheriting the prior credential's
// breaker state.
func (e *Engine) ReplaceRoster(identities []Identity) error {
	normalized := make([]Identity, 0, len(identities))
	seenIndexes := make(map[string]struct{}, len(identities))
	seenIDs := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		item, err := normalizeIdentity(identity)
		if err != nil {
			return err
		}
		if _, duplicate := seenIndexes[item.AuthIndex]; duplicate {
			return fmt.Errorf("%w: duplicate auth index %q", ErrInvalidIdentity, item.AuthIndex)
		}
		if _, duplicate := seenIDs[item.AuthID]; duplicate {
			return fmt.Errorf("%w: duplicate auth id %q", ErrInvalidIdentity, item.AuthID)
		}
		seenIndexes[item.AuthIndex] = struct{}{}
		seenIDs[item.AuthID] = struct{}{}
		normalized = append(normalized, item)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	next := make(map[string]*Entry, len(normalized)*2)
	nextAuthByID := make(map[string]string, len(normalized))
	for _, identity := range normalized {
		nextAuthByID[identity.AuthID] = identity.AuthIndex
		for _, group := range []ModelGroup{ModelGroupGemini, ModelGroupClaudeGPT} {
			key := entryKey(identity.AuthIndex, group)
			current := e.entries[key]
			if current != nil && current.AuthID == identity.AuthID &&
				current.IdentityFingerprint == identity.IdentityFingerprint {
				clone := *current
				clone.Provider = identity.Provider
				clone.Priority = identity.Priority
				next[key] = &clone
				continue
			}
			next[key] = newEntry(identity, group)
		}
	}

	if entriesEqual(e.entries, next) {
		e.authByID = nextAuthByID
		return nil
	}
	e.entries = next
	e.authByID = nextAuthByID
	e.markDirtyLocked(time.Now().UTC())
	return nil
}

// RegisterIdentity adds or replaces one identity without removing the rest of
// the roster.
func (e *Engine) RegisterIdentity(identity Identity) error {
	normalized, err := normalizeIdentity(identity)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if existingIndex, exists := e.authByID[normalized.AuthID]; exists && existingIndex != normalized.AuthIndex {
		return fmt.Errorf("%w: auth id %q already belongs to index %q", ErrInvalidIdentity, normalized.AuthID, existingIndex)
	}

	changed := false
	for _, group := range []ModelGroup{ModelGroupGemini, ModelGroupClaudeGPT} {
		key := entryKey(normalized.AuthIndex, group)
		current := e.entries[key]
		if current == nil || current.AuthID != normalized.AuthID ||
			current.IdentityFingerprint != normalized.IdentityFingerprint {
			e.entries[key] = newEntry(normalized, group)
			changed = true
			continue
		}
		if current.Provider != normalized.Provider || current.Priority != normalized.Priority {
			current.Provider = normalized.Provider
			current.Priority = normalized.Priority
			changed = true
		}
	}
	for id, index := range e.authByID {
		if index == normalized.AuthIndex && id != normalized.AuthID {
			delete(e.authByID, id)
		}
	}
	e.authByID[normalized.AuthID] = normalized.AuthIndex
	if changed {
		e.markDirtyLocked(time.Now().UTC())
	}
	return nil
}

func newEntry(identity Identity, group ModelGroup) *Entry {
	return &Entry{
		AuthID:              identity.AuthID,
		AuthIndex:           identity.AuthIndex,
		IdentityFingerprint: identity.IdentityFingerprint,
		Provider:            identity.Provider,
		Priority:            identity.Priority,
		ModelGroup:          group,
		State:               StateUninitialized,
	}
}

func entriesEqual(left, right map[string]*Entry) bool {
	if len(left) != len(right) {
		return false
	}
	for key, a := range left {
		b := right[key]
		if b == nil || !entryEqual(*a, *b) {
			return false
		}
	}
	return true
}

func entryEqual(a, b Entry) bool {
	if a.AuthID != b.AuthID || a.AuthIndex != b.AuthIndex ||
		a.IdentityFingerprint != b.IdentityFingerprint || a.Provider != b.Provider ||
		a.Priority != b.Priority || a.ModelGroup != b.ModelGroup || a.State != b.State ||
		a.Reason != b.Reason || !a.OpenedAt.Equal(b.OpenedAt) ||
		!a.RecoverAt.Equal(b.RecoverAt) || !a.ResetAt.Equal(b.ResetAt) ||
		a.FailureCount != b.FailureCount || !a.LastFailureAt.Equal(b.LastFailureAt) ||
		!a.HalfOpenStartedAt.Equal(b.HalfOpenStartedAt) ||
		!a.HalfOpenLeaseUntil.Equal(b.HalfOpenLeaseUntil) ||
		a.LastEvidenceSource != b.LastEvidenceSource || !a.LastEvidenceAt.Equal(b.LastEvidenceAt) {
		return false
	}
	if a.halfOpenRequestID != b.halfOpenRequestID {
		return false
	}
	if !a.LastQuotaEvidenceAt.Equal(b.LastQuotaEvidenceAt) {
		return false
	}
	if a.RemainingPercent == nil || b.RemainingPercent == nil {
		return a.RemainingPercent == nil && b.RemainingPercent == nil
	}
	return *a.RemainingPercent == *b.RemainingPercent
}

func (e *Engine) markDirtyLocked(at time.Time) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	e.updated = at.UTC()
	e.dirty++
	e.generation++
}

func sortedEntries(entries map[string]*Entry) []Entry {
	items := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		clone := *entry
		if entry.RemainingPercent != nil {
			value := *entry.RemainingPercent
			clone.RemainingPercent = &value
		}
		items = append(items, clone)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].AuthIndex != items[j].AuthIndex {
			return items[i].AuthIndex < items[j].AuthIndex
		}
		return items[i].ModelGroup < items[j].ModelGroup
	})
	return items
}
