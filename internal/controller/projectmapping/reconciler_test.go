package projectmapping

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

const (
	mappingControllerNamespace     = "mapping-controller-tests"
	mappingControllerAgentName     = "mapping-agent"
	mappingControllerAccountID     = "agent-account"
	mappingControllerAgentOrgID    = "agent-home-org"
	mappingControllerAgentProject  = "agent-home-project"
	mappingControllerTargetOrgID   = "target-org"
	mappingControllerTargetProject = "target-project"
	mappingControllerAppProject    = "payments-app-project"
	mappingControllerAgentID       = "runtime-agent"
	mappingControllerAPISecret     = "harness-api-key"
	mappingSelectionID             = "mapping-a"
)

type fakeProjectMappingReconcileAPI struct {
	listResults  [][]harnessapi.ProjectMapping
	listErr      error
	listCalls    int
	createResult harnessapi.ProjectMapping
	createErr    error
	createCalls  int
	deleteErr    error
	deleteCalls  int
	deleteIDs    []string
	requests     []harnessapi.ProjectMappingRequest
	onList       func()
	onCreate     func()
}

func (f *fakeProjectMappingReconcileAPI) List(
	_ context.Context,
	_ *harnessapi.Session,
	request harnessapi.ProjectMappingRequest,
) ([]harnessapi.ProjectMapping, error) {
	f.requests = append(f.requests, request)
	f.listCalls++
	if f.onList != nil {
		f.onList()
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.listResults) == 0 {
		return nil, nil
	}
	index := f.listCalls - 1
	if index >= len(f.listResults) {
		index = len(f.listResults) - 1
	}
	return append([]harnessapi.ProjectMapping(nil), f.listResults[index]...), nil
}

func (f *fakeProjectMappingReconcileAPI) Create(
	_ context.Context,
	_ *harnessapi.Session,
	request harnessapi.ProjectMappingRequest,
) (harnessapi.ProjectMapping, error) {
	f.requests = append(f.requests, request)
	f.createCalls++
	if f.onCreate != nil {
		f.onCreate()
	}
	return f.createResult, f.createErr
}

func (f *fakeProjectMappingReconcileAPI) Delete(
	_ context.Context,
	_ *harnessapi.Session,
	request harnessapi.ProjectMappingRequest,
	mappingID string,
) error {
	f.requests = append(f.requests, request)
	f.deleteCalls++
	f.deleteIDs = append(f.deleteIDs, mappingID)
	return f.deleteErr
}

type fakeMappingAgentReadinessAPI struct {
	readiness harnessapi.AgentReadiness
	err       error
	calls     int
	agents    []harnessapi.Agent
}

func (f *fakeMappingAgentReadinessAPI) Readiness(
	_ context.Context,
	_ *harnessapi.Session,
	agent harnessapi.Agent,
) (harnessapi.AgentReadiness, error) {
	f.calls++
	f.agents = append(f.agents, agent)
	return f.readiness, f.err
}

type mappingReconcilerFixture struct {
	reconciler    *Reconciler
	mappingAPI    *fakeProjectMappingReconcileAPI
	readinessAPI  *fakeMappingAgentReadinessAPI
	key           client.ObjectKey
	statusUpdates *int
	statusFailAt  *int
	statusFailErr *error
}

