package harness

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/harness/harness-go-sdk/harness/nextgen"
)

// ErrorBody returns a safe SDK response body for diagnostics.
func ErrorBody(err error) string {
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

// WrapAPIError preserves an SDK error while adding any useful response body.
func WrapAPIError(message string, err error) error {
	if err == nil {
		return nil
	}
	if body := ErrorBody(err); body != "" {
		return fmt.Errorf("%s: %w (body: %s)", message, err, body)
	}
	return fmt.Errorf("%s: %w", message, err)
}

// IsAgentNotFound identifies the Harness agent-not-found response.
func IsAgentNotFound(err error) bool {
	return errors.Is(err, ErrAgentNotFound) ||
		strings.Contains(strings.ToLower(ErrorBody(err)), "agent not found")
}

// IsAgentAlreadyExists identifies the Harness agent conflict response.
func IsAgentAlreadyExists(err error) bool {
	return errors.Is(err, ErrAgentAlreadyExists) ||
		strings.Contains(strings.ToLower(ErrorBody(err)), "agent already exists")
}

func isAgentAlreadyExists(response *http.Response, err error) bool {
	if response != nil && response.StatusCode == http.StatusConflict {
		return true
	}
	return strings.Contains(strings.ToLower(ErrorBody(err)), "agent already exists")
}

func isNotFound(response *http.Response, err error) bool {
	if response != nil && response.StatusCode == http.StatusNotFound {
		return true
	}
	return strings.Contains(strings.ToLower(ErrorBody(err)), "not found")
}

func isMappingAlreadyExists(response *http.Response, err error) bool {
	if response != nil && response.StatusCode == http.StatusConflict {
		return true
	}
	return strings.Contains(strings.ToLower(ErrorBody(err)), "already exists")
}

func isAmbiguousCreateResponse(response *http.Response) bool {
	return response == nil ||
		response.StatusCode == http.StatusRequestTimeout ||
		response.StatusCode >= http.StatusInternalServerError
}

func safeAPIError(operation string, agentIdentifier string, response *http.Response, err error) error {
	if response != nil {
		return fmt.Errorf(
			"%s for agentIdentifier=%q failed with HTTP %d: %w",
			operation,
			agentIdentifier,
			response.StatusCode,
			err,
		)
	}
	return fmt.Errorf("%s for agentIdentifier=%q failed: %w", operation, agentIdentifier, err)
}
