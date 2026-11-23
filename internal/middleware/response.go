// Package middleware implements cross-cutting HTTP concerns: request IDs,
// panic recovery, logging, security headers, CORS, body limits and error envelopes.
package middleware

import (
	"context"
	"encoding/json"
	"net/http"
)

// ErrorCode is a stable machine-readable API error code.
type ErrorCode string

const (
	CodeValidationFailed     ErrorCode = "validation_failed"
	CodeUnauthenticated      ErrorCode = "unauthenticated"
	CodeForbidden            ErrorCode = "forbidden"
	CodeNotFound             ErrorCode = "not_found"
	CodeConflict             ErrorCode = "conflict"
	CodePayloadTooLarge      ErrorCode = "payload_too_large"
	CodeUnsupportedMediaType ErrorCode = "unsupported_media_type"
	CodeRateLimited          ErrorCode = "rate_limited"
	CodeStorageUnavailable   ErrorCode = "storage_unavailable"
	CodeInternalError        ErrorCode = "internal_error"
	CodeBadRequest           ErrorCode = "bad_request"
	CodeMethodNotAllowed     ErrorCode = "method_not_allowed"
	CodeRequestTimeout       ErrorCode = "request_timeout"
)

// APIError matches the transport-neutral frontend error envelope.
type APIError struct {
	Code      ErrorCode           `json:"code"`
	Message   string              `json:"message"`
	RequestID string              `json:"requestId"`
	Fields    map[string][]string `json:"fields,omitempty"`
}

type errorEnvelope struct {
	Error APIError `json:"error"`
}

// WriteJSON writes a successful (bare object/array) JSON response.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if payload != nil {
		_ = json.NewEncoder(w).Encode(payload)
	}
}

// WriteError writes the centralized structured error envelope. Messages must be
// safe for clients and must never contain credentials, DSNs, or raw SQL errors.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code ErrorCode, message string, fields map[string][]string) {
	requestID := ""
	if r != nil {
		requestID = RequestIDFromContext(r.Context())
	}
	WriteJSON(w, status, errorEnvelope{Error: APIError{
		Code:      code,
		Message:   message,
		RequestID: requestID,
		Fields:    fields,
	}})
}

type requestIDContextKey struct{}

// WithRequestID attaches the request ID to a context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, id)
}

// RequestIDFromContext returns the request ID, or an empty string if absent.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}
