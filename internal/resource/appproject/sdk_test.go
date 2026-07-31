package appproject

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

// TestSDKAppProjectGetUsesRoutingIdentifierAndConverts pins the wire contract
// of Get for an ACCOUNT-scoped agent: exactly one request, addressed with the
// scope-qualified routing identifier, no org/project query params, and the
// nested SDK response converted into this package's observed type.
func TestSDKAppProjectGetUsesRoutingIdentifierAndConverts(t *testing.T) {
	var paths []string
	var query url.Values
	session := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		// Shape taken from a live probe response (2026-07-31).
		_, _ = w.Write([]byte(`{
			"metadata": {"name":"default","namespace":"hub-account-agent","uid":"uid-1","resourceVersion":"42"},
			"spec": {
				"sourceRepos":["*"],
				"destinations":[{"server":"*","namespace":"*"}],
				"description":"Default AppProject",
				"clusterResourceWhitelist":[{"group":"*","kind":"*"}],
				"namespaceResourceWhitelist":[{"group":"*","kind":"*"}]
			},
			"status": {}
		}`))
	}))

	result, err := (SDKAppProjectAPI{}).Get(context.Background(), session, testAppProjectRequest())
	if err != nil {
		t.Fatalf("get AppProject: %v", err)
	}
	if !result.Exists {
		t.Fatal("expected the AppProject to exist")
	}

	wantPaths := []string{"/gitops/api/v1/agents/account.hub-agent/projects/default"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("unexpected request paths: got %#v, want %#v", paths, wantPaths)
	}
	if got := query.Get("accountIdentifier"); got != "account" {
		t.Fatalf("query accountIdentifier = %q, want %q", got, "account")
	}
	if got := query.Get("orgIdentifier"); got != "" {
		t.Fatalf("query orgIdentifier = %q, want empty for an ACCOUNT-scoped agent", got)
	}
	if got := query.Get("projectIdentifier"); got != "" {
		t.Fatalf("query projectIdentifier = %q, want empty for an ACCOUNT-scoped agent", got)
	}

	want := AppProject{
		Name:            "default",
		Namespace:       "hub-account-agent",
		UID:             "uid-1",
		ResourceVersion: "42",
		Description:     "Default AppProject",
		SourceRepos:     []string{"*"},
		Destinations: []Destination{
			{Server: "*", Namespace: "*"},
		},
		ClusterResourceWhitelist:   []GroupKind{{Group: "*", Kind: "*"}},
		NamespaceResourceWhitelist: []GroupKind{{Group: "*", Kind: "*"}},
	}
	if !reflect.DeepEqual(result.AppProject, want) {
		t.Fatalf("converted AppProject mismatch:\n got %#v\nwant %#v", result.AppProject, want)
	}
}

// TestSDKAppProjectGetReturnsAbsenceWithoutFallback is the INVERSE of the
// mapping boundary's candidate fallback: on the agent-proxied endpoints a
// genuine 404 means the AppProject is absent, so Get must report
// Exists=false with a nil error after EXACTLY ONE request. A second
// candidate request here would turn real absence into a fake transient
// failure (the unroutable form hangs instead of returning 404).
func TestSDKAppProjectGetReturnsAbsenceWithoutFallback(t *testing.T) {
	var paths []string
	session := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))

	result, err := (SDKAppProjectAPI{}).Get(context.Background(), session, testAppProjectRequest())
	if err != nil {
		t.Fatalf("absence must be a successful answer, got error: %v", err)
	}
	if result.Exists {
		t.Fatal("expected Exists=false on 404")
	}
	want := []string{"/gitops/api/v1/agents/account.hub-agent/projects/default"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("404 must not trigger candidate fallback: got %#v, want %#v", paths, want)
	}
}

// TestSDKAppProjectGetPreservesFailureVerdicts pins the error path: statuses
// that are not 404 must surface as errors carrying the classified Verdict —
// never as absence. 400 is the live behavior of a never-connected agent
// (probe-verified), and treating it as absence would make the reconciler
// try to create through a dead agent.
func TestSDKAppProjectGetPreservesFailureVerdicts(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantVerdict harnessapi.Verdict
	}{
		{name: "denied", status: http.StatusForbidden, wantVerdict: harnessapi.VerdictDenied},
		{name: "server error", status: http.StatusBadGateway, wantVerdict: harnessapi.VerdictTransient},
		{name: "agent not connected", status: http.StatusBadRequest, wantVerdict: harnessapi.VerdictFailed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, http.StatusText(test.status), test.status)
			}))

			_, err := (SDKAppProjectAPI{}).Get(context.Background(), session, testAppProjectRequest())
			if err == nil {
				t.Fatalf("expected status %d to fail", test.status)
			}
			if got := harnessapi.VerdictOf(err); got != test.wantVerdict {
				t.Fatalf("verdict = %q, want %q", got, test.wantVerdict)
			}
		})
	}
}

