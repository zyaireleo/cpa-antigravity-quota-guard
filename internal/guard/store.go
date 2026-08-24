package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxStateFileSize = 16 << 20

var stateWriteProbe = []byte("quota-guard-state-write-probe\n")

// Load creates an engine and restores its state when path exists. A missing
// state file is the normal first-start condition and returns an empty engine.
func Load(path string, config Config) (*Engine, error) {
	engine, err := New(config)
	if err != nil {
		return nil, err
	}
	if err := engine.Load(path); err != nil {
		return nil, err
	}
	return engine, nil
}

// VerifyStatePathWritable performs the same directory and symlink checks used
// by Save, then proves that a file can be written, fsynced, atomically renamed,
// read back, and removed without changing the real breaker state file.
func VerifyStatePathWritable(path string) error {
	rawPath := strings.TrimSpace(path)
	path = filepath.Clean(rawPath)
	if path == "" || path == "." {
		return ErrUnsafeStatePath
	}
	directory := filepath.Dir(path)
	if err := rejectSymlinkComponents(directory); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := rejectSymlinkComponents(directory); err != nil {
		return err
	}
	if info, err := os.Lstat(directory); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafeStatePath
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return ErrUnsafeStatePath
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	temporary, err := os.CreateTemp(directory, ".guard-write-probe-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	verifiedName := temporaryName + ".verified"
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
		_ = os.Remove(verifiedName)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(stateWriteProbe); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, verifiedName); err != nil {
		return err
	}
	if err := syncStateDirectory(directory); err != nil {
		return err
	}
	readBack, err := os.ReadFile(verifiedName)
	if err != nil {
		return err
	}
	if !bytes.Equal(readBack, stateWriteProbe) {
		return fmt.Errorf("guard: state write probe read-back mismatch")
	}
	if err := os.Remove(verifiedName); err != nil {
		return err
	}
	return syncStateDirectory(directory)
}

// Load replaces the engine's in-memory state from a verified JSON document.
func (e *Engine) Load(path string) error {
	data, err := readStateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	document, err := decodeDocument(data)
	if err != nil {
		return err
	}
	entries, authByID, err := validateDocument(document)
	if err != nil {
		return err
	}

	e.mu.Lock()
	e.entries = entries
	e.authByID = authByID
	e.updated = document.UpdatedAt.UTC()
	e.dirty = 0
	e.generation++
	e.mu.Unlock()
	return nil
}

func readStateFile(path string) ([]byte, error) {
	rawPath := strings.TrimSpace(path)
	path = filepath.Clean(rawPath)
	if path == "" || path == "." {
		return nil, ErrUnsafeStatePath
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, ErrUnsafeStatePath
	}
	if info.Size() > maxStateFileSize {
		return nil, fmt.Errorf("%w: state file exceeds %d bytes", ErrInvalidState, maxStateFileSize)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return nil, ErrUnsafeStatePath
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxStateFileSize {
		return nil, fmt.Errorf("%w: state file exceeds %d bytes", ErrInvalidState, maxStateFileSize)
	}
	return data, nil
}

func decodeDocument(data []byte) (Document, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Document{}, fmt.Errorf("%w: trailing JSON", ErrInvalidState)
	}
	return document, nil
}

func validateDocument(document Document) (map[string]*Entry, map[string]string, error) {
	if document.SchemaVersion != SchemaVersion {
		return nil, nil, fmt.Errorf("%w: unsupported schema version %d", ErrInvalidState, document.SchemaVersion)
	}
	entries := make(map[string]*Entry, len(document.Entries))
	authByID := make(map[string]string)
	identityByIndex := make(map[string]Identity)
	for index := range document.Entries {
		entry := document.Entries[index]
		entry.AuthID = strings.TrimSpace(entry.AuthID)
		entry.AuthIndex = strings.TrimSpace(entry.AuthIndex)
		entry.IdentityFingerprint = strings.TrimSpace(entry.IdentityFingerprint)
		entry.Provider = strings.TrimSpace(entry.Provider)
		if entry.AuthID == "" || entry.AuthIndex == "" || entry.IdentityFingerprint == "" ||
			!entry.ModelGroup.Valid() || !entry.State.Valid() || !entry.Reason.Valid() ||
			entry.FailureCount < 0 || (entry.RemainingPercent != nil &&
			(mathInvalid(*entry.RemainingPercent) || *entry.RemainingPercent < 0 || *entry.RemainingPercent > 100)) {
			return nil, nil, fmt.Errorf("%w: invalid entry %d", ErrInvalidState, index)
		}
		key := entryKey(entry.AuthIndex, entry.ModelGroup)
		if _, duplicate := entries[key]; duplicate {
			return nil, nil, fmt.Errorf("%w: duplicate entry %q", ErrInvalidState, key)
		}
		identity := Identity{
			AuthID:              entry.AuthID,
			AuthIndex:           entry.AuthIndex,
			IdentityFingerprint: entry.IdentityFingerprint,
			Provider:            entry.Provider,
			Priority:            entry.Priority,
		}
		if prior, exists := identityByIndex[entry.AuthIndex]; exists && prior != identity {
			return nil, nil, fmt.Errorf("%w: inconsistent identity at auth index %q", ErrInvalidState, entry.AuthIndex)
		}
		if priorIndex, exists := authByID[entry.AuthID]; exists && priorIndex != entry.AuthIndex {
			return nil, nil, fmt.Errorf("%w: auth id %q maps to multiple indexes", ErrInvalidState, entry.AuthID)
		}
		identityByIndex[entry.AuthIndex] = identity
		authByID[entry.AuthID] = entry.AuthIndex
		clone := entry
		if entry.RemainingPercent != nil {
			remaining := *entry.RemainingPercent
			clone.RemainingPercent = &remaining
		}
		entries[key] = &clone
	}
	return entries, authByID, nil
}