func TestProjectMappingCreateLifecycleAtEveryAgentScope(t *testing.T) {
	tests := []struct {
		scope          string
		configureAgent func(*infrastructurev1.HarnessGitopsAgent)
		configureMap   func(*infrastructurev1.HarnessGitopsProjectMapping)
		wantAgent      harnessapi.Scope
		wantTarget     harnessapi.Scope
		returnedAgent  string
	}{
		{
			scope: agentScopeProject,
			wantAgent: harnessapi.Scope{
				OrgIdentifier:     mappingControllerAgentOrgID,
				ProjectIdentifier: mappingControllerAgentProject,
			},
			wantTarget: harnessapi.Scope{
				OrgIdentifier:     mappingControllerAgentOrgID,
				ProjectIdentifier: mappingControllerAgentProject,
			},
			returnedAgent: mappingControllerAgentID,
		},
		{
			scope: agentScopeOrg,
			configureMap: func(mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				mapping.Spec.ProjectID = mappingControllerTargetProject
			},
			wantAgent: harnessapi.Scope{
				OrgIdentifier: mappingControllerAgentOrgID,
			},
			wantTarget: harnessapi.Scope{
				OrgIdentifier:     mappingControllerAgentOrgID,
				ProjectIdentifier: mappingControllerTargetProject,
			},
			returnedAgent: "org." + mappingControllerAgentID,
		},
		{
			scope: agentScopeAccount,
			configureAgent: func(agent *infrastructurev1.HarnessGitopsAgent) {
				// These deliberately differ from the target and must not leak.
				agent.Spec.OrgId = "account-agent-org-must-not-leak"
				agent.Spec.ProjectId = "account-agent-project-must-not-leak"
			},
			configureMap: func(mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				mapping.Spec.OrgID = mappingControllerTargetOrgID
				mapping.Spec.ProjectID = mappingControllerTargetProject
				mapping.Spec.AutoCreateServiceEnv = true
			},
			wantAgent: harnessapi.Scope{},
			wantTarget: harnessapi.Scope{
				OrgIdentifier:     mappingControllerTargetOrgID,
				ProjectIdentifier: mappingControllerTargetProject,
			},
			returnedAgent: "account." + mappingControllerAgentID,
		},
	}

	for _, test := range tests {
		t.Run(test.scope, func(t *testing.T) {
			agent := newMappingControllerAgent(test.scope)
			mapping := newMappingControllerResource()
			if test.configureAgent != nil {
				test.configureAgent(agent)
			}
			if test.configureMap != nil {
				test.configureMap(mapping)
			}

			fixture := newMappingReconcilerFixture(t, agent, mapping, true)
			request := resolvedRequestForTest(t, agent, mapping)
			created := exactMappingForRequest(request, "created-mapping-id", test.returnedAgent)
			fixture.mappingAPI.createResult = created
			fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{
				nil,
				{created},
			}

			fixture.mappingAPI.onCreate = func() {
				current := fixture.getMapping(t)
				if current.Status.CreationState != infrastructurev1.MappingCreationPending {
					t.Fatalf("creationState at POST = %q, want Pending", current.Status.CreationState)
				}
				if current.Status.Remote == nil {
					t.Fatal("resolved remote tuple was not persisted before POST")
				}
				if current.Status.Remote.MappingID != "" || current.Status.Remote.Ownership != "" {
					t.Fatalf(
						"pre-POST identity = (%q, %q), want empty",
						current.Status.Remote.MappingID,
						current.Status.Remote.Ownership,
					)
				}
			}

			finalizerResult, err := fixture.reconcile(t)
			if err != nil {
				t.Fatalf("finalizer reconcile: %v", err)
			}
			if finalizerResult.RequeueAfter <= 0 || fixture.mappingAPI.createCalls != 0 {
				t.Fatal("finalizer was not persisted before remote create")
			}

			result, err := fixture.reconcile(t)
			if err != nil {
				t.Fatalf("create reconcile: %v", err)
			}
			if result.RequeueAfter <= 0 {
				t.Fatal("confirmed create did not request immediate verification")
			}
			if fixture.mappingAPI.createCalls != 1 {
				t.Fatalf("create calls = %d, want 1", fixture.mappingAPI.createCalls)
			}

			current := fixture.getMapping(t)
			if current.Status.CreationState != infrastructurev1.MappingCreationPending {
				t.Fatalf("creationState after POST = %q, want Pending", current.Status.CreationState)
			}
			if current.Status.Remote == nil {
				t.Fatal("remote status is nil after POST")
			}
			if current.Status.Remote.MappingID != "created-mapping-id" {
				t.Fatalf("mapping ID = %q, want created-mapping-id", current.Status.Remote.MappingID)
			}
			if current.Status.Remote.Ownership != "" {
				t.Fatalf("ownership before verification = %q, want empty", current.Status.Remote.Ownership)
			}
			if current.Status.Remote.Agent.Identifier != test.returnedAgent {
				t.Fatalf(
					"recorded agent path ID = %q, want %q",
					current.Status.Remote.Agent.Identifier,
					test.returnedAgent,
				)
			}
			if current.Status.Remote.Agent.OrgID != test.wantAgent.OrgIdentifier ||
				current.Status.Remote.Agent.ProjectID != test.wantAgent.ProjectIdentifier {
				t.Fatalf("agent status scope = %#v, want %#v", current.Status.Remote.Agent, test.wantAgent)
			}
			if current.Status.Remote.Target.OrgID != test.wantTarget.OrgIdentifier ||
				current.Status.Remote.Target.ProjectID != test.wantTarget.ProjectIdentifier {
				t.Fatalf("target status = %#v, want %#v", current.Status.Remote.Target, test.wantTarget)
			}

			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("verification reconcile: %v", err)
			}
			current = fixture.getMapping(t)
			if current.Status.CreationState != "" {
				t.Fatalf("creationState after verification = %q, want empty", current.Status.CreationState)
			}
			if current.Status.Remote.Ownership != infrastructurev1.OwnershipManaged {
				t.Fatalf("ownership = %q, want Managed", current.Status.Remote.Ownership)
			}
			assertReadyCondition(t, current, metav1.ConditionTrue, projectMappingReasonMappingVerified)

			if got := fixture.readinessAPI.agents[0]; got.OrgIdentifier != test.wantAgent.OrgIdentifier ||
				got.ProjectIdentifier != test.wantAgent.ProjectIdentifier {
				t.Fatalf("readiness agent scope = %#v, want %#v", got, test.wantAgent)
			}
		})
	}
}

