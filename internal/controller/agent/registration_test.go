package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

type fakeAgentRegistrationAPI struct {
	lookupResult  harnessapi.AgentLookupResult
	lookupErr     error
	createResult  harnessapi.CreateAgentResult
	createErr     error
	deleteErr     error
	resolveErr    error
	resolvedToken string
	commitCreate  bool

	lookupCalls  int
	createCalls  int
	deleteCalls  int
	resolveCalls int
	lastLookup   harnessapi.Agent
	lastCreate   harnessapi.CreateAgentRequest
	lastDelete   harnessapi.Agent
}

func (f *fakeAgentRegistrationAPI) Lookup(
	_ context.Context,
	_ *harnessapi.Session,
	agent harnessapi.Agent,
) (harnessapi.AgentLookupResult, error) {
	f.lookupCalls++
	f.lastLookup = agent
	return f.lookupResult, f.lookupErr
}

func (f *fakeAgentRegistrationAPI) Create(
	_ context.Context,
	_ *harnessapi.Session,
	request harnessapi.CreateAgentRequest,
) (harnessapi.CreateAgentResult, error) {
	f.createCalls++
	f.lastCreate = request
	if f.commitCreate {
		created := request.Agent
		if f.createResult.Identifier != "" {
			created.Identifier = f.createResult.Identifier
		}
		f.lookupResult = harnessapi.AgentLookupResult{
			Exists: true,
			Agent:  created,
		}
	}
	return f.createResult, f.createErr
}

func (f *fakeAgentRegistrationAPI) Delete(
	_ context.Context,
	_ *harnessapi.Session,
	agent harnessapi.Agent,
) error {
	f.deleteCalls++
	f.lastDelete = agent
	return f.deleteErr
}

func (f *fakeAgentRegistrationAPI) ResolveToken(
	_ context.Context,
	_ *harnessapi.Session,
	_ harnessapi.Agent,
	_ string,
) (string, error) {
	f.resolveCalls++
	if f.resolvedToken == "" {
		f.resolvedToken = "recovered-agent-token"
	}
	return f.resolvedToken, f.resolveErr
}

type statusFailureClient struct {
	client.Client
	failUpdate  map[int]error
	updateCalls int
}

func (c *statusFailureClient) Status() client.SubResourceWriter {
	return &statusFailureWriter{
		parent:   c,
		delegate: c.Client.Status(),
	}
}

type statusFailureWriter struct {
	parent   *statusFailureClient
	delegate client.SubResourceWriter
}

func (w *statusFailureWriter) Create(
	ctx context.Context,
	obj client.Object,
	subResource client.Object,
	opts ...client.SubResourceCreateOption,
) error {
	return w.delegate.Create(ctx, obj, subResource, opts...)
}

func (w *statusFailureWriter) Update(
	ctx context.Context,
	obj client.Object,
	opts ...client.SubResourceUpdateOption,
) error {
	w.parent.updateCalls++
	if err := w.parent.failUpdate[w.parent.updateCalls]; err != nil {
		delete(w.parent.failUpdate, w.parent.updateCalls)
		return err
	}
	return w.delegate.Update(ctx, obj, opts...)
}

func (w *statusFailureWriter) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.SubResourcePatchOption,
) error {
	return w.delegate.Patch(ctx, obj, patch, opts...)
}

func TestAgentRegistrationDoesNotCreateBeforeIntentPersists(t *testing.T) {
	fixture := newAgentRegistrationFixture(t, "PROJECT", map[int]error{
		1: errors.New("status API unavailable"),
	})

	_, err := fixture.reconciler.reconcileReady(
		context.Background(),
		ctrlRequestFor(fixture.agent),
		fixture.agent,
		"",
		false,
	)
	if err == nil {
		t.Fatal("expected the Pending status write to fail")
	}
	if fixture.agentAPI.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", fixture.agentAPI.createCalls)
	}
	if fixture.agentAPI.lookupCalls != 0 {
		t.Fatalf("lookup calls = %d, want 0", fixture.agentAPI.lookupCalls)
	}
}

