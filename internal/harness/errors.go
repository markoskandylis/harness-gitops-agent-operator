package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/harness/harness-go-sdk/harness/nextgen"
)

// Verdict is the status-based outcome of one failed Harness API call.
// Absence is deliberately limited to HTTP 404: Harness denial messages may
// contain "not found", but a resource that cannot be read is not proven absent.
type Verdict string

const (
	VerdictAbsent    Verdict = "Absent"
	VerdictConflict  Verdict = "Conflict"
	VerdictDenied    Verdict = "Denied"
	VerdictTransient Verdict = "Transient"
	VerdictFailed    Verdict = "Failed"
)

// ResponseError retains an API verdict through wrapping.
type ResponseError struct {
	Operation           string
	ResourceDescription string
	StatusCode          int
	Verdict             Verdict
	cause               error
}

func (e *ResponseError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf(
			"%s for %s failed with HTTP %d (%s)",
			e.Operation,
			e.ResourceDescription,
			e.StatusCode,
			e.Verdict,
		)
	}
	return fmt.Sprintf(
		"%s for %s failed (%s)",
		e.Operation,
		e.ResourceDescription,
		e.Verdict,
	)
}

func (e *ResponseError) Unwrap() error {
	return e.cause
}

// ClassifyResponse derives an API verdict from transport and HTTP state. Body
// text is diagnostic only and never changes the verdict.
func ClassifyResponse(response *http.Response, err error) Verdict {
	if response == nil {
		return VerdictTransient
	}
	switch {
	case response.StatusCode == http.StatusNotFound:
		return VerdictAbsent
	case response.StatusCode == http.StatusConflict:
		return VerdictConflict
	case response.StatusCode == http.StatusUnauthorized,
		response.StatusCode == http.StatusForbidden:
		return VerdictDenied
	case response.StatusCode == http.StatusRequestTimeout,
		response.StatusCode == http.StatusTooManyRequests,
		response.StatusCode >= http.StatusInternalServerError:
		return VerdictTransient
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return VerdictTransient
	}
	var timeoutError interface{ Timeout() bool }
	if errors.As(err, &timeoutError) && timeoutError.Timeout() {
		return VerdictTransient
	}
	return VerdictFailed
}

// VerdictOf returns the retained API verdict. Unclassified errors are definite
// failures and must never be interpreted as absence.
func VerdictOf(err error) Verdict {
	if err == nil {
		return ""
	}
	var responseErr *ResponseError
	if errors.As(err, &responseErr) {
		return responseErr.Verdict
	}
	return VerdictFailed
}

// errorBody is for internal compatibility checks only. Provider bodies can
// contain credentials; never return them to reconcilers, logs or status.
func errorBody(err error) string {
	var swaggerErr nextgen.GenericSwaggerError
	if errors.As(err, &swaggerErr) {
		return strings.TrimSpace(string(swaggerErr.Body()))
	}
	var swaggerErrPointer *nextgen.GenericSwaggerError
	if errors.As(err, &swaggerErrPointer) && swaggerErrPointer != nil {
		return strings.TrimSpace(string(swaggerErrPointer.Body()))
	}
	return ""
}

// IsAlreadyExists supports the legacy HTTP 400 duplicate responses used by
// Harness. Denied, absent and transient responses must keep their HTTP verdict,
// regardless of misleading body text. Only a boolean leaves this boundary.
func IsAlreadyExists(response *http.Response, err error, phrase string) bool {
	if err == nil || response == nil {
		return false
	}
	if response.StatusCode == http.StatusConflict {
		return true
	}
	return response.StatusCode == http.StatusBadRequest && phrase != "" &&
		strings.Contains(strings.ToLower(errorBody(err)), strings.ToLower(phrase))
}

// APIError exposes only operation, resource identity, HTTP status and verdict.
// Keep the cause for errors.Is/As, but never format SDK bodies or transport
// errors (which can contain URLs or credentials) into logs or CR status.
func APIError(
	operation string,
	resourceDescription string,
	response *http.Response,
	err error,
) error {
	if err == nil {
		return nil
	}
	statusCode := 0
	if response != nil {
		statusCode = response.StatusCode
	}
	return &ResponseError{
		Operation:           operation,
		ResourceDescription: resourceDescription,
		StatusCode:          statusCode,
		Verdict:             ClassifyResponse(response, err),
		cause:               err,
	}
}
