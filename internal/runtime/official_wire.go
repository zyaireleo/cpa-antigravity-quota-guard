package runtime

import (
	"encoding/json"
	"net/http"
	"time"
)

// The wire types in this file intentionally mirror CLIProxyAPI v7.2.141's
// sdk/pluginapi JSON contract without importing the host implementation into
// the plugin domain.

type schedulerPickRequest struct {
	RequestID  string                   `json:"RequestID"`
	Provider   string                   `json:"Provider"`
	Providers  []string                 `json:"Providers"`
	Model      string                   `json:"Model"`
	Stream     bool                     `json:"Stream"`
	Candidates []schedulerAuthCandidate `json:"Candidates"`
}

type schedulerAuthCandidate struct {
	ID         string            `json:"ID"`
	Provider   string            `json:"Provider"`
	Priority   int               `json:"Priority"`
	Status     string            `json:"Status"`
	Attributes map[string]string `json:"Attributes"`
	Metadata   map[string]any    `json:"Metadata"`
}

type schedulerPickResponse struct {
	AuthID          string `json:"AuthID,omitempty"`
	DelegateBuiltin string `json:"DelegateBuiltin,omitempty"`
	Handled         bool   `json:"Handled"`
}

const schedulerDelegateConfigured = "configured"

type usageRecord struct {
	RequestID       string        `json:"RequestID"`
	Provider        string        `json:"Provider"`
	ExecutorType    string        `json:"ExecutorType"`
	Model           string        `json:"Model"`
	Alias           string        `json:"Alias"`
	AuthID          string        `json:"AuthID"`
	AuthIndex       string        `json:"AuthIndex"`
	AuthType        string        `json:"AuthType"`
	Source          string        `json:"Source"`
	ReasoningEffort string        `json:"ReasoningEffort"`
	ServiceTier     string        `json:"ServiceTier"`
	Generate        bool          `json:"Generate"`
	RequestedAt     time.Time     `json:"RequestedAt"`
	Latency         time.Duration `json:"Latency"`
	TTFT            time.Duration `json:"TTFT"`
	Failed          bool          `json:"Failed"`
	Failure         usageFailure  `json:"Failure"`
	ResponseHeaders http.Header   `json:"ResponseHeaders"`
}

type usageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

type requestInterceptRequest struct {
	RequestID      string `json:"RequestID"`
	TraceID        string `json:"TraceID"`
	SourceFormat   string `json:"SourceFormat"`
	ToFormat       string `json:"ToFormat"`
	Model          string `json:"Model"`
	RequestedModel string `json:"RequestedModel"`
	Stream         bool   `json:"Stream"`
}

type requestInterceptResponse struct {
	Terminate       bool        `json:"Terminate"`
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
}

type managementRegistrationRequest struct {
	BasePath         string `json:"BasePath"`
	ResourceBasePath string `json:"ResourceBasePath"`
}

func successResult(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return failure(err)
	}
	return mustMarshal(Envelope{OK: true, Result: encoded})
}