func TestAgentRegistrationRecoversCommittedCreateAfterStatusFailure(t *testing.T) {
	fixture := newAgentRegistrationFixture(t, "PROJECT", map[int]error{
		2: errors.New("managed status write failed"),
	})
	fixture.agentAPI.commitCreate = true
	fixture.agentAPI.createResult = harnessapi.CreateAgentResult{
		Identifier:   fixture.agent.Spec.Identifier,
		InitialToken: "one-time-token",
	}

	if _, err := fixture.reconciler.reconcileReady(
		context.Background(),
		ctrlRequestFor(fixture.agent),
		fixture.agent,
		"",
		false,
	); err == nil {
		t.Fatal("expected the post-create status write to fail")
	}
	if fixture.agentAPI.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", fixture.agentAPI.createCalls)
	}

	persisted := fixture.getAgent(t)
	if persisted.Status.CreationState != infrastructurev1.AgentCreationPending {
		t.Fatalf("creation state = %q, want Pending", persisted.Status.CreationState)
	}
	if persisted.Status.AgentOwnership != "" {
		t.Fatalf("ownership = %q, want empty", persisted.Status.AgentOwnership)
	}

	result, err := fixture.reconciler.reconcileReady(
		context.Background(),
		ctrlRequestFor(persisted),
		persisted,
		"",
		false,
	)
	if err != nil {
		t.Fatalf("recover committed create: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("recovery result = %#v, want zero", result)
	}
	if fixture.agentAPI.createCalls != 1 {
		t.Fatalf("recovery issued another create: %d calls", fixture.agentAPI.createCalls)
	}
	if fixture.agentAPI.resolveCalls != 1 {
		t.Fatalf("token resolve calls = %d, want 1", fixture.agentAPI.resolveCalls)
	}
	fixture.assertManagedWithToken(t)
}

func TestAgentRegistrationRecoversTimeoutByUIDTag(t *testing.T) {
	fixture := newAgentRegistrationFixture(t, "ORG", nil)
	fixture.agentAPI.commitCreate = true
	fixture.agentAPI.createErr = fmt.Errorf(
		"%w: connection closed",
		harnessapi.ErrAgentCreateOutcomeUnknown,
	)

	result, err := fixture.reconciler.reconcileReady(
		context.Background(),
		ctrlRequestFor(fixture.agent),
		fixture.agent,
		"",
		false,
	)
	if err != nil {
		t.Fatalf("ambiguous create: %v", err)
	}
	if result.RequeueAfter != agentRegistrationRetryInterval {
		t.Fatalf("requeueAfter = %s, want %s", result.RequeueAfter, agentRegistrationRetryInterval)
	}

	persisted := fixture.getAgent(t)
	if persisted.Status.CreationState != infrastructurev1.AgentCreationOutcomeUnknown {
		t.Fatalf("creation state = %q, want OutcomeUnknown", persisted.Status.CreationState)
	}

	if _, err := fixture.reconciler.reconcileReady(
		context.Background(),
		ctrlRequestFor(persisted),
		persisted,
		"",
		false,
	); err != nil {
		t.Fatalf("recover ambiguous create: %v", err)
	}
	if fixture.agentAPI.createCalls != 1 {
		t.Fatalf("ambiguous recovery issued another create: %d calls", fixture.agentAPI.createCalls)
	}
	fixture.assertManagedWithToken(t)
}

func TestAgentRegistrationRecoversCreateConflictOnNextLookup(t *testing.T) {
	fixture := newAgentRegistrationFixture(t, "ACCOUNT", nil)
	fixture.agentAPI.createErr = harnessapi.ErrAgentAlreadyExists

	result, err := fixture.reconciler.reconcileReady(
		context.Background(),
		ctrlRequestFor(fixture.agent),
		fixture.agent,
		"",
		false,
	)
	if err != nil {
		t.Fatalf("create conflict should enter recovery: %v", err)
	}
	if result.RequeueAfter != agentRegistrationRetryInterval {
		t.Fatalf("requeueAfter = %s, want %s", result.RequeueAfter, agentRegistrationRetryInterval)
	}
	persisted := fixture.getAgent(t)
	if persisted.Status.CreationState != infrastructurev1.AgentCreationOutcomeUnknown {
		t.Fatalf("creation state = %q, want OutcomeUnknown", persisted.Status.CreationState)
	}

	fixture.agentAPI.createErr = nil
	fixture.agentAPI.lookupResult = harnessapi.AgentLookupResult{
		Exists: true,
		Agent:  ownedAgentObservation(persisted),
	}
	if _, err := fixture.reconciler.reconcileReady(
		context.Background(),
		ctrlRequestFor(persisted),
		persisted,
		"",
		false,
	); err != nil {
		t.Fatalf("recover create conflict: %v", err)
	}
	if fixture.agentAPI.createCalls != 1 {
		t.Fatalf("conflict recovery issued another create: %d calls", fixture.agentAPI.createCalls)
	}
	fixture.assertManagedWithToken(t)
}