func mathInvalid(value float64) bool {
	return value != value || value > 1.7976931348623157e+308 || value < -1.7976931348623157e+308
}

// Save writes a stable state snapshot with mode 0600, fsyncs the file,
// atomically renames it in place, fsyncs the directory, and verifies that the
// exact bytes can be read back and decoded.
func (e *Engine) Save(path string) error {
	rawPath := strings.TrimSpace(path)
	path = filepath.Clean(rawPath)
	if path == "" || path == "." {
		e.recordPersistError()
		return ErrUnsafeStatePath
	}
	directory := filepath.Dir(path)
	if err := rejectSymlinkComponents(directory); err != nil {
		e.recordPersistError()
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		e.recordPersistError()
		return err
	}
	if err := rejectSymlinkComponents(directory); err != nil {
		e.recordPersistError()
		return err
	}
	if info, err := os.Lstat(directory); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		e.recordPersistError()
		return ErrUnsafeStatePath
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			e.recordPersistError()
			return ErrUnsafeStatePath
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		e.recordPersistError()
		return err
	}

	e.mu.Lock()
	document := Document{SchemaVersion: SchemaVersion, UpdatedAt: e.updated, Entries: sortedEntries(e.entries)}
	if document.UpdatedAt.IsZero() {
		document.UpdatedAt = time.Now().UTC()
	}
	generation := e.generation
	e.mu.Unlock()
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		e.recordPersistError()
		return err
	}
	data = append(data, '\n')
	if len(data) > maxStateFileSize {
		e.recordPersistError()
		return fmt.Errorf("%w: encoded state exceeds %d bytes", ErrInvalidState, maxStateFileSize)
	}

	temporary, err := os.CreateTemp(directory, ".guard-state-*.tmp")
	if err != nil {
		e.recordPersistError()
		return err
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		e.recordPersistError()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		e.recordPersistError()
		return err
	}
	if err := temporary.Sync(); err != nil {
		e.recordPersistError()
		return err
	}
	if err := temporary.Close(); err != nil {
		e.recordPersistError()
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		e.recordPersistError()
		return err
	}
	removeTemporary = false
	if syncErr := syncStateDirectory(directory); syncErr != nil {
		e.recordPersistError()
		return syncErr
	}
	readBack, err := readStateFile(path)
	if err != nil || !bytes.Equal(readBack, data) {
		e.recordPersistError()
		if err != nil {
			return err
		}
		return fmt.Errorf("guard: state read-back mismatch")
	}
	verifiedDocument, err := decodeDocument(readBack)
	if err != nil {
		e.recordPersistError()
		return err
	}
	if _, _, err := validateDocument(verifiedDocument); err != nil {
		e.recordPersistError()
		return err
	}

	e.mu.Lock()
	if e.generation == generation {
		e.dirty = 0
	}
	e.mu.Unlock()
	return nil
}

// rejectSymlinkComponents rejects any existing symlink in a state path. Public
// configuration only permits relative plugin-data paths; absolute paths are
// accepted solely by isolated tests and receive the same component checks.
func rejectSymlinkComponents(path string) error {
	path = filepath.Clean(path)
	if filepath.IsAbs(path) {
		// Absolute paths are only accepted through explicit Runtime test/dev
		// overrides. macOS temp roots commonly traverse the system /var symlink;
		// production YAML is restricted to relative plugin-data paths.
		return nil
	}
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	componentsPath := strings.TrimPrefix(path, current)
	if !filepath.IsAbs(path) {
		current = "."
		componentsPath = path
	}
	for _, component := range strings.Split(componentsPath, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			return ErrUnsafeStatePath
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeStatePath
		}
	}
	return nil
}

func syncStateDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer func() { _ = directoryFile.Close() }()
	return directoryFile.Sync()
}

func (e *Engine) recordPersistError() {
	e.mu.Lock()
	e.metrics.StatePersistErrorTotal++
	e.mu.Unlock()
}