func TestProjectMappingPreexistingAndAdoptionOwnership(t *testing.T) {
	tests := []struct {
		name          string
		adoptID       string
		wantOwnership infrastructurev1.ResourceOwnership
		wantReady     metav1.ConditionStatus
		wantReason    string
	}{
		{
			name:          "preexisting exact mapping is external",
			wantOwnership: infrastructurev1.OwnershipExternal,
			wantReady:     metav1.ConditionTrue,
			wantReason:    projectMappingReasonMappingExternal,
		},
		{
			name:          "exact ID is adopted",
			adoptID:       "existing-mapping",
			wantOwnership: infrastructurev1.OwnershipAdopted,
			wantReady:     metav1.ConditionTrue,
			wantReason:    projectMappingReasonMappingAdopted,
		},
		{
			name:          "wrong adoption ID fails closed",
			adoptID:       "wrong-mapping",
			wantOwnership: "",
			wantReady:     metav1.ConditionFalse,
			wantReason:    projectMappingReasonAdoptionFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeAccount)
			mapping := newMappingControllerResource()
			mapping.Finalizers = []string{harnessProjectMappingFinalizer}
			mapping.Spec.OrgID = mappingControllerTargetOrgID
			mapping.Spec.ProjectID = mappingControllerTargetProject
			mapping.Spec.AdoptMappingID = test.adoptID
			fixture := newMappingReconcilerFixture(t, agent, mapping, true)
			request := resolvedRequestForTest(t, agent, mapping)
			fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{
				exactMappingForRequest(request, "existing-mapping", "account."+mappingControllerAgentID),
			}}

			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			current := fixture.getMapping(t)
			if current.Status.Remote == nil {
				t.Fatal("observed external row was not recorded")
			}
			if current.Status.Remote.MappingID != "existing-mapping" {
				t.Fatalf("mapping ID = %q, want existing-mapping", current.Status.Remote.MappingID)
			}
			if current.Status.Remote.Ownership != test.wantOwnership {
				t.Fatalf("ownership = %q, want %q", current.Status.Remote.Ownership, test.wantOwnership)
			}
			assertReadyCondition(t, current, test.wantReady, test.wantReason)
			if fixture.mappingAPI.createCalls != 0 {
				t.Fatalf("create calls = %d, want 0", fixture.mappingAPI.createCalls)
			}
		})
	}
}

func TestProjectMappingAdoptOnlyNeverCreates(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping := newMappingControllerResource()
	mapping.Finalizers = []string{harnessProjectMappingFinalizer}
	mapping.Spec.OrgID = mappingControllerTargetOrgID
	mapping.Spec.ProjectID = mappingControllerTargetProject
	mapping.Spec.AdoptMappingID = "missing-mapping"
	fixture := newMappingReconcilerFixture(t, agent, mapping, true)

	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if fixture.mappingAPI.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", fixture.mappingAPI.createCalls)
	}
	assertReadyCondition(
		t,
		fixture.getMapping(t),
		metav1.ConditionFalse,
		projectMappingReasonAdoptionFailed,
	)
}

func TestOwnedProjectMappingAdoptionCannotTransferOwnership(t *testing.T) {
	tests := []struct {
		name     string
		liveRows func(harnessapi.ProjectMappingRequest) []harnessapi.ProjectMapping
	}{
		{
			name: "owned A and adopt target B both exist",
			liveRows: func(request harnessapi.ProjectMappingRequest) []harnessapi.ProjectMapping {
				return []harnessapi.ProjectMapping{
					exactMappingForRequest(request, mappingSelectionID, "account."+mappingControllerAgentID),
					exactMappingForRequest(request, "mapping-b", "account."+mappingControllerAgentID),
				}
			},
		},
		{
			name: "only owned A exists",
			liveRows: func(request harnessapi.ProjectMappingRequest) []harnessapi.ProjectMapping {
				return []harnessapi.ProjectMapping{
					exactMappingForRequest(request, mappingSelectionID, "account."+mappingControllerAgentID),
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent, mapping, request := newAccountMappingControllerTest(t)
			mapping.Spec.AdoptMappingID = "mapping-b"
			mapping.Status.Remote = remoteStatusForRequest(request)
			mapping.Status.Remote.MappingID = mappingSelectionID
			mapping.Status.Remote.Ownership = infrastructurev1.OwnershipManaged
			wantRemote := mapping.Status.Remote.DeepCopy()
			rows := test.liveRows(request)

			selection := selectProjectMapping(rows, request, mapping)
			if selection.mapping == nil || selection.mapping.Identifier != mappingSelectionID {
				t.Fatalf("selected mapping = %#v, want remembered mapping-a", selection.mapping)
			}

			fixture := newMappingReconcilerFixture(t, agent, mapping, true)
			fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{rows}
			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			current := fixture.getMapping(t)
			if !reflect.DeepEqual(current.Status.Remote, wantRemote) {
				t.Fatalf(
					"owned cleanup snapshot changed:\n got %#v\nwant %#v",
					current.Status.Remote,
					wantRemote,
				)
			}
			assertReadyCondition(
				t,
				current,
				metav1.ConditionFalse,
				projectMappingReasonAdoptionFailed,
			)
		})
	}
}

func TestWrongAdoptionPreservesOutcomeUnknownCandidate(t *testing.T) {
	agent, mapping, request := newAccountMappingControllerTest(t)
	mapping.Spec.AdoptMappingID = "mapping-b"
	mapping.Status.CreationState = infrastructurev1.MappingCreationOutcomeUnknown
	mapping.Status.Remote = remoteStatusForRequest(request)
	mapping.Status.Remote.MappingID = mappingSelectionID
	wantRemote := mapping.Status.Remote.DeepCopy()
	rows := []harnessapi.ProjectMapping{
		exactMappingForRequest(request, mappingSelectionID, "account."+mappingControllerAgentID),
		exactMappingForRequest(request, "mapping-b", "account."+mappingControllerAgentID),
	}

	selection := selectProjectMapping(rows, request, mapping)
	if selection.mapping == nil || selection.mapping.Identifier != mappingSelectionID {
		t.Fatalf("selected mapping = %#v, want remembered candidate mapping-a", selection.mapping)
	}

	fixture := newMappingReconcilerFixture(t, agent, mapping, true)
	fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{rows}
	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	current := fixture.getMapping(t)
	if current.Status.CreationState != infrastructurev1.MappingCreationOutcomeUnknown {
		t.Fatalf("creationState = %q, want OutcomeUnknown", current.Status.CreationState)
	}
	if current.Status.Remote.Ownership != "" ||
		!reflect.DeepEqual(current.Status.Remote, wantRemote) {
		t.Fatalf(
			"unresolved candidate changed:\n got %#v\nwant %#v",
			current.Status.Remote,
			wantRemote,
		)
	}
	assertReadyCondition(
		t,
		current,
		metav1.ConditionFalse,
		projectMappingReasonAdoptionFailed,
	)
}

func TestProjectMappingTupleMismatchPreservesOwnedCleanupSnapshot(t *testing.T) {
	agent, mapping, request := newAccountMappingControllerTest(t)
	mapping.Status.Remote = remoteStatusForRequest(request)
	mapping.Status.Remote.MappingID = mappingSelectionID
	mapping.Status.Remote.Ownership = infrastructurev1.OwnershipManaged
	wantRemote := mapping.Status.Remote.DeepCopy()
	mismatch := exactMappingForRequest(request, mappingSelectionID, "account."+mappingControllerAgentID)
	mismatch.AutoCreateServiceEnv = !request.AutoCreateServiceEnv

	fixture := newMappingReconcilerFixture(t, agent, mapping, true)
	fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{mismatch}}
	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	current := fixture.getMapping(t)
	if !reflect.DeepEqual(current.Status.Remote, wantRemote) {
		t.Fatalf(
			"same-ID mismatch changed cleanup snapshot:\n got %#v\nwant %#v",
			current.Status.Remote,
			wantRemote,
		)
	}
	assertReadyCondition(
		t,
		current,
		metav1.ConditionFalse,
		projectMappingReasonMappingMismatch,
	)
}

