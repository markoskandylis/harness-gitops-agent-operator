package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/harness/harness-go-sdk/harness/nextgen"
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

	mappings, err := (sdkAppProjectMappingAPI{}).List(context.Background(), session, sdkMappingTestRequest())
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

	if _, err := (sdkAppProjectMappingAPI{}).List(context.Background(), session, sdkMappingTestRequest()); err != nil {
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

	if _, err := (sdkAppProjectMappingAPI{}).List(context.Background(), session, sdkMappingTestRequest()); err == nil {
		t.Fatal("expected the canonical server error to be returned")
	}
	want := []string{"/gitops/api/v2/agents/org.mapping-agent/appprojectsmappings"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("canonical server error queried alternate candidates: got %#v, want %#v", paths, want)
	}
}

func TestSDKAgentReadinessUsesConnectedHealthyPayload(t *testing.T) {
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"identifier":"mapping-agent",
			"health":{
				"connectionStatus":"CONNECTED",
				"harnessGitopsAgent":{
					"status":"HEALTHY",
					"message":"All Argo CD components are healthy"
				}
			}
		}`))
	}))

	readiness, err := (sdkAgentReadinessChecker{}).Readiness(
		context.Background(),
		session,
		newMappingTestAgent("mapping-resource"),
		mappingTestAgentID,
	)
	if err != nil {
		t.Fatalf("get agent readiness: %v", err)
	}
	if !readiness.Exists || !readiness.Ready {
		t.Fatalf("expected the agent to be ready, got %#v", readiness)
	}
}

func TestHarnessAgentReadinessRequiresConnectedAndHealthy(t *testing.T) {
	tests := []struct {
		name       string
		agent      nextgen.V1Agent
		wantReady  bool
		wantExists bool
	}{
		{
			name:       "health not reported",
			agent:      nextgen.V1Agent{},
			wantReady:  false,
			wantExists: true,
		},
		{
			name:       "connected but unhealthy",
			agent:      mappingSDKTestAgentHealth(nextgen.CONNECTED_V1ConnectedStatus, nextgen.UNHEALTHY_Servicev1HealthStatus),
			wantReady:  false,
			wantExists: true,
		},
		{
			name:       "healthy but disconnected",
			agent:      mappingSDKTestAgentHealth(nextgen.DISCONNECTED_V1ConnectedStatus, nextgen.HEALTHY_Servicev1HealthStatus),
			wantReady:  false,
			wantExists: true,
		},
		{
			name:       "connected and healthy",
			agent:      mappingSDKTestAgentHealth(nextgen.CONNECTED_V1ConnectedStatus, nextgen.HEALTHY_Servicev1HealthStatus),
			wantReady:  true,
			wantExists: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readiness := readinessFromHarnessAgent(tt.agent)
			if readiness.Exists != tt.wantExists || readiness.Ready != tt.wantReady {
				t.Fatalf("unexpected readiness: got %#v", readiness)
			}
			if readiness.Message == "" {
				t.Fatal("readiness message must explain the observed state")
			}
		})
	}
}

func mappingSDKTestAgentHealth(
	connection nextgen.V1ConnectedStatus,
	status nextgen.Servicev1HealthStatus,
) nextgen.V1Agent {
	return nextgen.V1Agent{Health: &nextgen.V1AgentHealth{
		ConnectionStatus: &connection,
		HarnessGitopsAgent: &nextgen.V1AgentComponentHealth{
			Status:  &status,
			Message: "test health",
		},
	}}
}

func newSDKMappingTestSession(t *testing.T, handler http.Handler) *HarnessSession {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cfg := nextgen.NewConfiguration()
	cfg.BasePath = server.URL
	cfg.HTTPClient.RetryMax = 0
	return &HarnessSession{
		Client:  nextgen.NewAPIClient(cfg),
		AuthCtx: context.Background(),
	}
}

func sdkMappingTestRequest() appProjectMappingRequest {
	return appProjectMappingRequest{
		AgentIdentifier:   "mapping-agent",
		AccountIdentifier: "account",
		AgentScope:        "ORG",
		// At ORG scope both scopes hold the same org. That coincidence is why a
		// single org field survived until ACCOUNT scope exposed it.
		Agent:           harnessScope{OrgIdentifier: "org"},
		Mapping:         harnessScope{OrgIdentifier: "org"},
		ArgoProjectName: "default",
	}
}

// sdkAccountScopeRequest is deliberately built with DIFFERENT agent and mapping
// scopes. sdkMappingTestRequest sets both orgs to "org", which means no test
// using it can tell the two apart -- that is precisely how B12 stayed invisible.
func sdkAccountScopeRequest() appProjectMappingRequest {
	return appProjectMappingRequest{
		AgentIdentifier:   "mapping-agent",
		AccountIdentifier: "account",
		AgentScope:        "ACCOUNT",
		// ACCOUNT-scoped agents live at account level: no org, no project.
		Agent: harnessScope{},
		// The mapped project always has both.
		Mapping:         harnessScope{OrgIdentifier: "harness_controllers", ProjectIdentifier: "hub_orchistrator"},
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

	if err := (sdkAppProjectMappingAPI{}).Create(context.Background(), session, sdkAccountScopeRequest()); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if body.OrgIdentifier != "harness_controllers" {
		t.Fatalf("create body orgIdentifier = %q, want the MAPPED project's org", body.OrgIdentifier)
	}
	if body.ProjectIdentifier != "hub_orchistrator" {
		t.Fatalf("create body projectIdentifier = %q, want the MAPPED project", body.ProjectIdentifier)
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

	if _, err := (sdkAppProjectMappingAPI{}).List(context.Background(), session, sdkAccountScopeRequest()); err != nil {
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

	if err := (sdkAppProjectMappingAPI{}).Delete(
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

	if err := (sdkAppProjectMappingAPI{}).Delete(
		context.Background(), session, sdkAccountScopeRequest(), "mapping-1",
	); err == nil {
		t.Fatal("expected the canonical server error to be returned")
	}
	want := []string{"/gitops/api/v2/agents/account.mapping-agent/appprojectsmapping/mapping-1"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("non-404 delete error was retried on alternate candidates: got %#v, want %#v", paths, want)
	}
}
