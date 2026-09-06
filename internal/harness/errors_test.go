package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/harness/harness-go-sdk/harness/nextgen"
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

type errorTestTransport func(*http.Request) (*http.Response, error)

func (f errorTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Obtain a real SDK error without network access or constructing private SDK fields.
func sdkResponseError(t *testing.T, code int, body string) (*http.Response, error) {
	t.Helper()
	cfg := newSDKConfiguration(DefaultEndpoint)
	cfg.HTTPClient.HTTPClient.Transport = errorTestTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: code, Status: http.StatusText(code), Request: req,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	session, err := NewSessionWithClient("test-key", nextgen.NewAPIClient(cfg))
	if err != nil {
		t.Fatal(err)
	}
	_, response, err := session.Client().AgentApi.AgentServiceForServerGet(
		session.AuthContext(context.Background()), "agent", "account", nil)
	if err == nil {
		t.Fatal("expected an SDK failure")
	}
	return response, err
}

func TestAPIErrorNeverFormatsProviderBodyOrTransportCause(t *testing.T) {
	const sensitive = "synthetic-secret-must-not-appear"
	response, sdkErr := sdkResponseError(t, http.StatusForbidden,
		`{"message":"`+sensitive+`","credentials":{"privateKey":"`+sensitive+`"}}`)
	for index, cause := range []error{sdkErr, fmt.Errorf("transport URL includes %s: %w", sensitive, context.DeadlineExceeded)} {
		err := APIError("get agent", `agentIdentifier="agent"`, response, cause)
		for _, rendered := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%v", errors.Join(err, errors.New("retry")))} {
			if strings.Contains(rendered, sensitive) || strings.Contains(rendered, "credentials") {
				t.Fatal("provider data leaked into a diagnostic error")
			}
			if !strings.Contains(rendered, "HTTP 403") || !strings.Contains(rendered, "Denied") {
				t.Fatal("safe diagnostics lost their HTTP status or verdict")
			}
		}
		if index == 0 {
			var original nextgen.GenericSwaggerError
			if !errors.As(err, &original) || !strings.Contains(string(original.Body()), sensitive) {
				t.Fatal("safe diagnostics lost their inspectable SDK cause")
			}
		} else if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("safe diagnostics lost their transport cause")
		}
	}
	if APIError("get", "agent", nil, nil) != nil {
		t.Fatal("successful calls must not acquire an error")
	}
}

func TestDuplicateBodyNeverOverridesDeniedAbsentOrTransientStatus(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 409, 429, 500, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			response, err := sdkResponseError(t, code, `{"message":"agent already exists"}`)
			want := code == http.StatusBadRequest || code == http.StatusConflict
			if IsAlreadyExists(response, err, "agent already exists") != want {
				t.Fatal("duplicate message overrode the authoritative HTTP status")
			}
		})
	}
	if IsAlreadyExists(nil, errors.New("already exists"), "already exists") {
		t.Fatal("a missing response cannot prove a duplicate")
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
