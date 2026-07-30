package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestClassifyResponseUsesStatusAsAuthority(t *testing.T) {
	tests := []struct {
		name     string
		response *http.Response
		err      error
		want     Verdict
	}{
		{
			name: "missing response is transient",
			err:  errors.New("connection closed"),
			want: VerdictTransient,
		},
		{
			name:     "not found is absent",
			response: &http.Response{StatusCode: http.StatusNotFound},
			err:      errors.New("not found"),
			want:     VerdictAbsent,
		},
		{
			name:     "conflict is explicit",
			response: &http.Response{StatusCode: http.StatusConflict},
			err:      errors.New("conflict"),
			want:     VerdictConflict,
		},
		{
			name:     "unauthorized is denied",
			response: &http.Response{StatusCode: http.StatusUnauthorized},
			err:      errors.New("not found"),
			want:     VerdictDenied,
		},
		{
			name:     "forbidden not-found wording is still denied",
			response: &http.Response{StatusCode: http.StatusForbidden},
			err:      errors.New("project not found"),
			want:     VerdictDenied,
		},
		{
			name:     "request timeout is transient",
			response: &http.Response{StatusCode: http.StatusRequestTimeout},
			err:      context.DeadlineExceeded,
			want:     VerdictTransient,
		},
		{
			name:     "rate limit is transient",
			response: &http.Response{StatusCode: http.StatusTooManyRequests},
			err:      errors.New("rate limited"),
			want:     VerdictTransient,
		},
		{
			name:     "server error is transient",
			response: &http.Response{StatusCode: http.StatusBadGateway},
			err:      errors.New("gateway unavailable"),
			want:     VerdictTransient,
		},
		{
			name:     "other client error is definite",
			response: &http.Response{StatusCode: http.StatusUnprocessableEntity},
			err:      errors.New("invalid request"),
			want:     VerdictFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyResponse(test.response, test.err); got != test.want {
				t.Fatalf("ClassifyResponse() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAPIErrorRetainsVerdictThroughWrapping(t *testing.T) {
	cause := errors.New("project not found")
	err := APIError(
		"list mappings",
		`agentIdentifier="agent"`,
		&http.Response{StatusCode: http.StatusForbidden},
		cause,
	)
	if got := VerdictOf(fmt.Errorf("reconcile failed: %w", err)); got != VerdictDenied {
		t.Fatalf("VerdictOf(wrapped error) = %q, want %q", got, VerdictDenied)
	}
	if !errors.Is(err, cause) {
		t.Fatal("APIError did not retain its cause")
	}
	if got := VerdictOf(errors.New("not found")); got != VerdictFailed {
		t.Fatalf("plain error verdict = %q, want %q", got, VerdictFailed)
	}
}
