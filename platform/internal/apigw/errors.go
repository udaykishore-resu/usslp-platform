package apigw

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ---------------------------------------------------------------------------
// Error responses
//
// One shape for every failure the gateway produces, because a client writing
// error handling against a front door that sometimes returns a JSON object and
// sometimes returns Go's default plain-text 500 page will handle neither well.
// The body carries a stable machine code, a human message and the request id,
// so a support ticket that pastes the response is immediately joinable to the
// access log and the trace.
// ---------------------------------------------------------------------------

// ErrorBody is the gateway's error representation.
type ErrorBody struct {
	// Code is a stable, machine-readable identifier. Clients branch on this;
	// the message is for humans and may change.
	Code string `json:"code"`
	// Message explains the failure without disclosing anything about another
	// tenant, an upstream's internals, or the shape of the credential store.
	Message string `json:"message"`
	// RequestID ties the response to the access log line and the trace.
	RequestID string `json:"request_id,omitempty"`
}

// apiError is an error carrying an HTTP status and a stable code.
type apiError struct {
	status int
	code   string
	err    error
	// headers are attached to the response, used by 429 for Retry-After and by
	// 401 for WWW-Authenticate.
	headers map[string]string
}

// Error implements error.
func (e *apiError) Error() string { return e.err.Error() }

// Unwrap keeps errors.Is working through the wrapper so that a sentinel
// returned deep in the key store is still matchable at the top.
func (e *apiError) Unwrap() error { return e.err }

func statusError(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, err: fmt.Errorf(format, args...)}
}

func errBadRequest(format string, args ...any) *apiError {
	return statusError(http.StatusBadRequest, "invalid_argument", format, args...)
}

func errUnauthorized(format string, args ...any) *apiError {
	e := statusError(http.StatusUnauthorized, "unauthenticated", format, args...)
	// RFC 7235: a 401 must say what would satisfy it. All three schemes are
	// advertised because a client holding a certificate and a client holding a
	// key both arrive here.
	e.headers = map[string]string{"WWW-Authenticate": `Bearer realm="usslp", charset="UTF-8"`}
	return e
}

func errForbidden(format string, args ...any) *apiError {
	return statusError(http.StatusForbidden, "permission_denied", format, args...)
}

// errNotFound is the answer to any question about a resource the caller may
// not see, including ones that exist. See the package documentation: 403 on a
// cross-tenant identifier confirms the identifier.
func errNotFound(format string, args ...any) *apiError {
	return statusError(http.StatusNotFound, "not_found", format, args...)
}

func errTooLarge(format string, args ...any) *apiError {
	return statusError(http.StatusRequestEntityTooLarge, "payload_too_large", format, args...)
}

func errInternal(format string, args ...any) *apiError {
	return statusError(http.StatusInternalServerError, "internal", format, args...)
}

func errUpstream(status int, code, format string, args ...any) *apiError {
	return statusError(status, code, format, args...)
}

// writeJSON emits a JSON body. Errors from the encoder are dropped: the status
// line is already on the wire, so there is nothing left to report to and the
// access log records the outcome either way.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Nothing the gateway returns is safe to cache by an intermediary: every
	// response is tenant-specific and most are authorisation-specific.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError renders an error as the gateway's standard body.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := http.StatusInternalServerError, "internal"
	message := "the gateway could not complete this request"
	var ae *apiError
	if errors.As(err, &ae) {
		status, code = ae.status, ae.code
		for k, v := range ae.headers {
			w.Header().Set(k, v)
		}
		// A 5xx message is the gateway's own text, never the underlying error:
		// upstream error strings leak topology, hostnames and sometimes other
		// tenants' identifiers into a response a customer reads.
		if status < http.StatusInternalServerError {
			message = ae.Error()
		}
	}
	writeJSON(w, status, ErrorBody{Code: code, Message: message, RequestID: RequestIDFrom(r.Context())})
}
