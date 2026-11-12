package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"antigravity-priority/internal/runtime"
	"antigravity-priority/internal/state"
)

func TestRuntime_NeutralMetadataDoesNotSuppressAuthenticationFailure(t *testing.T) {
	statuses := []struct {
		code int
		kind state.FailureKind
	}{
		{http.StatusUnauthorized, state.FailureUnauthorized},
		{http.StatusForbidden, state.FailureForbidden},
	}
	classes := []string{
		"upstream_429_ambiguous",
		"upstream_request_rejected",
		"upstream_attempt_budget_exhausted",
		"upstream_rate_limited",
	}
	for _, status := range statuses {
		for _, class := range classes {
			t.Run(fmt.Sprintf("%d/%s", status.code, class), func(t *testing.T) {
				ctx := context.Background()
				now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
				r, _, mock, cachePath, paths := newHealthRuntime(t, now, "idx-a")
				t.Cleanup(func() { _ = r.Shutdown(ctx) })

				payload := neutralConflictUsagePayload(t, "gemini-3.8-flash-high", class, status.code)
				mustOK(t, r.Handle(ctx, runtime.MethodUsageHandle, payload))
				store, err := state.Load(ctx, cachePath)
				if err != nil {
					t.Fatal(err)
				}
				entry, exists := store.GetCooldowns()["idx-a"]
				if !exists || entry.FailureKind != status.kind || entry.ProbeEnabled {
					t.Fatalf("authentication protection lost to neutral metadata: %#v", store.GetCooldowns())
				}
				if got := readPriority(t, paths["idx-a"]); got != -1 {
					t.Fatalf("authentication failure priority = %d, want -1", got)
				}
				mock.mu.Lock()
				probeCalls := len(mock.httpCalls)
				mock.mu.Unlock()
				if probeCalls != 0 {
					t.Fatalf("authentication failure triggered %d probes", probeCalls)
				}
				history, err := r.GetRequestHistory(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(history) != 1 || !history[0].Failed || history[0].StatusCode != status.code {
					t.Fatalf("raw authentication outcome was not retained: %#v", history)
				}
			})
		}
	}
}

func TestRuntime_NeutralGeminiModelNormalizationMatchesHost(t *testing.T) {
	models := []string{
		"models/gemini-3.8-flash-high",
		"  gemini-3.8-flash-high  ",
		" Models/GEMINI-3.8-flash-high ",
	}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
			r, _, mock, cachePath, paths := newHealthRuntime(t, now, "idx-a")
			t.Cleanup(func() { _ = r.Shutdown(ctx) })
			for range 3 {
				payload := neutralConflictUsagePayload(t, model, "upstream_429_ambiguous", http.StatusTooManyRequests)
				mustOK(t, r.Handle(ctx, runtime.MethodUsageHandle, payload))
			}
			store, err := state.Load(ctx, cachePath)
			if err != nil {
				t.Fatal(err)
			}
			if cooldowns := store.GetCooldowns(); len(cooldowns) != 0 {
				t.Fatalf("host-compatible neutral model created cooldown: %#v", cooldowns)
			}
			if got := readPriority(t, paths["idx-a"]); got != 100 {
				t.Fatalf("neutral model changed priority to %d, want 100", got)
			}
			mock.mu.Lock()
			probeCalls := len(mock.httpCalls)
			mock.mu.Unlock()
			if probeCalls != 0 {
				t.Fatalf("neutral model triggered %d probes", probeCalls)
			}
			history, err := r.GetRequestHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 3 {
				t.Fatalf("history length = %d, want 3", len(history))
			}
			for _, entry := range history {
				if entry.Model != model || !entry.Failed || entry.StatusCode != http.StatusTooManyRequests ||
					entry.FailureClass != "upstream_429_ambiguous" || entry.HealthEffect != "neutral" ||
					entry.GatewayTraceID != "trace-normalization" || entry.RequestID != "request-normalization" ||
					entry.AttemptsUsed != 2 || !entry.RetryExhausted {
					t.Fatalf("raw neutral history metadata changed: %#v", entry)
				}
			}
		})
	}
}

func neutralConflictUsagePayload(t *testing.T, model, class string, status int) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"Provider":       "antigravity",
		"Model":          model,
		"AuthIndex":      "idx-a",
		"Failed":         true,
		"Failure":        map[string]any{"StatusCode": status},
		"FailureClass":   class,
		"HealthEffect":   "neutral",
		"GatewayTraceID": "trace-normalization",
		"RequestID":      "request-normalization",
		"AttemptsUsed":   2,
		"RetryExhausted": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