func TestAgentRegistrationAcceptsScopedObservedIdentifier(t *testing.T) {
	tests := []struct {
		scope    string
		prefixed string
	}{
		{scope: "ACCOUNT", prefixed: "account.agent-identifier-711"},
		{scope: "ORG", prefixed: "org.agent-identifier-711"},
	}

	for _, test := range tests {
		t.Run(test.scope, func(t *testing.T) {
			fixture := newAgentRegistrationFixture(t, test.scope, nil)
			observed := ownedAgentObservation(fixture.agent)
			observed.Identifier = test.prefixed
			fixture.agentAPI.lookupResult = harnessapi.AgentLookupResult{
				Exists: true,
				Agent:  observed,
			}

			if _, err := fixture.reconciler.reconcileReady(
				context.Background(),
				ctrlRequestFor(fixture.agent),
				fixture.agent,
				"",
				false,
			); err != nil {
				t.Fatalf("recover scoped identifier: %v", err)
			}
			if fixture.agentAPI.createCalls != 0 {
				t.Fatalf("create calls = %d, want 0", fixture.agentAPI.createCalls)
			}
			fixture.assertManagedWithToken(t)
		})
	}
}

func TestAgentRegistrationDefiniteCreateFailureClearsIntent(t *testing.T) {
	fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
	fixture.agentAPI.createErr = errors.New("HTTP 422: invalid Agent request")

	if _, err := fixture.reconciler.reconcileReady(
		context.Background(),
		ctrlRequestFor(fixture.agent),
		fixture.agent,
		"",
		false,
	); err == nil {
		t.Fatal("expected definite create failure")
	}
	persisted := fixture.getAgent(t)
	if persisted.Status.CreationState != "" {
		t.Fatalf("creation state = %q, want cleared", persisted.Status.CreationState)
	}
	if fixture.agentAPI.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", fixture.agentAPI.createCalls)
	}
	if fixture.agentAPI.resolveCalls != 0 {
		t.Fatalf("resolve calls = %d, want 0", fixture.agentAPI.resolveCalls)
	}
}

func TestAgentRegistrationRejectsEmptyKubernetesUID(t *testing.T) {
	fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
	fixture.agent.UID = ""

	if _, err := fixture.reconciler.reconcileReady(
		context.Background(),
		ctrlRequestFor(fixture.agent),
		fixture.agent,
		"",
		false,
	); err == nil {
		t.Fatal("expected empty Kubernetes UID to fail")
	}
	if fixture.agentAPI.lookupCalls != 0 || fixture.agentAPI.createCalls != 0 {
		t.Fatalf(
			"empty UID contacted Harness: lookup=%d create=%d",
			fixture.agentAPI.lookupCalls,
			fixture.agentAPI.createCalls,
		)
	}
}

func TestAgentRegistrationRejectsWrongOwnershipProof(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*harnessapi.Agent)
	}{
		{
			name: "missing UID tag",
			mutate: func(agent *harnessapi.Agent) {
				delete(agent.Tags, harnessAgentCRUIDTag)
			},
		},
		{
			name: "wrong UID tag",
			mutate: func(agent *harnessapi.Agent) {
				agent.Tags[harnessAgentCRUIDTag] = "another-cr-uid"
			},
		},
		{name: "wrong name", mutate: func(agent *harnessapi.Agent) { agent.Name = "other-name" }},
		{name: "wrong account", mutate: func(agent *harnessapi.Agent) { agent.AccountIdentifier = "other-account" }},
		{name: "wrong org", mutate: func(agent *harnessapi.Agent) { agent.OrgIdentifier = "other-org" }},
		{name: "wrong project", mutate: func(agent *harnessapi.Agent) { agent.ProjectIdentifier = "other-project" }},
		{name: "wrong scope", mutate: func(agent *harnessapi.Agent) { agent.Scope = "ORG" }},
		{name: "wrong type", mutate: func(agent *harnessapi.Agent) { agent.Type = "CONNECTED_ARGO_PROVIDER" }},
		{name: "wrong operator", mutate: func(agent *harnessapi.Agent) { agent.Operator = "FLAMINGO" }},
	}

	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
			observed := ownedAgentObservation(fixture.agent)
			test.mutate(&observed)
			fixture.agentAPI.lookupResult = harnessapi.AgentLookupResult{
				Exists: true,
				Agent:  observed,
			}

			_, err := fixture.reconciler.reconcileReady(
				context.Background(),
				ctrlRequestFor(fixture.agent),
				fixture.agent,
				"",
				false,
			)
			if !errors.Is(err, errHarnessAgentAlreadyExists) {
				t.Fatalf("expected ownership conflict, got %v", err)
			}
			if err == nil || !containsReplacementCRGuidance(err.Error()) {
				t.Fatalf("conflict lacks replacement-CR guidance: %v", err)
			}
			if fixture.agentAPI.createCalls != 0 {
				t.Fatalf("create calls = %d, want 0", fixture.agentAPI.createCalls)
			}
			if fixture.agentAPI.resolveCalls != 0 {
				t.Fatalf("resolve calls = %d, want 0", fixture.agentAPI.resolveCalls)
			}
			persisted := fixture.getAgent(t)
			if persisted.Status.CreationState != infrastructurev1.AgentCreationOutcomeUnknown {
				t.Fatalf("creation state = %q, want OutcomeUnknown", persisted.Status.CreationState)
			}
		})
	}
}

