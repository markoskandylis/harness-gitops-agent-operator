package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/harness/harness-go-sdk/harness/nextgen"

	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

func TestSDKAgentLookupUsesScopedPathCandidatesAndQueries(t *testing.T) {
	tests := []struct {
		name        string
		agent       Agent
		wantPathID  string
		wantOrg     string
		wantProject string
	}{
		{
			name: "ACCOUNT",
			agent: Agent{
				Identifier:        "agent-account-102",
				AccountIdentifier: "account-101",
				Scope:             "ACCOUNT",
			},
			wantPathID: "account.agent-account-102",
		},
		{
			name: "ORG",
			agent: Agent{
				Identifier:        "agent-org-203",
				AccountIdentifier: "account-201",
				OrgIdentifier:     "org-202",
				Scope:             "ORG",
			},
			wantPathID: "org.agent-org-203",
			wantOrg:    "org-202",
		},
		{
			name: "PROJECT",
			agent: Agent{
				Identifier:        "agent-project-304",
				AccountIdentifier: "account-301",
				OrgIdentifier:     "org-302",
				ProjectIdentifier: "project-303",
				Scope:             "PROJECT",
			},
			wantPathID:  "agent-project-304",
			wantOrg:     "org-302",
			wantProject: "project-303",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requestPath string
			var requestQuery url.Values
			session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requestPath = request.URL.Path
				requestQuery = request.URL.Query()
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{
					"identifier":%q,
					"name":"Observed Agent",
					"accountIdentifier":%q,
					"orgIdentifier":%q,
					"projectIdentifier":%q,
					"scope":%q,
					"type":"MANAGED_ARGO_PROVIDER",
					"operator":"ARGO",
					"tags":{"hga_cr_uid":"uid-901","other":"value"}
				}`,
					test.agent.Identifier,
					test.agent.AccountIdentifier,
					test.wantOrg,
					test.wantProject,
					test.agent.Scope,
				)
			}))

			result, err := (SDKAgentAPI{}).Lookup(
				context.Background(),
				session,
				test.agent,
			)
			if err != nil {
				t.Fatalf("lookup Agent: %v", err)
			}
			if !result.Exists {
				t.Fatal("lookup did not report the Agent as existing")
			}
			if requestPath != "/gitops/api/v1/agents/"+test.wantPathID {
				t.Fatalf("request path = %q, want scoped identifier path", requestPath)
			}
			if got := requestQuery.Get("accountIdentifier"); got != test.agent.AccountIdentifier {
				t.Fatalf("accountIdentifier = %q, want %q", got, test.agent.AccountIdentifier)
			}
			if got := requestQuery.Get("routingId"); got != test.agent.AccountIdentifier {
				t.Fatalf("routingId = %q, want %q", got, test.agent.AccountIdentifier)
			}
			if got := requestQuery.Get("scope"); got != test.agent.Scope {
				t.Fatalf("scope = %q, want %q", got, test.agent.Scope)
			}
			if got := requestQuery.Get("withCredentials"); got != "false" {
				t.Fatalf("withCredentials = %q, want false", got)
			}
			if got := requestQuery.Get("orgIdentifier"); got != test.wantOrg {
				t.Fatalf("orgIdentifier = %q, want %q", got, test.wantOrg)
			}
			if got := requestQuery.Get("projectIdentifier"); got != test.wantProject {
				t.Fatalf("projectIdentifier = %q, want %q", got, test.wantProject)
			}
			if result.Agent.Tags["hga_cr_uid"] != "uid-901" ||
				result.Agent.Operator != "ARGO" ||
				result.Agent.Type != "MANAGED_ARGO_PROVIDER" ||
				result.Agent.Scope != test.agent.Scope {
				t.Fatalf("unexpected observed Agent: %#v", result.Agent)
			}
		})
	}
}

func TestSDKAgentLookupFallsBackToRawOnlyAfterNotFound(t *testing.T) {
	var paths []string
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/gitops/api/v1/agents/org.agent-raw-351" {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{
			"identifier":"agent-raw-351",
			"name":"Agent",
			"accountIdentifier":"account-352",
			"orgIdentifier":"org-353",
			"scope":"ORG",
			"type":"MANAGED_ARGO_PROVIDER",
			"operator":"ARGO",
			"tags":{"hga_cr_uid":"uid-354"}
		}`))
	}))

	result, err := (SDKAgentAPI{}).Lookup(
		context.Background(),
		session,
		Agent{
			Identifier:        "agent-raw-351",
			AccountIdentifier: "account-352",
			OrgIdentifier:     "org-353",
			Scope:             "ORG",
		},
	)
	if err != nil {
		t.Fatalf("lookup with fallback: %v", err)
	}
	if !result.Exists {
		t.Fatal("raw fallback did not find Agent")
	}
	want := []string{
		"/gitops/api/v1/agents/org.agent-raw-351",
		"/gitops/api/v1/agents/agent-raw-351",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestSDKAgentLookupDoesNotFallbackAfterNonNotFound(t *testing.T) {
	var paths []string
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		http.Error(w, `{"message":"agent not found"}`, http.StatusForbidden)
	}))

	_, err := (SDKAgentAPI{}).Lookup(
		context.Background(),
		session,
		Agent{
			Identifier:        "agent-raw-361",
			AccountIdentifier: "account-362",
			Scope:             "ACCOUNT",
		},
	)
	if err == nil {
		t.Fatal("expected forbidden lookup to fail")
	}
	if got := harnessapi.VerdictOf(err); got != harnessapi.VerdictDenied {
		t.Fatalf("lookup verdict = %q, want %q", got, harnessapi.VerdictDenied)
	}
	want := []string{"/gitops/api/v1/agents/account.agent-raw-361"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestSDKAgentLookupHandlesHTTPStatus(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantExists bool
		wantErr    bool
	}{
		{
			name:   "not found",
			status: http.StatusNotFound,
		},
		{
			name:    "forbidden",
			status:  http.StatusForbidden,
			wantErr: true,
		},
		{
			name:    "server error",
			status:  http.StatusBadGateway,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := testStatusSession(t, test.status)
			result, err := (SDKAgentAPI{}).Lookup(
				context.Background(),
				session,
				Agent{
					Identifier:        "agent-raw-401",
					AccountIdentifier: "account-402",
					Scope:             "ACCOUNT",
				},
			)
			if (err != nil) != test.wantErr {
				t.Fatalf("lookup error = %v, wantErr %t", err, test.wantErr)
			}
			if result.Exists != test.wantExists {
				t.Fatalf("exists = %t, want %t", result.Exists, test.wantExists)
			}
		})
	}
}

