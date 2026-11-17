package imdb

import (
	"fmt"
	"net/http"
)

// Error codes shared by the HTTP API and MCP tool results.
const (
	CodeInvalidRequest = "invalid_request"
	CodeNotFound       = "not_found"
	CodeNotReady       = "not_ready"
	CodeBusy           = "busy"
)

// Error is a client-facing failure with an HTTP status.
type Error struct {
	Status    int            `json:"status"`
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Detail    map[string]any `json:"detail,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func invalid(field, format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest,
		Message: fmt.Sprintf(format, args...), Detail: map[string]any{"field": field}}
}

func notFound(msg string) *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeNotFound, Message: msg}
}

var errNotReady = &Error{Status: http.StatusServiceUnavailable, Code: CodeNotReady,
	Message: "the IMDb database is still being downloaded and imported; try again later", Retryable: true}