// TestSDKAppProjectListConvertsAllItems pins the unfiltered list: one request
// on the routing identifier, NO query.name filter, every item converted.
func TestSDKAppProjectListConvertsAllItems(t *testing.T) {
	var paths []string
	var query url.Values
	session := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"items": [
				{"metadata":{"name":"default"},"spec":{"sourceRepos":["*"]}},
				{"metadata":{"name":"payments"},"spec":{"description":"payments team"}}
			]
		}`))
	}))

	request := testAppProjectRequest()
	request.Name = "" // list everything — the reconciler's sweep case

	projects, err := (SDKAppProjectAPI{}).List(context.Background(), session, request)
	if err != nil {
		t.Fatalf("list AppProjects: %v", err)
	}
	wantPaths := []string{"/gitops/api/v1/agents/account.hub-agent/projects"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("unexpected request paths: got %#v, want %#v", paths, wantPaths)
	}
	if query.Has("query.name") {
		t.Fatalf("unfiltered list must omit query.name, got %q", query.Get("query.name"))
	}
	if len(projects) != 2 || projects[0].Name != "default" || projects[1].Name != "payments" {
		t.Fatalf("unexpected converted projects: %#v", projects)
	}
	if projects[1].Description != "payments team" {
		t.Fatalf("item spec was not converted: %#v", projects[1])
	}
}

// TestSDKAppProjectListSendsNameFilter pins the filtered form: request.Name
// becomes the query.name parameter.
func TestSDKAppProjectListSendsNameFilter(t *testing.T) {
	var query url.Values
	session := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))

	projects, err := (SDKAppProjectAPI{}).List(context.Background(), session, testAppProjectRequest())
	if err != nil {
		t.Fatalf("list AppProjects: %v", err)
	}
	if got := query.Get("query.name"); got != "default" {
		t.Fatalf("query.name = %q, want %q", got, "default")
	}
	if len(projects) != 0 {
		t.Fatalf("expected an empty result, got %#v", projects)
	}
}

// TestSDKAppProjectListDoesNotTreatNotFoundAsEmpty is List's half of the
// Get/List asymmetry: no object name rides in the list path, so a 404 can
// only mean a routing or agent problem — it must surface as an error with
// its verdict intact, NEVER as "zero projects". Mapping it to an empty
// slice would let a routing failure masquerade as absence and push the
// reconciler toward creating things that already exist.
func TestSDKAppProjectListDoesNotTreatNotFoundAsEmpty(t *testing.T) {
	var paths []string
	session := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))

	projects, err := (SDKAppProjectAPI{}).List(context.Background(), session, testAppProjectRequest())
	if err == nil {
		t.Fatalf("expected a 404 list to fail, got %#v", projects)
	}
	if got := harnessapi.VerdictOf(err); got != harnessapi.VerdictAbsent {
		t.Fatalf("verdict = %q, want %q preserved inside the error", got, harnessapi.VerdictAbsent)
	}
	if len(paths) != 1 {
		t.Fatalf("404 must not trigger candidate fallback: got %#v", paths)
	}
}

// TestSDKAppProjectCreateSendsDesiredSpecNeverUpsert pins the outbound body:
// upsert must be false (blind upsert would clobber human-made projects — the
// UI and Terraform write through this same API), the name comes from
// request.Name and NOT from the desired struct, and observation-only fields
// (UID/ResourceVersion/Namespace) never leave the process.
func TestSDKAppProjectCreateSendsDesiredSpecNeverUpsert(t *testing.T) {
	var paths []string
	var body nextgen.ProjectsProjectCreateRequest
	session := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"name":"default","uid":"uid-new"},"spec":{"sourceRepos":["*"]}}`))
	}))

	desired := AppProject{
		// Deliberately hostile inputs: a wrong name and observation fields
		// that must all be ignored by the builder.
		Name:            "wrong-name-must-be-ignored",
		Namespace:       "must-not-be-sent",
		UID:             "must-not-be-sent",
		ResourceVersion: "must-not-be-sent",
		Description:     "made by the controller",
		SourceRepos:     []string{"*"},
		Destinations:    []Destination{{Server: "*", Namespace: "*"}},
	}
	created, err := (SDKAppProjectAPI{}).Create(context.Background(), session, testAppProjectRequest(), desired)
	if err != nil {
		t.Fatalf("create AppProject: %v", err)
	}
	if created.UID != "uid-new" {
		t.Fatalf("create response was not converted: %#v", created)
	}
	wantPaths := []string{"/gitops/api/v1/agents/account.hub-agent/projects"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("unexpected request paths: got %#v, want %#v", paths, wantPaths)
	}
	if body.Upsert {
		t.Fatal("create body set upsert=true; it must always be false")
	}
	if body.Project == nil || body.Project.Metadata == nil {
		t.Fatalf("create body missing project metadata: %#v", body)
	}
	if got := body.Project.Metadata.Name; got != "default" {
		t.Fatalf("body metadata.name = %q, want request.Name %q", got, "default")
	}
	if body.Project.Metadata.Namespace != "" || body.Project.Metadata.Uid != "" || body.Project.Metadata.ResourceVersion != "" {
		t.Fatalf("observation-only metadata leaked into the create body: %#v", body.Project.Metadata)
	}
	if body.Project.Spec == nil || body.Project.Spec.Description != "made by the controller" {
		t.Fatalf("desired spec was not sent: %#v", body.Project.Spec)
	}
}