func TestProjectMappingExactReplacementDoesNotHideOwnedTupleMismatch(t *testing.T) {
	for _, ownership := range []infrastructurev1.ResourceOwnership{
		infrastructurev1.OwnershipManaged,
		infrastructurev1.OwnershipAdopted,
	} {
		t.Run(string(ownership), func(t *testing.T) {
			agent, mapping, request := newAccountMappingControllerTest(t)
			mapping.Status.Remote = remoteStatusForRequest(request)
			mapping.Status.Remote.MappingID = "owned-drifted-id"
			mapping.Status.Remote.Ownership = ownership
			wantRemote := mapping.Status.Remote.DeepCopy()

			driftedOwned := exactMappingForRequest(
				request,
				"owned-drifted-id",
				"account."+mappingControllerAgentID,
			)
			driftedOwned.ProjectIdentifier = "different-remote-project"
			exactReplacement := exactMappingForRequest(
				request,
				"replacement-exact-id",
				"account."+mappingControllerAgentID,
			)

			fixture := newMappingReconcilerFixture(t, agent, mapping, true)
			fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{
				driftedOwned,
				exactReplacement,
			}}
			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			current := fixture.getMapping(t)
			if !reflect.DeepEqual(current.Status.Remote, wantRemote) {
				t.Fatalf(
					"exact replacement changed owned cleanup snapshot:\n got %#v\nwant %#v",
					current.Status.Remote,
					wantRemote,
				)
			}
			if fixture.mappingAPI.createCalls != 0 || fixture.mappingAPI.deleteCalls != 0 {
				t.Fatalf(
					"remote mutations = (create=%d, delete=%d), want zero",
					fixture.mappingAPI.createCalls,
					fixture.mappingAPI.deleteCalls,
				)
			}
			assertReadyCondition(
				t,
				current,
				metav1.ConditionFalse,
				projectMappingReasonMappingMismatch,
			)
		})
	}
}

func TestProjectMappingSameIDPreservesEstablishedOwnership(t *testing.T) {
	for _, ownership := range []infrastructurev1.ResourceOwnership{
		infrastructurev1.OwnershipManaged,
		infrastructurev1.OwnershipAdopted,
	} {
		t.Run(string(ownership), func(t *testing.T) {
			agent, mapping, request := newAccountMappingControllerTest(t)
			mapping.Spec.AdoptMappingID = mappingSelectionID
			mapping.Status.Remote = remoteStatusForRequest(request)
			mapping.Status.Remote.MappingID = mappingSelectionID
			mapping.Status.Remote.Ownership = ownership
			wantRemote := mapping.Status.Remote.DeepCopy()
			observed := exactMappingForRequest(
				request,
				mappingSelectionID,
				"account."+mappingControllerAgentID,
			)

			fixture := newMappingReconcilerFixture(t, agent, mapping, true)
			fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{observed}}
			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			current := fixture.getMapping(t)
			if !reflect.DeepEqual(current.Status.Remote, wantRemote) {
				t.Fatalf(
					"same-ID %s snapshot changed:\n got %#v\nwant %#v",
					ownership,
					current.Status.Remote,
					wantRemote,
				)
			}
			assertReadyCondition(
				t,
				current,
				metav1.ConditionTrue,
				projectMappingReasonMappingVerified,
			)
		})
	}
}