func TestUncertainAgentDeletionUsesUIDTaggedLookup(t *testing.T) {
	tests := []struct {
		name          string
		lookup        func(*infrastructurev1.HarnessGitopsAgent) harnessapi.AgentLookupResult
		lookupErr     error
		wantDelete    int
		wantErr       bool
		wantFinalizer bool
	}{
		{
			name: "matching tagged Agent is deleted",
			lookup: func(agent *infrastructurev1.HarnessGitopsAgent) harnessapi.AgentLookupResult {
				return harnessapi.AgentLookupResult{Exists: true, Agent: ownedAgentObservation(agent)}
			},
			wantDelete: 1,
		},
		{
			name: "absent Agent is not deleted",
			lookup: func(*infrastructurev1.HarnessGitopsAgent) harnessapi.AgentLookupResult {
				return harnessapi.AgentLookupResult{}
			},
		},
		{
			name: "wrong tag is not deleted",
			lookup: func(agent *infrastructurev1.HarnessGitopsAgent) harnessapi.AgentLookupResult {
				observed := ownedAgentObservation(agent)
				observed.Tags[harnessAgentCRUIDTag] = "different-uid"
				return harnessapi.AgentLookupResult{Exists: true, Agent: observed}
			},
		},
		{
			name: "lookup failure retains finalizer",
			lookup: func(*infrastructurev1.HarnessGitopsAgent) harnessapi.AgentLookupResult {
				return harnessapi.AgentLookupResult{}
			},
			lookupErr:     errors.New("temporary Harness failure"),
			wantErr:       true,
			wantFinalizer: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAgentRegistrationFixture(t, "ACCOUNT", nil)
			fixture.agent.Status.CreationState = infrastructurev1.AgentCreationOutcomeUnknown
			if err := fixture.reconciler.Status().Update(context.Background(), fixture.agent); err != nil {
				t.Fatalf("persist uncertain status: %v", err)
			}
			fixture.agentAPI.lookupResult = test.lookup(fixture.agent)
			fixture.agentAPI.lookupErr = test.lookupErr

			_, err := fixture.reconciler.reconcileDeletion(
				context.Background(),
				fixture.agent,
				"",
				false,
			)
			if (err != nil) != test.wantErr {
				t.Fatalf("delete error = %v, wantErr %t", err, test.wantErr)
			}
			if fixture.agentAPI.deleteCalls != test.wantDelete {
				t.Fatalf("delete calls = %d, want %d", fixture.agentAPI.deleteCalls, test.wantDelete)
			}

			persisted := fixture.getAgent(t)
			hasFinalizer := false
			for _, finalizer := range persisted.Finalizers {
				hasFinalizer = hasFinalizer || finalizer == harnessAgentFinalizer
			}
			if hasFinalizer != test.wantFinalizer {
				t.Fatalf("has finalizer = %t, want %t", hasFinalizer, test.wantFinalizer)
			}
		})
	}
}

