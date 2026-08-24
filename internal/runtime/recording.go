package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/apply"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/host"
	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/state"
)

// projectRun is the one Runtime path that projects a run into the in-memory
// bounded history and state cache. Host truth is already present in result;
// cache persistence health is recorded independently in result.Record.
func (r *Runtime) projectRun(ctx context.Context, store *state.Store, result apply.Result, audit string, entry RunHistoryEntry) (apply.Result, error) {
	return r.persistRun(ctx, store, result, audit, &entry)
}

func (r *Runtime) persistRun(ctx context.Context, store *state.Store, result apply.Result, audit string, entry *RunHistoryEntry) (apply.Result, error) {
	if store == nil {
		result.Record = apply.RecordResult{Status: apply.RecordFailed, Error: "state store is required"}
		if entry != nil {
			r.snapshotRunEntry(result, audit, *entry)
		} else {
			r.snapshotLatestResult(result, audit)
		}
		return result, fmt.Errorf("project execution result: state store is required")
	}

	result.Record = apply.RecordResult{Status: apply.RecordPersisted}
	if entry != nil {
		entry.Record = result.Record
		entry.Attempted = result.Attempted
		entry.Succeeded = result.Succeeded
		entry.Failed = result.Failed
		entry.Skipped = result.Skipped
		entry.NoChange = result.NoChange
		entry.Conflicts = result.Conflicts
		entry.Uncertain = result.Uncertain
		r.snapshotRunEntry(result, audit, *entry)
	} else {
		r.snapshotLatestResult(result, audit)
	}

	resultJSON, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		result.Record = apply.RecordResult{Status: apply.RecordFailed, Error: host.RedactBytes([]byte(marshalErr.Error()))}
		if entry != nil {
			entry.Record = result.Record
			r.snapshotRunEntry(result, audit, *entry)
		} else {
			r.snapshotLatestResult(result, audit)
		}
		return result, fmt.Errorf("encode execution result: %w", marshalErr)
	}
	historyJSON, marshalErr := json.Marshal(r.currentRunHistory())
	if marshalErr != nil {
		result.Record = apply.RecordResult{Status: apply.RecordFailed, Error: host.RedactBytes([]byte(marshalErr.Error()))}
		if entry != nil {
			entry.Record = result.Record
			r.snapshotRunEntry(result, audit, *entry)
		} else {
			r.snapshotLatestResult(result, audit)
		}
		return result, fmt.Errorf("encode execution history: %w", marshalErr)
	}
	store.SetRuntimeSnapshot(audit, resultJSON, historyJSON)
	if err := store.SaveAtomic(ctx); err != nil {
		result.Record = apply.RecordResult{Status: apply.RecordFailed, Error: host.RedactBytes([]byte(err.Error()))}
		if entry != nil {
			entry.Record = result.Record
			r.snapshotRunEntry(result, audit, *entry)
		} else {
			r.snapshotLatestResult(result, audit)
		}
		return result, fmt.Errorf("persist execution result: %w", err)
	}
	return result, nil
}
