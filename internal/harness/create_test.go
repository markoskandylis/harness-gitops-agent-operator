package harness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/harness/harness-go-sdk/harness/nextgen"
)

func TestProjectMappingCreateReturnsRemoteIdentity(t *testing.T) {
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"identifier":"mapping-id",
			"accountIdentifier":"account",
			"orgIdentifier":"mapped-org",
			"projectIdentifier":"mapped-project",
			"argoProjectName":"default"
		}`))
	}))

	mapping, err := (SDKProjectMappingAPI{}).Create(
		context.Background(),
		session,
		testMappingRequest(),
	)
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if mapping.Identifier != "mapping-id" || mapping.AgentIdentifier != "account.agent-id" {
		t.Fatalf("unexpected mapping identity: %#v", mapping)
	}
}

func TestProjectMappingCreateClassifiesAmbiguousOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		session     func(*testing.T) *Session
		wantUnknown bool
	}{
		{
			name: "transport error without an HTTP response",
			session: func(t *testing.T) *Session {
				cfg := nextgen.NewConfiguration()
				cfg.HTTPClient.RetryMax = 0
				cfg.HTTPClient.HTTPClient.Transport = roundTripperFunc(
					func(*http.Request) (*http.Response, error) {
						return nil, errors.New("connection closed before a response")
					},
				)
				return &Session{client: nextgen.NewAPIClient(cfg)}
			},
			wantUnknown: true,
		},
		{
			name: "request timeout",
			session: func(t *testing.T) *Session {
				return testStatusSession(t, http.StatusRequestTimeout)
			},
			wantUnknown: true,
		},
		{
			name: "server error",
			session: func(t *testing.T) *Session {
				return testStatusSession(t, http.StatusBadGateway)
			},
			wantUnknown: true,
		},
		{
			name: "definite client rejection",
			session: func(t *testing.T) *Session {
				return testStatusSession(t, http.StatusBadRequest)
			},
			wantUnknown: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := (SDKProjectMappingAPI{}).Create(
				context.Background(),
				test.session(t),
				testMappingRequest(),
			)
			if err == nil {
				t.Fatal("expected create to fail")
			}
			if got := errors.Is(err, ErrProjectMappingCreateOutcomeUnknown); got != test.wantUnknown {
				t.Fatalf("outcomeUnknown = %t, want %t: %v", got, test.wantUnknown, err)
			}
		})
	}
}

func TestProjectMappingCreateRejectsEmptySuccessfulResponse(t *testing.T) {
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))

	_, err := (SDKProjectMappingAPI{}).Create(
		context.Background(),
		session,
		testMappingRequest(),
	)
	if !errors.Is(err, ErrProjectMappingCreateOutcomeUnknown) {
		t.Fatalf("expected unknown create outcome, got %v", err)
	}
}

func TestAgentCreateReturnsRawAndPrefixedIdentifiers(t *testing.T) {
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"identifier":"agent-id",
			"prefixedIdentifier":"account.agent-id",
			"credentials":{"privateKey":"token"}
		}`))
	}))

	result, err := (SDKAgentAPI{}).Create(
		context.Background(),
		session,
		CreateAgentRequest{
			Agent: Agent{
				Identifier:        "agent-id",
				Name:              "Agent",
				AccountIdentifier: "account",
				Scope:             "ACCOUNT",
				Type:              "MANAGED_ARGO_PROVIDER",
				Operator:          "ARGO",
			},
			Namespace: "default",
		},
	)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if result.Identifier != "agent-id" ||
		result.PrefixedIdentifier != "account.agent-id" ||
		result.InitialToken != "token" {
		t.Fatalf("unexpected create result: %#v", result)
	}
}

func TestAgentCreateClassifiesConflictByHTTPStatus(t *testing.T) {
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"message":"conflict without canonical wording"}`, http.StatusConflict)
	}))

	_, err := (SDKAgentAPI{}).Create(
		context.Background(),
		session,
		CreateAgentRequest{Agent: Agent{
			Identifier:        "agent-id",
			AccountIdentifier: "account",
			Scope:             "ACCOUNT",
		}},
	)
	if !errors.Is(err, ErrAgentAlreadyExists) {
		t.Fatalf("expected already-exists classification, got %v", err)
	}
}

func TestAgentCreateClassifiesServerErrorAsOutcomeUnknown(t *testing.T) {
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"gateway failed after forwarding"}`, http.StatusBadGateway)
	}))

	_, err := (SDKAgentAPI{}).Create(
		context.Background(),
		session,
		CreateAgentRequest{Agent: Agent{
			Identifier:        "agent-id",
			AccountIdentifier: "account",
			Scope:             "ACCOUNT",
		}},
	)
	if !errors.Is(err, ErrAgentCreateOutcomeUnknown) {
		t.Fatalf("expected unknown create outcome, got %v", err)
	}
}

func testSession(t *testing.T, handler http.Handler) *Session {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cfg := nextgen.NewConfiguration()
	cfg.BasePath = server.URL
	cfg.HTTPClient.RetryMax = 0
	return &Session{
		client: nextgen.NewAPIClient(cfg),
	}
}

func testStatusSession(t *testing.T, status int) *Session {
	t.Helper()
	return testSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(status), status)
	}))
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testMappingRequest() ProjectMappingRequest {
	return ProjectMappingRequest{
		AccountIdentifier: "account",
		AgentIdentifier:   "agent-id",
		AgentScope:        "ACCOUNT",
		Mapping: Scope{
			OrgIdentifier:     "mapped-org",
			ProjectIdentifier: "mapped-project",
		},
		ArgoProjectName: "default",
	}
}