func TestSDKAgentDeleteUsesScopedCandidates(t *testing.T) {
	tests := []struct {
		name     string
		agent    Agent
		wantPath string
	}{
		{
			name: "ACCOUNT",
			agent: Agent{
				Identifier:        "delete-agent-701",
				Name:              "Delete Agent",
				AccountIdentifier: "delete-account-702",
				Scope:             "ACCOUNT",
				Type:              "MANAGED_ARGO_PROVIDER",
			},
			wantPath: "/gitops/api/v1/agents/account.delete-agent-701",
		},
		{
			name: "ORG",
			agent: Agent{
				Identifier:        "delete-agent-711",
				Name:              "Delete Agent",
				AccountIdentifier: "delete-account-712",
				OrgIdentifier:     "delete-org-713",
				Scope:             "ORG",
				Type:              "MANAGED_ARGO_PROVIDER",
			},
			wantPath: "/gitops/api/v1/agents/org.delete-agent-711",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var paths []string
			session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				paths = append(paths, request.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))

			if err := (SDKAgentAPI{}).Delete(
				context.Background(),
				session,
				test.agent,
			); err != nil {
				t.Fatalf("delete Agent: %v", err)
			}
			if !reflect.DeepEqual(paths, []string{test.wantPath}) {
				t.Fatalf("paths = %#v, want %q", paths, test.wantPath)
			}
		})
	}
}

func TestSDKAgentDeleteFallsBackOnlyAfterNotFound(t *testing.T) {
	var paths []string
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/gitops/api/v1/agents/account.delete-agent-721" {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))

	if err := (SDKAgentAPI{}).Delete(
		context.Background(),
		session,
		Agent{
			Identifier:        "delete-agent-721",
			AccountIdentifier: "delete-account-722",
			Scope:             "ACCOUNT",
		},
	); err != nil {
		t.Fatalf("delete Agent with raw fallback: %v", err)
	}
	want := []string{
		"/gitops/api/v1/agents/account.delete-agent-721",
		"/gitops/api/v1/agents/delete-agent-721",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestSDKAgentDeleteStopsOnNonNotFound(t *testing.T) {
	var paths []string
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		http.Error(w, `{"message":"agent not found"}`, http.StatusForbidden)
	}))

	err := (SDKAgentAPI{}).Delete(
		context.Background(),
		session,
		Agent{
			Identifier:        "delete-agent-725",
			AccountIdentifier: "delete-account-726",
			Scope:             "ACCOUNT",
		},
	)
	if err == nil {
		t.Fatal("expected forbidden delete to fail")
	}
	if got := harnessapi.VerdictOf(err); got != harnessapi.VerdictDenied {
		t.Fatalf("delete verdict = %q, want %q", got, harnessapi.VerdictDenied)
	}
	want := []string{"/gitops/api/v1/agents/account.delete-agent-725"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestSDKAgentResolveTokenUsesCreateCredentialWithoutAnotherRequest(t *testing.T) {
	requests := 0
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		http.Error(w, `{"message":"Agent is not visible yet"}`, http.StatusNotFound)
	}))

	token, err := (SDKAgentAPI{}).ResolveToken(
		context.Background(),
		session,
		Agent{
			Identifier:        "new-agent-729",
			AccountIdentifier: "new-account-730",
			Scope:             "ACCOUNT",
		},
		"one-time-create-token",
	)
	if err != nil {
		t.Fatalf("resolve create credential: %v", err)
	}
	if token != "one-time-create-token" {
		t.Fatalf("token = %q, want the create response credential", token)
	}
	if requests != 0 {
		t.Fatalf("create credential triggered %d follow-up requests, want 0", requests)
	}
}