func TestManagedAgentDeletionReverifiesUIDTaggedTuple(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(*harnessapi.Agent)
		lookupErr     error
		wantDelete    int
		wantErr       bool
		wantFinalizer bool
	}{
		{
			name:       "matching managed Agent is deleted",
			wantDelete: 1,
		},
		{
			name: "replacement without the CR tag is preserved",
			mutate: func(agent *harnessapi.Agent) {
				delete(agent.Tags, harnessAgentCRUIDTag)
			},
		},
		{
			name:          "lookup failure retains finalizer",
			lookupErr:     errors.New("temporary Harness lookup failure"),
			wantErr:       true,
			wantFinalizer: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
			fixture.agent.Status.AgentIdentifier = fixture.agent.Spec.Identifier
			fixture.agent.Status.AgentOwnership = infrastructurev1.OwnershipManaged
			if err := fixture.reconciler.Status().Update(
				context.Background(),
				fixture.agent,
			); err != nil {
				t.Fatalf("persist managed status: %v", err)
			}

			observed := ownedAgentObservation(fixture.agent)
			if test.mutate != nil {
				test.mutate(&observed)
			}
			fixture.agentAPI.lookupResult = harnessapi.AgentLookupResult{
				Exists: true,
				Agent:  observed,
			}
			fixture.agentAPI.lookupErr = test.lookupErr

			_, err := fixture.reconciler.reconcileDeletion(
				context.Background(),
				fixture.agent,
				"",
				false,
			)
			if (err != nil) != test.wantErr {
				t.Fatalf("delete error = %v, wantErr %t", err, test.wantErr)
			}
			if fixture.agentAPI.lookupCalls != 1 {
				t.Fatalf("lookup calls = %d, want 1", fixture.agentAPI.lookupCalls)
			}
			if fixture.agentAPI.deleteCalls != test.wantDelete {
				t.Fatalf("delete calls = %d, want %d", fixture.agentAPI.deleteCalls, test.wantDelete)
			}

			persisted := fixture.getAgent(t)
			hasFinalizer := false
			for _, finalizer := range persisted.Finalizers {
				hasFinalizer = hasFinalizer || finalizer == harnessAgentFinalizer
			}
			if hasFinalizer != test.wantFinalizer {
				t.Fatalf("has finalizer = %t, want %t", hasFinalizer, test.wantFinalizer)
			}
		})
	}
}

func TestAgentRegistrationScopeTuplesUseDistinctIdentifiers(t *testing.T) {
	tests := []struct {
		scope     string
		accountID string
		orgID     string
		projectID string
		agentID   string
	}{
		{
			scope:     "ACCOUNT",
			accountID: "account-11",
			agentID:   "agent-12",
		},
		{
			scope:     "ORG",
			accountID: "account-21",
			orgID:     "org-22",
			agentID:   "agent-23",
		},
		{
			scope:     "PROJECT",
			accountID: "account-31",
			orgID:     "org-32",
			projectID: "project-33",
			agentID:   "agent-34",
		},
	}

	for _, test := range tests {
		t.Run(test.scope, func(t *testing.T) {
			fixture := newAgentRegistrationFixture(t, test.scope, nil)
			fixture.agent.Spec.AccountId = test.accountID
			fixture.agent.Spec.OrgId = test.orgID
			fixture.agent.Spec.ProjectId = test.projectID
			fixture.agent.Spec.Identifier = test.agentID
			if err := fixture.reconciler.Update(context.Background(), fixture.agent); err != nil {
				t.Fatalf("persist %s Agent tuple: %v", test.scope, err)
			}
			fixture.agent = fixture.getAgent(t)
			fixture.agentAPI.createResult.Identifier = test.agentID

			if _, err := fixture.reconciler.reconcileAgentRegistration(
				context.Background(),
				nil,
				fixture.agent,
				fixture.agent.Namespace,
			); err != nil {
				t.Fatalf("register %s Agent: %v", test.scope, err)
			}

			request := fixture.agentAPI.lastCreate.Agent
			if request.Identifier != test.agentID || request.AccountIdentifier != test.accountID {
				t.Fatalf("unexpected base tuple: %#v", request)
			}
			if request.OrgIdentifier != test.orgID || request.ProjectIdentifier != test.projectID {
				t.Fatalf("unexpected scoped tuple: %#v", request)
			}
			if request.Tags[harnessAgentCRUIDTag] != string(fixture.agent.UID) {
				t.Fatalf("UID tag = %q, want %q", request.Tags[harnessAgentCRUIDTag], fixture.agent.UID)
			}
		})
	}
}

type agentRegistrationFixture struct {
	reconciler *Reconciler
	client     *statusFailureClient
	agentAPI   *fakeAgentRegistrationAPI
	agent      *infrastructurev1.HarnessGitopsAgent
}

