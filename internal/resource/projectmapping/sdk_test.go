package projectmapping

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/harness/harness-go-sdk/harness/nextgen"

	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

func TestSDKMappingListStopsAfterSuccessfulEmptyCanonicalResponse(t *testing.T) {
	var paths []string
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/gitops/api/v2/agents/org.mapping-agent/appprojectsmappings" {
			_, _ = w.Write([]byte(`{"appProjectMappings":[]}`))
			return
		}
		http.Error(w, `{"message":"unexpected alternate candidate"}`, http.StatusInternalServerError)
	}))

	mappings, err := (SDKProjectMappingAPI{}).List(context.Background(), session, sdkMappingTestRequest())
	if err != nil {
		t.Fatalf("list mappings: %v", err)
	}
	if len(mappings) != 0 {
		t.Fatalf("expected an empty mapping list, got %#v", mappings)
	}
	want := []string{"/gitops/api/v2/agents/org.mapping-agent/appprojectsmappings"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("successful canonical response queried alternate candidates: got %#v, want %#v", paths, want)
	}
}

func TestSDKMappingListFallsBackOnlyAfterNotFound(t *testing.T) {
	var paths []string
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/gitops/api/v2/agents/org.mapping-agent/appprojectsmappings" {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"appProjectMappings":[]}`))
	}))

	if _, err := (SDKProjectMappingAPI{}).List(context.Background(), session, sdkMappingTestRequest()); err != nil {
		t.Fatalf("list mappings: %v", err)
	}
	want := []string{
		"/gitops/api/v2/agents/org.mapping-agent/appprojectsmappings",
		"/gitops/api/v2/agents/mapping-agent/appprojectsmappings",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("unexpected candidate requests: got %#v, want %#v", paths, want)
	}
}

func TestSDKMappingListDoesNotHideCanonicalServerError(t *testing.T) {
	var paths []string
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"message":"server error"}`, http.StatusInternalServerError)
	}))

	if _, err := (SDKProjectMappingAPI{}).List(context.Background(), session, sdkMappingTestRequest()); err == nil {
		t.Fatal("expected the canonical server error to be returned")
	}
	want := []string{"/gitops/api/v2/agents/org.mapping-agent/appprojectsmappings"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("canonical server error queried alternate candidates: got %#v, want %#v", paths, want)
	}
}

func TestSDKMappingListDoesNotTreatDeniedNotFoundBodyAsAbsent(t *testing.T) {
	var paths []string
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		http.Error(w, `{"message":"project not found"}`, http.StatusForbidden)
	}))

	_, err := (SDKProjectMappingAPI{}).List(
		context.Background(),
		session,
		sdkMappingTestRequest(),
	)
	if err == nil {
		t.Fatal("expected denied list to fail")
	}
	if got := harnessapi.VerdictOf(err); got != harnessapi.VerdictDenied {
		t.Fatalf("list verdict = %q, want %q", got, harnessapi.VerdictDenied)
	}
	want := []string{"/gitops/api/v2/agents/org.mapping-agent/appprojectsmappings"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("denied list queried alternate candidates: got %#v, want %#v", paths, want)
	}
}

func newSDKMappingTestSession(t *testing.T, handler http.Handler) *harnessapi.Session {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cfg := nextgen.NewConfiguration()
	cfg.BasePath = server.URL
	cfg.HTTPClient.RetryMax = 0
	return newTestHarnessSession(t, nextgen.NewAPIClient(cfg))
}

func sdkMappingTestRequest() ProjectMappingRequest {
	return ProjectMappingRequest{
		AgentIdentifier:   "mapping-agent",
		AccountIdentifier: "account",
		AgentScope:        "ORG",
		// At ORG scope both scopes hold the same org. That coincidence is why a
		// single org field survived until ACCOUNT scope exposed it.
		Agent:           Scope{OrgIdentifier: "org"},
		Mapping:         Scope{OrgIdentifier: "org"},
		ArgoProjectName: "default",
	}
}

// sdkAccountScopeRequest is deliberately built with DIFFERENT agent and mapping
// scopes. sdkMappingTestRequest sets both orgs to "org", which means no test
// using it can tell the two apart -- that is precisely how B12 stayed invisible.
func sdkAccountScopeRequest() ProjectMappingRequest {
	return ProjectMappingRequest{
		AgentIdentifier:   "mapping-agent",
		AccountIdentifier: "account",
		AgentScope:        "ACCOUNT",
		// ACCOUNT-scoped agents live at account level: no org, no project.
		Agent: Scope{},
		// The mapped project always has both.
		Mapping:         Scope{OrgIdentifier: "harness_controllers", ProjectIdentifier: "hub_orchistrator"},
		ArgoProjectName: "default",
	}
}

// TestSDKMappingCreateSendsMappingScope pins the create body to the MAPPED
// project's scope. Sending the agent scope here is B12: Harness receives a
// project with no org to resolve it under and silently maps nothing.
func TestSDKMappingCreateSendsMappingScope(t *testing.T) {
	var body nextgen.V1AppProjectMappingCreateRequestV2
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"identifier":"mapping-1"}`))
	}))

	request := sdkAccountScopeRequest()
	request.AutoCreateServiceEnv = true
	if _, err := (SDKProjectMappingAPI{}).Create(context.Background(), session, request); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if body.OrgIdentifier != "harness_controllers" {
		t.Fatalf("create body orgIdentifier = %q, want the MAPPED project's org", body.OrgIdentifier)
	}
	if body.ProjectIdentifier != "hub_orchistrator" {
		t.Fatalf("create body projectIdentifier = %q, want the MAPPED project", body.ProjectIdentifier)
	}
	if !body.AutoCreateServiceEnv {
		t.Fatal("create body did not preserve autoCreateServiceEnv")
	}
}