func TestSDKAgentResolveTokenRegeneratesWithSuccessfulCandidate(t *testing.T) {
	tests := []struct {
		name        string
		agent       Agent
		fallback    bool
		wantActions []string
	}{
		{
			name: "ACCOUNT canonical",
			agent: Agent{
				Identifier:        "token-agent-731",
				AccountIdentifier: "token-account-732",
				Scope:             "ACCOUNT",
			},
			wantActions: []string{
				"GET /gitops/api/v1/agents/account.token-agent-731",
				"POST /gitops/api/v1/agents/account.token-agent-731/credentials",
			},
		},
		{
			name: "ORG raw fallback",
			agent: Agent{
				Identifier:        "token-agent-741",
				AccountIdentifier: "token-account-742",
				OrgIdentifier:     "token-org-743",
				Scope:             "ORG",
			},
			fallback: true,
			wantActions: []string{
				"GET /gitops/api/v1/agents/org.token-agent-741",
				"GET /gitops/api/v1/agents/token-agent-741",
				"POST /gitops/api/v1/agents/token-agent-741/credentials",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var actions []string
			session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				action := request.Method + " " + request.URL.Path
				actions = append(actions, action)
				w.Header().Set("Content-Type", "application/json")
				if test.fallback &&
					action == "GET /gitops/api/v1/agents/org."+test.agent.Identifier {
					http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
					return
				}
				if request.Method == http.MethodPost {
					_, _ = w.Write([]byte(`{"credentials":{"privateKey":"regenerated-token"}}`))
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))

			token, err := (SDKAgentAPI{}).ResolveToken(
				context.Background(),
				session,
				test.agent,
				"",
			)
			if err != nil {
				t.Fatalf("resolve Agent token: %v", err)
			}
			if token != "regenerated-token" {
				t.Fatalf("token = %q, want regenerated-token", token)
			}
			if !reflect.DeepEqual(actions, test.wantActions) {
				t.Fatalf("actions = %#v, want %#v", actions, test.wantActions)
			}
		})
	}
}

func TestSDKAgentResolveTokenStopsOnNonNotFound(t *testing.T) {
	var paths []string
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
	}))

	_, err := (SDKAgentAPI{}).ResolveToken(
		context.Background(),
		session,
		Agent{
			Identifier:        "token-agent-751",
			AccountIdentifier: "token-account-752",
			Scope:             "ACCOUNT",
		},
		"",
	)
	if err == nil {
		t.Fatal("expected forbidden token lookup to fail")
	}
	want := []string{"/gitops/api/v1/agents/account.token-agent-751"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestSDKAgentReadinessStopsOnCanonicalNonNotFoundError(t *testing.T) {
	var paths []string
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		http.Error(w, `{"message":"service unavailable"}`, http.StatusServiceUnavailable)
	}))

	_, err := (SDKAgentAPI{}).Readiness(
		context.Background(),
		session,
		Agent{
			Identifier:        "ready-agent-761",
			AccountIdentifier: "ready-account-762",
			OrgIdentifier:     "ready-org-763",
			Scope:             "ORG",
		},
	)
	if err == nil {
		t.Fatal("expected readiness server error")
	}
	want := []string{"/gitops/api/v1/agents/org.ready-agent-761"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestSDKAgentReadinessUsesConnectedHealthyPayload(t *testing.T) {
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	readiness, err := (SDKAgentAPI{}).Readiness(
		context.Background(),
		session,
		Agent{
			Identifier:        "mapping-agent",
			AccountIdentifier: "account",
			OrgIdentifier:     "org",
			Scope:             "ORG",
		},
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
			agent:      testAgentHealth(nextgen.CONNECTED_V1ConnectedStatus, nextgen.UNHEALTHY_Servicev1HealthStatus),
			wantReady:  false,
			wantExists: true,
		},
		{
			name:       "healthy but disconnected",
			agent:      testAgentHealth(nextgen.DISCONNECTED_V1ConnectedStatus, nextgen.HEALTHY_Servicev1HealthStatus),
			wantReady:  false,
			wantExists: true,
		},
		{
			name:       "connected and healthy",
			agent:      testAgentHealth(nextgen.CONNECTED_V1ConnectedStatus, nextgen.HEALTHY_Servicev1HealthStatus),
			wantReady:  true,
			wantExists: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readiness := readinessFromAgent(tt.agent)
			if readiness.Exists != tt.wantExists || readiness.Ready != tt.wantReady {
				t.Fatalf("unexpected readiness: got %#v", readiness)
			}
			if readiness.Message == "" {
				t.Fatal("readiness message must explain the observed state")
			}
		})
	}
}