func newAgentRegistrationFixture(
	t *testing.T,
	scope string,
	statusFailures map[int]error,
) *agentRegistrationFixture {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add API scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	agent := registrationTestAgent(scope)
	apiKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agent.Spec.ApiKeySecretRef,
			Namespace: agent.Namespace,
		},
		Data: map[string][]byte{"api_key": []byte("test-api-key")},
	}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrastructurev1.HarnessGitopsAgent{}).
		WithObjects(agent, apiKey).
		Build()
	statusClient := &statusFailureClient{
		Client:     baseClient,
		failUpdate: statusFailures,
	}
	agentAPI := &fakeAgentRegistrationAPI{
		createResult: harnessapi.CreateAgentResult{Identifier: agent.Spec.Identifier},
	}

	persisted := &infrastructurev1.HarnessGitopsAgent{}
	if err := statusClient.Get(
		context.Background(),
		client.ObjectKeyFromObject(agent),
		persisted,
	); err != nil {
		t.Fatalf("get Agent fixture: %v", err)
	}

	return &agentRegistrationFixture{
		reconciler: &Reconciler{
			Client:    statusClient,
			APIReader: statusClient,
			Scheme:    scheme,
			agentAPI:  agentAPI,
		},
		client:   statusClient,
		agentAPI: agentAPI,
		agent:    persisted,
	}
}

func registrationTestAgent(scope string) *infrastructurev1.HarnessGitopsAgent {
	agent := &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "registration-agent",
			Namespace:  "agent-namespace",
			UID:        types.UID("cr-uid-741"),
			Finalizers: []string{harnessAgentFinalizer},
		},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{
			Name:            "Registration Agent",
			Identifier:      "agent-identifier-711",
			Operator:        "ARGO",
			AccountId:       "account-712",
			Scope:           scope,
			Type:            "MANAGED_ARGO_PROVIDER",
			ApiKeySecretRef: "harness-api-key",
			TokenSecretRef:  "registration-agent-token",
		},
	}
	if scope == "ORG" || scope == "PROJECT" {
		agent.Spec.OrgId = "org-713"
	}
	if scope == "PROJECT" {
		agent.Spec.ProjectId = "project-714"
	}
	return agent
}

func ownedAgentObservation(
	agent *infrastructurev1.HarnessGitopsAgent,
) harnessapi.Agent {
	observed := harnessAgentFor(agent, agent.Spec.Identifier)
	observed.Tags = map[string]string{
		harnessAgentCRUIDTag: string(agent.UID),
		"unrelated":          "preserved",
	}
	return observed
}

func (f *agentRegistrationFixture) getAgent(
	t *testing.T,
) *infrastructurev1.HarnessGitopsAgent {
	t.Helper()
	persisted := &infrastructurev1.HarnessGitopsAgent{}
	if err := f.client.Get(
		context.Background(),
		client.ObjectKeyFromObject(f.agent),
		persisted,
	); err != nil {
		t.Fatalf("get persisted Agent: %v", err)
	}
	return persisted
}

func (f *agentRegistrationFixture) assertManagedWithToken(t *testing.T) {
	t.Helper()
	persisted := f.getAgent(t)
	if persisted.Status.CreationState != "" {
		t.Fatalf("creation state = %q, want empty", persisted.Status.CreationState)
	}
	if persisted.Status.AgentOwnership != infrastructurev1.OwnershipManaged {
		t.Fatalf("ownership = %q, want Managed", persisted.Status.AgentOwnership)
	}
	if !harnessapi.AgentIdentifiersEquivalent(
		persisted.Spec.Scope,
		persisted.Status.AgentIdentifier,
		persisted.Spec.Identifier,
	) {
		t.Fatalf("identifier = %q, want %q", persisted.Status.AgentIdentifier, persisted.Spec.Identifier)
	}

	secret := &corev1.Secret{}
	if err := f.client.Get(
		context.Background(),
		client.ObjectKey{
			Name:      persisted.Spec.TokenSecretRef,
			Namespace: persisted.Namespace,
		},
		secret,
	); err != nil {
		t.Fatalf("get token Secret: %v", err)
	}
	if got := string(secret.Data[gitopsAgentTokenSecretKey]); got != f.agentAPI.resolvedToken {
		t.Fatalf("token = %q, want %q", got, f.agentAPI.resolvedToken)
	}
}

func containsReplacementCRGuidance(message string) bool {
	return strings.Contains(message, "replacement HarnessGitopsAgent CR") &&
		strings.Contains(message, "existingAgentIdentifier")
}
