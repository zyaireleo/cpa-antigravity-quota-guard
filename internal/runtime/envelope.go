package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/zyaireleo/cpa-antigravity-quota-guard/internal/config"
)

// Envelope is the JSON envelope format used by the CPA C ABI.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError represents error details inside a failure envelope.
type EnvelopeError struct {
	Code            string      `json:"code"`
	Message         string      `json:"message"`
	Retryable       bool        `json:"retryable"`
	HTTPStatus      int         `json:"http_status,omitempty"`
	ResponseHeaders http.Header `json:"response_headers,omitempty"`
	ResponseBody    []byte      `json:"response_body,omitempty"`
}

func decodeRegisterRequest(raw []byte) (RegisterRequest, error) {
	configYAML, schemaVersion, hostFeatures, err := decodeLifecycleRequest(raw)
	if err != nil {
		return RegisterRequest{}, err
	}
	return RegisterRequest{ConfigYAML: configYAML, SchemaVersion: schemaVersion, HostFeatures: hostFeatures}, nil
}

func decodeReconfigureRequest(raw []byte) (ReconfigureRequest, error) {
	configYAML, schemaVersion, hostFeatures, err := decodeLifecycleRequest(raw)
	if err != nil {
		return ReconfigureRequest{}, err
	}
	return ReconfigureRequest{ConfigYAML: configYAML, SchemaVersion: schemaVersion, HostFeatures: hostFeatures}, nil
}

func decodeLifecycleRequest(raw []byte) (string, uint32, []string, error) {
	if len(raw) == 0 {
		return "", 0, nil, nil
	}
	var request struct {
		ConfigYAML    json.RawMessage `json:"config_yaml"`
		SchemaVersion uint32          `json:"schema_version"`
		HostFeatures  []string        `json:"host_features,omitempty"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return "", 0, nil, fmt.Errorf("%w: decode json: %v", ErrInvalidRequest, err)
	}
	if len(request.ConfigYAML) == 0 || string(request.ConfigYAML) == "null" {
		return "", request.SchemaVersion, append([]string(nil), request.HostFeatures...), nil
	}
	var configBytes []byte
	if err := json.Unmarshal(request.ConfigYAML, &configBytes); err == nil {
		return string(configBytes), request.SchemaVersion, append([]string(nil), request.HostFeatures...), nil
	}
	var configString string
	if err := json.Unmarshal(request.ConfigYAML, &configString); err != nil {
		return "", 0, nil, fmt.Errorf("%w: decode config_yaml: %v", ErrInvalidRequest, err)
	}
	return configString, request.SchemaVersion, append([]string(nil), request.HostFeatures...), nil
}

func failureStatus(code, message string, status int, retryable bool) []byte {
	return failureDirectResponse(code, message, status, retryable, nil, nil)
}

func failureDirectResponse(code, message string, status int, retryable bool, headers http.Header, body []byte) []byte {
	return mustMarshal(Envelope{OK: false, Error: &EnvelopeError{
		Code:            code,
		Message:         message,
		Retryable:       retryable,
		HTTPStatus:      status,
		ResponseHeaders: headers,
		ResponseBody:    body,
	}})
}

func envelopeRegister(result RegisterResult, err error) []byte {
	if err != nil {
		return failure(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return failure(fmt.Errorf("encode result: %w", err))
	}
	return mustMarshal(Envelope{OK: true, Result: encoded})
}

func envelopeStatus(err error) []byte {
	if err != nil {
		return failure(err)
	}
	return mustMarshal(Envelope{OK: true, Result: json.RawMessage(`{"status":"ok"}`)})
}

func failure(err error) []byte {
	return mustMarshal(Envelope{OK: false, Error: envelopeError(err)})
}

func envelopeError(err error) *EnvelopeError {
	code, retryable := "internal_error", false
	switch {
	case errors.Is(err, ErrInvalidRequest):
		code = "invalid_request"
	case errors.Is(err, config.ErrInvalidConfig):
		code = "invalid_config"
	case errors.Is(err, ErrRunInProgress):
		code, retryable = "run_in_progress", true
	case errors.Is(err, ErrShutdown):
		code = "shutdown"
	}
	return &EnvelopeError{Code: code, Message: err.Error(), Retryable: retryable}
}

func mustMarshal(envelope Envelope) []byte {
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"internal_error","message":"encode envelope failed","retryable":false}}`)
	}
	return encoded
}
