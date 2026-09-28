package server

import (
	"net/http"
)

// Envelope is the standard API response shape (owner directive 28 Sep:
// "json response like API response with error code and reason").
// Every /v1 endpoint answers with exactly this shape — success or error:
//
//	{"success": true,  "code": "OK",              "message": "", "data": {...}}
//	{"success": false, "code": "UNAUTHORIZED",    "message": "missing or invalid bearer token", "reason": "auth"}
//	{"success": false, "code": "OVERLOADED",      "message": "provider 529", "reason": "provider"}
//
// code is a STABLE machine-readable token (never the HTTP phrase), message
// is the human explanation, reason is the failure category (auth | request
// | not_found | state | provider | internal). Clients branch on code,
// display message.
type Envelope struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Reason  string `json:"reason,omitempty"` // failure category; empty on success
	Data    any    `json:"data,omitempty"`
}

// Failure categories for reason.
const (
	ReasonAuth     = "auth"
	ReasonRequest  = "request"
	ReasonNotFound = "not_found"
	ReasonState    = "state"
	ReasonProvider = "provider"
	ReasonInternal = "internal"
)

// codeFor maps an HTTP status to the stable code table. Codes are tokens,
// not phrases — clients switch on them.
func codeFor(status int) string {
	switch status {
	case http.StatusOK:
		return "OK"
	case http.StatusAccepted:
		return "ACCEPTED"
	case http.StatusBadRequest:
		return "BAD_REQUEST"
	case http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusConflict:
		return "CONFLICT"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusNotImplemented:
		return "NOT_IMPLEMENTED"
	case http.StatusInternalServerError:
		return "INTERNAL"
	default:
		return "ERROR"
	}
}

// reasonFor maps the status to the failure category.
func reasonFor(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return ReasonAuth
	case status == http.StatusBadRequest:
		return ReasonRequest
	case status == http.StatusNotFound:
		return ReasonNotFound
	case status == http.StatusServiceUnavailable, status == http.StatusNotImplemented:
		return ReasonState
	case status >= 500:
		return ReasonInternal
	default:
		return ""
	}
}

// okEnvelope wraps data in a success envelope.
func okEnvelope(status int, data any) Envelope {
	return Envelope{Success: true, Code: codeFor(status), Data: data}
}

// errEnvelope builds a failure envelope. reasonOverride lets a handler
// classify precisely (e.g. 500 from a provider outage → provider, not
// internal).
func errEnvelope(status int, msg, reasonOverride string) Envelope {
	reason := reasonOverride
	if reason == "" {
		reason = reasonFor(status)
	}
	return Envelope{Success: false, Code: codeFor(status), Message: msg, Reason: reason}
}

// writeOK emits a success envelope.
func writeOK(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, okEnvelope(status, data))
}

// writeErr emits a failure envelope (drop-in replacement for writeError).
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errEnvelope(status, msg, ""))
}