func TestProjectMappingPendingRestartNeverCreates(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping := newMappingControllerResource()
	mapping.Finalizers = []string{harnessProjectMappingFinalizer}
	mapping.Spec.OrgID = mappingControllerTargetOrgID
	mapping.Spec.ProjectID = mappingControllerTargetProject
	request := resolvedRequestForTest(t, agent, mapping)
	mapping.Status.CreationState = infrastructurev1.MappingCreationPending
	mapping.Status.Remote = remoteStatusForRequest(request)

	fixture := newMappingReconcilerFixture(t, agent, mapping, true)
	discovered := exactMappingForRequest(request, "candidate-mapping", "account."+mappingControllerAgentID)
	fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{discovered}}

	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if fixture.mappingAPI.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", fixture.mappingAPI.createCalls)
	}
	current := fixture.getMapping(t)
	if current.Status.CreationState != infrastructurev1.MappingCreationOutcomeUnknown {
		t.Fatalf("creationState = %q, want OutcomeUnknown", current.Status.CreationState)
	}
	if current.Status.Remote == nil ||
		current.Status.Remote.MappingID != "candidate-mapping" ||
		current.Status.Remote.Ownership != "" {
		t.Fatalf("unresolved candidate status = %#v", current.Status.Remote)
	}
	assertReadyCondition(t, current, metav1.ConditionFalse, projectMappingReasonCreateUnknown)
}

func TestProjectMappingConfirmedCreateOutcomeUnknownDoesNotRetry(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping := newMappingControllerResource()
	mapping.Finalizers = []string{harnessProjectMappingFinalizer}
	mapping.Spec.OrgID = mappingControllerTargetOrgID
	mapping.Spec.ProjectID = mappingControllerTargetProject
	fixture := newMappingReconcilerFixture(t, agent, mapping, true)
	fixture.mappingAPI.createErr = fmt.Errorf(
		"%w: timeout",
		harnessapi.ErrProjectMappingCreateOutcomeUnknown,
	)

	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("ambiguous create reconcile: %v", err)
	}
	if fixture.mappingAPI.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", fixture.mappingAPI.createCalls)
	}
	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("outcome-unknown reconcile: %v", err)
	}
	if fixture.mappingAPI.createCalls != 1 {
		t.Fatalf("create calls after retry = %d, want 1", fixture.mappingAPI.createCalls)
	}
	current := fixture.getMapping(t)
	if current.Status.CreationState != infrastructurev1.MappingCreationOutcomeUnknown {
		t.Fatalf("creationState = %q, want OutcomeUnknown", current.Status.CreationState)
	}
}

func TestProjectMappingExternalRecreationDemotesOwnership(t *testing.T) {
	for _, ownership := range []infrastructurev1.ResourceOwnership{
		infrastructurev1.OwnershipManaged,
		infrastructurev1.OwnershipAdopted,
	} {
		t.Run(string(ownership), func(t *testing.T) {
			agent, mapping, request := newAccountMappingControllerTest(t)
			mapping.Status.Remote = remoteStatusForRequest(request)
			mapping.Status.Remote.MappingID = "old-owned-id"
			mapping.Status.Remote.Ownership = ownership

			fixture := newMappingReconcilerFixture(t, agent, mapping, true)
			fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{
				exactMappingForRequest(request, "new-external-id", "account."+mappingControllerAgentID),
			}}

			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			current := fixture.getMapping(t)
			if current.Status.Remote.MappingID != "new-external-id" {
				t.Fatalf("mapping ID = %q, want new-external-id", current.Status.Remote.MappingID)
			}
			if current.Status.Remote.Ownership != infrastructurev1.OwnershipExternal {
				t.Fatalf("ownership = %q, want External", current.Status.Remote.Ownership)
			}
		})
	}
}

func TestProjectMappingAutoCreateMismatchBlocksCreate(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping := newMappingControllerResource()
	mapping.Finalizers = []string{harnessProjectMappingFinalizer}
	mapping.Spec.OrgID = mappingControllerTargetOrgID
	mapping.Spec.ProjectID = mappingControllerTargetProject
	mapping.Spec.AutoCreateServiceEnv = true
	request := resolvedRequestForTest(t, agent, mapping)
	mismatch := exactMappingForRequest(request, "mismatch-id", "account."+mappingControllerAgentID)
	mismatch.AutoCreateServiceEnv = false

	fixture := newMappingReconcilerFixture(t, agent, mapping, true)
	fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{mismatch}}
	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if fixture.mappingAPI.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", fixture.mappingAPI.createCalls)
	}
	assertReadyCondition(
		t,
		fixture.getMapping(t),
		metav1.ConditionFalse,
		projectMappingReasonMappingMismatch,
	)
}

func TestProjectMappingAdoptionRequiresIDAndCompleteTuple(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*harnessapi.ProjectMapping)
	}{
		{
			name: "wrong project",
			mutate: func(mapping *harnessapi.ProjectMapping) {
				mapping.ProjectIdentifier = "different-target-project"
			},
		},
		{
			name: "wrong auto-create option",
			mutate: func(mapping *harnessapi.ProjectMapping) {
				mapping.AutoCreateServiceEnv = !mapping.AutoCreateServiceEnv
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent, mapping, request := newAccountMappingControllerTest(t)
			mapping.Spec.AdoptMappingID = "adopt-target"
			observed := exactMappingForRequest(
				request,
				"adopt-target",
				"account."+mappingControllerAgentID,
			)
			test.mutate(&observed)

			fixture := newMappingReconcilerFixture(t, agent, mapping, true)
			fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{observed}}
			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			current := fixture.getMapping(t)
			if current.Status.Remote == nil {
				t.Fatal("resolved desired tuple was not recorded")
			}
			if current.Status.Remote.MappingID != "" ||
				current.Status.Remote.Ownership != "" {
				t.Fatalf("mismatched adoption gained identity or ownership: %#v", current.Status.Remote)
			}
			if fixture.mappingAPI.createCalls != 0 {
				t.Fatalf("create calls = %d, want 0", fixture.mappingAPI.createCalls)
			}
			assertReadyCondition(
				t,
				current,
				metav1.ConditionFalse,
				projectMappingReasonAdoptionFailed,
			)
		})
	}
}