func TestSDKMappingCreateStopsOnNonNotFoundError(t *testing.T) {
	var paths []string
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"message":"server error"}`, http.StatusInternalServerError)
	}))

	if _, err := (SDKProjectMappingAPI{}).Create(
		context.Background(),
		session,
		sdkAccountScopeRequest(),
	); err == nil {
		t.Fatal("expected the canonical server error to be returned")
	}
	want := []string{"/gitops/api/v2/agents/account.mapping-agent/appprojectsmapping"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("non-404 create error was retried on alternate candidates: got %#v, want %#v", paths, want)
	}
}

// TestSDKMappingListSendsAgentScope is the other half: the List query locates
// the AGENT, so at ACCOUNT scope it must carry no org and no project. Sending
// the mapping's org here would break the lookup that currently works.
func TestSDKMappingListSendsAgentScope(t *testing.T) {
	var query url.Values
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"appProjectMappings":[]}`))
	}))

	if _, err := (SDKProjectMappingAPI{}).List(context.Background(), session, sdkAccountScopeRequest()); err != nil {
		t.Fatalf("list mappings: %v", err)
	}
	if got := query.Get("orgIdentifier"); got != "" {
		t.Fatalf("list query orgIdentifier = %q, want empty for an ACCOUNT-scoped agent", got)
	}
	if got := query.Get("projectIdentifier"); got != "" {
		t.Fatalf("list query projectIdentifier = %q, want empty for an ACCOUNT-scoped agent", got)
	}
	if got := query.Get("accountIdentifier"); got != "account" {
		t.Fatalf("list query accountIdentifier = %q, want %q", got, "account")
	}
}

// TestSDKMappingDeleteSendsMappingScope: the delete path had the same
// conflation as create.
func TestSDKMappingDeleteSendsMappingScope(t *testing.T) {
	var query url.Values
	var path string
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))

	if err := (SDKProjectMappingAPI{}).Delete(
		context.Background(), session, sdkAccountScopeRequest(), "mapping-1",
	); err != nil {
		t.Fatalf("delete mapping: %v", err)
	}
	if want := "/gitops/api/v2/agents/account.mapping-agent/appprojectsmapping/mapping-1"; path != want {
		t.Fatalf("delete path = %q, want %q", path, want)
	}
	if got := query.Get("orgIdentifier"); got != "harness_controllers" {
		t.Fatalf("delete query orgIdentifier = %q, want the MAPPED project's org", got)
	}
	if got := query.Get("projectIdentifier"); got != "hub_orchistrator" {
		t.Fatalf("delete query projectIdentifier = %q, want the MAPPED project", got)
	}
}

// TestSDKMappingDeleteStopsOnNonNotFoundError mirrors List: only a 404 may
// advance to the next identifier shape. Retrying a non-404 on an alternate path
// cannot succeed for a different reason and blocks the single reconcile worker
// for up to another full client timeout.
func TestSDKMappingDeleteStopsOnNonNotFoundError(t *testing.T) {
	var paths []string
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"message":"server error"}`, http.StatusInternalServerError)
	}))

	if err := (SDKProjectMappingAPI{}).Delete(
		context.Background(), session, sdkAccountScopeRequest(), "mapping-1",
	); err == nil {
		t.Fatal("expected the canonical server error to be returned")
	}
	want := []string{"/gitops/api/v2/agents/account.mapping-agent/appprojectsmapping/mapping-1"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("non-404 delete error was retried on alternate candidates: got %#v, want %#v", paths, want)
	}
}

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
		session     func(*testing.T) *harnessapi.Session
		wantUnknown bool
	}{
		{
			name: "transport error without an HTTP response",
			session: func(t *testing.T) *harnessapi.Session {
				cfg := nextgen.NewConfiguration()
				cfg.HTTPClient.RetryMax = 0
				cfg.HTTPClient.HTTPClient.Transport = roundTripperFunc(
					func(*http.Request) (*http.Response, error) {
						return nil, errors.New("connection closed before a response")
					},
				)
				return newTestHarnessSession(t, nextgen.NewAPIClient(cfg))
			},
			wantUnknown: true,
		},
		{
			name: "request timeout",
			session: func(t *testing.T) *harnessapi.Session {
				return testStatusSession(t, http.StatusRequestTimeout)
			},
			wantUnknown: true,
		},
		{
			name: "server error",
			session: func(t *testing.T) *harnessapi.Session {
				return testStatusSession(t, http.StatusBadGateway)
			},
			wantUnknown: true,
		},
		{
			name: "rate limited",
			session: func(t *testing.T) *harnessapi.Session {
				return testStatusSession(t, http.StatusTooManyRequests)
			},
			wantUnknown: true,
		},
		{
			name: "definite client rejection",
			session: func(t *testing.T) *harnessapi.Session {
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

func testSession(t *testing.T, handler http.Handler) *harnessapi.Session {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cfg := nextgen.NewConfiguration()
	cfg.BasePath = server.URL
	cfg.HTTPClient.RetryMax = 0
	return newTestHarnessSession(t, nextgen.NewAPIClient(cfg))
}

func testStatusSession(t *testing.T, status int) *harnessapi.Session {
	t.Helper()
	return testSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(status), status)
	}))
}

func newTestHarnessSession(t *testing.T, client *nextgen.APIClient) *harnessapi.Session {
	t.Helper()
	session, err := harnessapi.NewSessionWithClient("test-api-key", client)
	if err != nil {
		t.Fatalf("create Harness session: %v", err)
	}
	return session
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