// TestSDKAppProjectCreateMapsConflictToAlreadyExists pins the one sentinel:
// a 409 means the reconciler lost a create race and must re-read, not fail.
// Ambiguous outcomes (5xx) stay plain errors with their verdict — there is
// no CreateOutcomeUnknown here because AppProject identity is the
// client-chosen name and a Get-by-name after ambiguity is deterministic.
func TestSDKAppProjectCreateMapsConflictToAlreadyExists(t *testing.T) {
	conflict := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"project already exists"}`, http.StatusConflict)
	}))
	_, err := (SDKAppProjectAPI{}).Create(context.Background(), conflict, testAppProjectRequest(), AppProject{})
	if !errors.Is(err, ErrAppProjectAlreadyExists) {
		t.Fatalf("409 must map to ErrAppProjectAlreadyExists, got: %v", err)
	}

	transient := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"bad gateway"}`, http.StatusBadGateway)
	}))
	_, err = (SDKAppProjectAPI{}).Create(context.Background(), transient, testAppProjectRequest(), AppProject{})
	if err == nil || errors.Is(err, ErrAppProjectAlreadyExists) {
		t.Fatalf("5xx must stay a plain error, got: %v", err)
	}
	if got := harnessapi.VerdictOf(err); got != harnessapi.VerdictTransient {
		t.Fatalf("verdict = %q, want %q", got, harnessapi.VerdictTransient)
	}
}

// TestSDKAppProjectUpdatePathMatchesBodyName pins the endpoint's contract:
// the PUT path parameter must equal body.project.metadata.name. Both come
// from request.Name, so this holds by construction — this test keeps it so.
func TestSDKAppProjectUpdatePathMatchesBodyName(t *testing.T) {
	var path string
	var body nextgen.ProjectsProjectUpdateRequest
	session := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode update body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"name":"default"}}`))
	}))

	if _, err := (SDKAppProjectAPI{}).Update(context.Background(), session, testAppProjectRequest(), AppProject{Description: "v2"}); err != nil {
		t.Fatalf("update AppProject: %v", err)
	}
	if want := "/gitops/api/v1/agents/account.hub-agent/projects/default"; path != want {
		t.Fatalf("update path = %q, want %q", path, want)
	}
	if body.Project == nil || body.Project.Metadata == nil || body.Project.Metadata.Name != "default" {
		t.Fatalf("body name must equal the path name: %#v", body.Project)
	}
}

// TestSDKAppProjectDeleteTreatsAbsentAsSuccess pins finalizer semantics: a
// re-run after half-completed cleanup must converge, so 404 is a completed
// delete. Anything else (here: denied) stays an error with its verdict.
func TestSDKAppProjectDeleteTreatsAbsentAsSuccess(t *testing.T) {
	var path string
	var query url.Values
	ok := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	if err := (SDKAppProjectAPI{}).Delete(context.Background(), ok, testAppProjectRequest()); err != nil {
		t.Fatalf("delete AppProject: %v", err)
	}
	if want := "/gitops/api/v1/agents/account.hub-agent/projects/default"; path != want {
		t.Fatalf("delete path = %q, want %q", path, want)
	}
	if !query.Has("orgIdentifier") {
		t.Fatal("delete must send orgIdentifier positionally (even empty at ACCOUNT scope)")
	}

	gone := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))
	if err := (SDKAppProjectAPI{}).Delete(context.Background(), gone, testAppProjectRequest()); err != nil {
		t.Fatalf("absent project must be a completed delete, got: %v", err)
	}

	denied := newAppProjectTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
	}))
	err := (SDKAppProjectAPI{}).Delete(context.Background(), denied, testAppProjectRequest())
	if err == nil {
		t.Fatal("denied delete must fail")
	}
	if got := harnessapi.VerdictOf(err); got != harnessapi.VerdictDenied {
		t.Fatalf("verdict = %q, want %q", got, harnessapi.VerdictDenied)
	}
}

func newAppProjectTestSession(t *testing.T, handler http.Handler) *harnessapi.Session {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cfg := nextgen.NewConfiguration()
	cfg.BasePath = server.URL
	cfg.HTTPClient.RetryMax = 0
	client := nextgen.NewAPIClient(cfg)

	session, err := harnessapi.NewSessionWithClient("test-api-key", client)
	if err != nil {
		t.Fatalf("create Harness session: %v", err)
	}
	return session
}

func testAppProjectRequest() AppProjectRequest {
	return AppProjectRequest{
		AccountIdentifier: "account",
		AgentIdentifier:   "hub-agent",
		AgentScope:        "ACCOUNT",
		Name:              "default",
	}
}