func TestProjectMappingDuplicateExactRowsRequireAnID(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping := newMappingControllerResource()
	mapping.Finalizers = []string{harnessProjectMappingFinalizer}
	mapping.Spec.OrgID = mappingControllerTargetOrgID
	mapping.Spec.ProjectID = mappingControllerTargetProject
	request := resolvedRequestForTest(t, agent, mapping)

	fixture := newMappingReconcilerFixture(t, agent, mapping, true)
	fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{
		exactMappingForRequest(request, "duplicate-a", "account."+mappingControllerAgentID),
		exactMappingForRequest(request, "duplicate-b", "account."+mappingControllerAgentID),
	}}
	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if fixture.mappingAPI.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", fixture.mappingAPI.createCalls)
	}
	assertReadyCondition(
		t,
		fixture.getMapping(t),
		metav1.ConditionFalse,
		projectMappingReasonDuplicateMapping,
	)
}

func TestProjectMappingDuplicateFailurePreservesOwnedCleanupSnapshot(t *testing.T) {
	agent, mapping, request := newAccountMappingControllerTest(t)
	mapping.Status.Remote = remoteStatusForRequest(request)
	mapping.Status.Remote.MappingID = "owned-row-no-longer-listed"
	mapping.Status.Remote.Ownership = infrastructurev1.OwnershipAdopted
	wantRemote := mapping.Status.Remote.DeepCopy()

	fixture := newMappingReconcilerFixture(t, agent, mapping, true)
	fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{
		exactMappingForRequest(request, "replacement-a", "account."+mappingControllerAgentID),
		exactMappingForRequest(request, "replacement-b", "account."+mappingControllerAgentID),
	}}
	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	current := fixture.getMapping(t)
	if !reflect.DeepEqual(current.Status.Remote, wantRemote) {
		t.Fatalf(
			"duplicate failure changed cleanup snapshot:\n got %#v\nwant %#v",
			current.Status.Remote,
			wantRemote,
		)
	}
	assertReadyCondition(
		t,
		current,
		metav1.ConditionFalse,
		projectMappingReasonDuplicateMapping,
	)
}

func TestProjectMappingWaitsForAppProjectAndAgentHealth(t *testing.T) {
	t.Run("AppProject", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeProject)
		mapping := newMappingControllerResource()
		mapping.Finalizers = []string{harnessProjectMappingFinalizer}
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)

		result, err := fixture.reconcile(t)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if result.RequeueAfter != time.Second {
			t.Fatalf("requeueAfter = %s, want 1s", result.RequeueAfter)
		}
		if fixture.readinessAPI.calls != 0 || fixture.mappingAPI.listCalls != 0 {
			t.Fatal("Harness was contacted before AppProject existed")
		}
		assertReadyCondition(
			t,
			fixture.getMapping(t),
			metav1.ConditionFalse,
			projectMappingReasonAppProjectNotFound,
		)
	})

	t.Run("health", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeProject)
		mapping := newMappingControllerResource()
		mapping.Finalizers = []string{harnessProjectMappingFinalizer}
		fixture := newMappingReconcilerFixture(t, agent, mapping, true)
		fixture.readinessAPI.readiness = harnessapi.AgentReadiness{
			Exists:  true,
			Ready:   false,
			Message: "agent is Disconnected",
		}

		result, err := fixture.reconcile(t)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if result.RequeueAfter != time.Second {
			t.Fatalf("requeueAfter = %s, want 1s", result.RequeueAfter)
		}
		if fixture.mappingAPI.listCalls != 0 {
			t.Fatal("mappings were listed before the Agent became healthy")
		}
		assertReadyCondition(
			t,
			fixture.getMapping(t),
			metav1.ConditionFalse,
			projectMappingReasonAgentNotHealthy,
		)
	})
}

func TestProjectMappingReadyStatusUpdateIsNoOpAware(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping := newMappingControllerResource()
	mapping.Finalizers = []string{harnessProjectMappingFinalizer}
	mapping.Spec.OrgID = mappingControllerTargetOrgID
	mapping.Spec.ProjectID = mappingControllerTargetProject
	request := resolvedRequestForTest(t, agent, mapping)
	observed := exactMappingForRequest(request, "stable-external", "account."+mappingControllerAgentID)
	mapping.Status.ObservedGeneration = mapping.Generation
	mapping.Status.Remote = remoteStatusForObserved(request, observed)
	mapping.Status.Remote.Ownership = infrastructurev1.OwnershipExternal
	apiMeta.SetStatusCondition(&mapping.Status.Conditions, metav1.Condition{
		Type:               projectMappingReadyCondition,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: mapping.Generation,
		Reason:             projectMappingReasonMappingExternal,
		Message:            "The exact Harness mapping exists and is treated as external",
	})

	fixture := newMappingReconcilerFixture(t, agent, mapping, true)
	fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{{observed}}
	before := *fixture.statusUpdates
	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := *fixture.statusUpdates; got != before {
		t.Fatalf("status updates = %d after stable reconcile, want %d", got, before)
	}
}

