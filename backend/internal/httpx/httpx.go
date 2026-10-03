package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// ErrorBody is the uniform error envelope returned by every endpoint.
type ErrorBody struct {
	Error   string            `json:"error"`
	Code    string            `json:"code,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

// ListBody is the uniform envelope for paginated collections.
type ListBody struct {
	Items any            `json:"items"`
	Total int            `json:"total"`
	Page  int            `json:"page"`
	Limit int            `json:"limit"`
	Pages int            `json:"pages"`
	Extra map[string]any `json:"extra,omitempty"`
}

// JSON writes v as a JSON response with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json response", "error", err)
	}
}

// OK writes a 200 response.
func OK(w http.ResponseWriter, v any) { JSON(w, http.StatusOK, v) }

// Created writes a 201 response.
func Created(w http.ResponseWriter, v any) { JSON(w, http.StatusCreated, v) }

// NoContent writes a 204 response.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Error writes a structured error response.
func Error(w http.ResponseWriter, status int, msg, code string) {
	JSON(w, status, ErrorBody{Error: msg, Code: code})
}

// Fail maps a domain error to the appropriate HTTP status. Unrecognised errors
// are logged and reported as 500 without leaking internals to the client.
func Fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, models.ErrNotFound):
		Error(w, http.StatusNotFound, "resource not found", "not_found")
	case errors.Is(err, models.ErrForbidden):
		Error(w, http.StatusForbidden, "you do not have access to this resource", "forbidden")
	case errors.Is(err, models.ErrUnauthorized):
		Error(w, http.StatusUnauthorized, "authentication required", "unauthorized")
	case errors.Is(err, models.ErrConflict):
		Error(w, http.StatusConflict, "resource conflict", "conflict")
	case errors.Is(err, models.ErrInvalidStatus):
		Error(w, http.StatusUnprocessableEntity, err.Error(), "invalid_status_transition")
	case errors.Is(err, models.ErrInvalidInput):
		Error(w, http.StatusBadRequest, err.Error(), "invalid_input")
	default:
		slog.Error("unhandled request error", "error", err)
		Error(w, http.StatusInternalServerError, "internal server error", "internal")
	}
}

// auth_errors was a placeholder for classifying auth-package errors; the auth
// package wraps models.ErrUnauthorized instead, so no special case is needed.

// DecodeJSON reads a JSON request body with a size limit and strict field
// checking so typos in client payloads surface as 400s.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	const maxBody = 1 << 20 // 1 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		switch {
		case errors.Is(err, io.EOF):
			return fmt.Errorf("%w: request body is empty", models.ErrInvalidInput)
		case strings.Contains(err.Error(), "http: request body too large"):
			return fmt.Errorf("%w: request body exceeds 1 MiB", models.ErrInvalidInput)
		default:
			return fmt.Errorf("%w: malformed JSON body: %s", models.ErrInvalidInput, err)
		}
	}
	// Reject trailing content after the first JSON value.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: request body must contain exactly one JSON object", models.ErrInvalidInput)
	}
	return nil
}

// PathInt64 parses a positive integer path parameter (e.g. /tickets/{id}).
func PathInt64(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: %s must be a positive integer, got %q", models.ErrInvalidInput, name, raw)
	}
	return id, nil
}

// QueryInt parses an optional integer query parameter, clamping out-of-range
// values into [min, max]. Non-numeric input falls back to the default; use
// QueryIntStrict where silently ignoring a malformed value would hide a bug.
func QueryInt(r *http.Request, name string, fallback, min, max int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// QueryIntStrict parses an optional integer query parameter.
//
// A value outside [min, max] is clamped, because a client asking for a million
// rows is better served by a large page than by an error. A value that is not a
// number at all is a client bug and is reported as such: falling back silently
// would hand the caller page 1 of results while they believe they asked for
// something else.
func QueryIntStrict(r *http.Request, name string, fallback, min, max int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be an integer, got %q", models.ErrInvalidInput, name, raw)
	}
	switch {
	case n < min:
		return min, nil
	case n > max:
		return max, nil
	default:
		return n, nil
	}
}

// QueryBool parses an optional boolean query parameter.
func QueryBool(r *http.Request, name string) (bool, bool) {
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	switch raw {
	case "":
		return false, false
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

// Pages computes total page count for a limit/total pair.
func Pages(total, limit int) int {
	if limit <= 0 || total == 0 {
		return 0
	}
	return (total + limit - 1) / limit
}