func testAgentHealth(
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

func TestSDKAgentCreateSendsStableTags(t *testing.T) {
	var requestAgent nextgen.V1Agent
	session := testSession(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&requestAgent); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"identifier":"agent-raw-501"}`))
	}))

	tags := map[string]string{
		"hga_cr_uid": "uid-502",
		"team":       "gitops",
	}
	if _, err := (SDKAgentAPI{}).Create(
		context.Background(),
		session,
		CreateAgentRequest{
			Agent: Agent{
				Identifier:        "agent-raw-501",
				Name:              "Tagged Agent",
				AccountIdentifier: "account-503",
				OrgIdentifier:     "org-must-be-omitted",
				ProjectIdentifier: "project-must-be-omitted",
				Scope:             "ACCOUNT",
				Type:              "MANAGED_ARGO_PROVIDER",
				Operator:          "ARGO",
				Tags:              tags,
			},
			Namespace: "agent-namespace",
		},
	); err != nil {
		t.Fatalf("create tagged Agent: %v", err)
	}
	if !reflect.DeepEqual(requestAgent.Tags, tags) {
		t.Fatalf("request tags = %#v, want %#v", requestAgent.Tags, tags)
	}
	if requestAgent.OrgIdentifier != "" || requestAgent.ProjectIdentifier != "" {
		t.Fatalf("ACCOUNT create leaked narrower scope: %#v", requestAgent)
	}
}

func TestSDKAgentCreateClassifiesAllAmbiguousOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		session     func(*testing.T) *harnessapi.Session
		wantUnknown bool
	}{
		{
			name: "transport error without response",
			session: func(t *testing.T) *harnessapi.Session {
				cfg := nextgen.NewConfiguration()
				cfg.HTTPClient.RetryMax = 0
				cfg.HTTPClient.HTTPClient.Transport = roundTripperFunc(
					func(*http.Request) (*http.Response, error) {
						return nil, errors.New("connection closed")
					},
				)
				return testSessionWithClient(t, nextgen.NewAPIClient(cfg))
			},
			wantUnknown: true,
		},
		{
			name:        "request timeout",
			session:     func(t *testing.T) *harnessapi.Session { return testStatusSession(t, http.StatusRequestTimeout) },
			wantUnknown: true,
		},
		{
			name:        "server error",
			session:     func(t *testing.T) *harnessapi.Session { return testStatusSession(t, http.StatusServiceUnavailable) },
			wantUnknown: true,
		},
		{
			name:        "rate limited",
			session:     func(t *testing.T) *harnessapi.Session { return testStatusSession(t, http.StatusTooManyRequests) },
			wantUnknown: true,
		},
		{
			name:    "definite client error",
			session: func(t *testing.T) *harnessapi.Session { return testStatusSession(t, http.StatusUnprocessableEntity) },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := (SDKAgentAPI{}).Create(
				context.Background(),
				test.session(t),
				CreateAgentRequest{
					Agent: Agent{
						Identifier:        "agent-raw-601",
						AccountIdentifier: "account-602",
						Scope:             "ACCOUNT",
						Type:              "MANAGED_ARGO_PROVIDER",
						Operator:          "ARGO",
					},
				},
			)
			if err == nil {
				t.Fatal("expected create error")
			}
			if got := errors.Is(err, ErrAgentCreateOutcomeUnknown); got != test.wantUnknown {
				t.Fatalf("outcomeUnknown = %t, want %t: %v", got, test.wantUnknown, err)
			}
		})
	}
}

func testSession(t *testing.T, handler http.Handler) *harnessapi.Session {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cfg := nextgen.NewConfiguration()
	cfg.BasePath = server.URL
	cfg.HTTPClient.RetryMax = 0
	return testSessionWithClient(t, nextgen.NewAPIClient(cfg))
}

func testStatusSession(t *testing.T, status int) *harnessapi.Session {
	t.Helper()
	return testSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(status), status)
	}))
}

func testSessionWithClient(
	t *testing.T,
	client *nextgen.APIClient,
) *harnessapi.Session {
	t.Helper()
	session, err := harnessapi.NewSessionWithClient("test-api-key", client)
	if err != nil {
		t.Fatalf("create Harness test session: %v", err)
	}
	return session
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