func TestProjectMappingDefiniteCreateErrorClearsPending(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping := newMappingControllerResource()
	mapping.Finalizers = []string{harnessProjectMappingFinalizer}
	mapping.Spec.OrgID = mappingControllerTargetOrgID
	mapping.Spec.ProjectID = mappingControllerTargetProject
	fixture := newMappingReconcilerFixture(t, agent, mapping, true)
	fixture.mappingAPI.createErr = errors.New("permission denied")

	if _, err := fixture.reconcile(t); err == nil {
		t.Fatal("expected create error")
	}
	current := fixture.getMapping(t)
	if current.Status.CreationState != "" {
		t.Fatalf("creationState = %q, want empty after definite error", current.Status.CreationState)
	}
	assertReadyCondition(
		t,
		current,
		metav1.ConditionFalse,
		projectMappingReasonVerificationFailed,
	)
}

func TestProjectMappingCreateRequiresDurableStatusWrites(t *testing.T) {
	t.Run("intent status failure prevents create", func(t *testing.T) {
		agent, mapping, _ := newAccountMappingControllerTest(t)
		fixture := newMappingReconcilerFixture(t, agent, mapping, true)
		statusErr := errors.New("persist create intent")
		*fixture.statusFailAt = 1
		*fixture.statusFailErr = statusErr

		if _, err := fixture.reconcile(t); !errors.Is(err, statusErr) {
			t.Fatalf("reconcile error = %v, want %v", err, statusErr)
		}
		if fixture.mappingAPI.createCalls != 0 {
			t.Fatalf("create calls = %d, want 0", fixture.mappingAPI.createCalls)
		}
		current := fixture.getMapping(t)
		if current.Status.CreationState != "" || current.Status.Remote != nil {
			t.Fatalf("failed intent write changed durable status: %#v", current.Status)
		}
	})

	t.Run("returned ID status failure never repeats create", func(t *testing.T) {
		agent, mapping, request := newAccountMappingControllerTest(t)
		created := exactMappingForRequest(
			request,
			"create-status-failure-id",
			"account."+mappingControllerAgentID,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, true)
		fixture.mappingAPI.createResult = created
		fixture.mappingAPI.listResults = [][]harnessapi.ProjectMapping{
			nil,
			{created},
		}
		statusErr := errors.New("persist returned mapping ID")
		*fixture.statusFailAt = 2
		*fixture.statusFailErr = statusErr

		if _, err := fixture.reconcile(t); !errors.Is(err, statusErr) {
			t.Fatalf("create reconcile error = %v, want %v", err, statusErr)
		}
		if fixture.mappingAPI.createCalls != 1 {
			t.Fatalf("create calls = %d, want 1", fixture.mappingAPI.createCalls)
		}
		current := fixture.getMapping(t)
		if current.Status.CreationState != infrastructurev1.MappingCreationPending ||
			current.Status.Remote == nil ||
			current.Status.Remote.MappingID != "" {
			t.Fatalf("durable pre-create intent was not retained: %#v", current.Status)
		}

		if _, err := fixture.reconcile(t); err != nil {
			t.Fatalf("reconcile after returned-ID status failure: %v", err)
		}
		if fixture.mappingAPI.createCalls != 1 {
			t.Fatalf("create calls after recovery = %d, want 1", fixture.mappingAPI.createCalls)
		}
		current = fixture.getMapping(t)
		if current.Status.CreationState != infrastructurev1.MappingCreationOutcomeUnknown ||
			current.Status.Remote == nil ||
			current.Status.Remote.MappingID != created.Identifier ||
			current.Status.Remote.Ownership != "" {
			t.Fatalf("ambiguous recovery status = %#v", current.Status)
		}
	})
}

func newMappingReconcilerFixture(
	t *testing.T,
	agent *infrastructurev1.HarnessGitopsAgent,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
	includeAppProject bool,
	extraObjects ...client.Object,
) *mappingReconcilerFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add operator scheme: %v", err)
	}

	objects := []client.Object{
		agent,
		mapping,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mappingControllerAPISecret,
				Namespace: mappingControllerNamespace,
			},
			Data: map[string][]byte{"api_key": []byte("test-api-key")},
		},
	}
	if includeAppProject {
		objects = append(
			objects,
			newAppProjectObject(mappingControllerNamespace, mappingControllerAppProject),
		)
	}
	objects = append(objects, extraObjects...)

	statusUpdates := 0
	statusFailAt := 0
	var statusFailErr error
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&infrastructurev1.HarnessGitopsAgent{},
			&infrastructurev1.HarnessGitopsProjectMapping{},
		).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				ctx context.Context,
				base client.Client,
				subResourceName string,
				obj client.Object,
				opts ...client.SubResourceUpdateOption,
			) error {
				if subResourceName == "status" {
					statusUpdates++
					if statusFailAt == statusUpdates && statusFailErr != nil {
						return statusFailErr
					}
				}
				return base.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()

	mappingAPI := &fakeProjectMappingReconcileAPI{}
	readinessAPI := &fakeMappingAgentReadinessAPI{
		readiness: harnessapi.AgentReadiness{
			Exists: true,
			Ready:  true,
		},
	}
	reconciler := &Reconciler{
		Client:                         k8sClient,
		APIReader:                      k8sClient,
		AppProjectPendingRetryInterval: time.Second,
		HarnessMappingResyncInterval:   2 * time.Second,
		mappingAPI:                     mappingAPI,
		agentAPI:                       readinessAPI,
	}
	return &mappingReconcilerFixture{
		reconciler:    reconciler,
		mappingAPI:    mappingAPI,
		readinessAPI:  readinessAPI,
		key:           client.ObjectKeyFromObject(mapping),
		statusUpdates: &statusUpdates,
		statusFailAt:  &statusFailAt,
		statusFailErr: &statusFailErr,
	}
}

func (f *mappingReconcilerFixture) reconcile(t *testing.T) (ctrl.Result, error) {
	t.Helper()
	return f.reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: f.key},
	)
}

func (f *mappingReconcilerFixture) getMapping(
	t *testing.T,
) *infrastructurev1.HarnessGitopsProjectMapping {
	t.Helper()
	mapping := &infrastructurev1.HarnessGitopsProjectMapping{}
	if err := f.reconciler.Get(context.Background(), f.key, mapping); err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	return mapping
}

func (f *mappingReconcilerFixture) getMappingOrNil(
	t *testing.T,
) *infrastructurev1.HarnessGitopsProjectMapping {
	t.Helper()
	mapping := &infrastructurev1.HarnessGitopsProjectMapping{}
	err := f.reconciler.Get(context.Background(), f.key, mapping)
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get mapping: %v", err)
	}
	if err != nil {
		return nil
	}
	return mapping
}

func newMappingControllerAgent(scope string) *infrastructurev1.HarnessGitopsAgent {
	return &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mappingControllerAgentName,
			Namespace: mappingControllerNamespace,
		},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{
			Name:            "Mapping Agent",
			Identifier:      "agent-spec-identifier",
			AccountId:       mappingControllerAccountID,
			OrgId:           mappingControllerAgentOrgID,
			ProjectId:       mappingControllerAgentProject,
			Operator:        "ARGO",
			Type:            "MANAGED_ARGO_PROVIDER",
			Scope:           scope,
			ApiKeySecretRef: mappingControllerAPISecret,
		},
		Status: infrastructurev1.HarnessGitopsAgentStatus{
			AgentIdentifier: mappingControllerAgentID,
			AgentOwnership:  infrastructurev1.OwnershipManaged,
		},
	}
}

func newMappingControllerResource() *infrastructurev1.HarnessGitopsProjectMapping {
	return &infrastructurev1.HarnessGitopsProjectMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments-mapping",
			Namespace:  mappingControllerNamespace,
			Generation: 7,
		},
		Spec: infrastructurev1.HarnessGitopsProjectMappingSpec{
			AgentRef: infrastructurev1.HarnessGitopsAgentReference{
				Name: mappingControllerAgentName,
			},
			AppProject: mappingControllerAppProject,
		},
	}
}

func newAccountMappingControllerTest(
	t *testing.T,
) (
	*infrastructurev1.HarnessGitopsAgent,
	*infrastructurev1.HarnessGitopsProjectMapping,
	harnessapi.ProjectMappingRequest,
) {
	t.Helper()
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping := newMappingControllerResource()
	mapping.Finalizers = []string{harnessProjectMappingFinalizer}
	mapping.Spec.OrgID = mappingControllerTargetOrgID
	mapping.Spec.ProjectID = mappingControllerTargetProject
	return agent, mapping, resolvedRequestForTest(t, agent, mapping)
}

func resolvedRequestForTest(
	t *testing.T,
	agent *infrastructurev1.HarnessGitopsAgent,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
) harnessapi.ProjectMappingRequest {
	t.Helper()
	request, err := resolveProjectMappingRequest(agent, mapping)
	if err != nil {
		t.Fatalf("resolve request: %v", err)
	}
	return request
}

func exactMappingForRequest(
	request harnessapi.ProjectMappingRequest,
	id string,
	agentID string,
) harnessapi.ProjectMapping {
	return harnessapi.ProjectMapping{
		Identifier:           id,
		AgentIdentifier:      agentID,
		AccountIdentifier:    request.AccountIdentifier,
		OrgIdentifier:        request.Mapping.OrgIdentifier,
		ProjectIdentifier:    request.Mapping.ProjectIdentifier,
		ArgoProjectName:      request.ArgoProjectName,
		AutoCreateServiceEnv: request.AutoCreateServiceEnv,
	}
}

func assertReadyCondition(
	t *testing.T,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
	status metav1.ConditionStatus,
	reason string,
) {
	t.Helper()
	condition := apiMeta.FindStatusCondition(mapping.Status.Conditions, projectMappingReadyCondition)
	if condition == nil {
		t.Fatal("Ready condition is missing")
	}
	if condition.Status != status || condition.Reason != reason {
		t.Fatalf(
			"Ready condition = (%s, %s), want (%s, %s)",
			condition.Status,
			condition.Reason,
			status,
			reason,
		)
	}
	if condition.ObservedGeneration != mapping.Generation {
		t.Fatalf(
			"condition observedGeneration = %d, want %d",
			condition.ObservedGeneration,
			mapping.Generation,
		)
	}
}
