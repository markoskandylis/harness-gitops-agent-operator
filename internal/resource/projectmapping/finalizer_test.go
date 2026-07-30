package projectmapping

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const mappingCleanupID = "mapping-cleanup-id"

func TestProjectMappingFinalizerFastPathsDoNotContactHarness(t *testing.T) {
	t.Run("unresolved reference with no remote intent", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeProject)
		mapping := newDeletingMapping()
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)
		deleteFixtureObject(t, fixture, agent)

		if _, err := fixture.reconcile(t); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertMappingFinalizer(t, fixture, false)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)
	})

	for _, ownership := range []infrastructurev1.ResourceOwnership{
		"",
		infrastructurev1.OwnershipExternal,
	} {
		name := "empty ownership"
		if ownership != "" {
			name = "external ownership"
		}
		t.Run(name, func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeAccount)
			mapping, request := newOwnedDeletingMapping(
				t,
				agent,
				ownership,
			)
			fixture := newMappingReconcilerFixture(t, agent, mapping, false)
			fixture.mappingAPI.listResults = [][]ProjectMapping{{
				exactMappingForRequest(request, mappingCleanupID, "account."+mappingControllerAgentID),
			}}

			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			assertMappingFinalizer(t, fixture, false)
			assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)
		})
	}
}

func TestProjectMappingFinalizerBlocksUncertainCreates(t *testing.T) {
	tests := []struct {
		state     infrastructurev1.MappingCreationState
		ownership infrastructurev1.ResourceOwnership
	}{
		{
			state:     infrastructurev1.MappingCreationPending,
			ownership: infrastructurev1.OwnershipExternal,
		},
		{
			state:     infrastructurev1.MappingCreationOutcomeUnknown,
			ownership: infrastructurev1.OwnershipManaged,
		},
	}

	for _, test := range tests {
		t.Run(string(test.state), func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeAccount)
			mapping, _ := newOwnedDeletingMapping(t, agent, test.ownership)
			mapping.Status.CreationState = test.state
			if test.state == infrastructurev1.MappingCreationPending {
				mapping.Status.Remote.MappingID = ""
			}
			fixture := newMappingReconcilerFixture(t, agent, mapping, false)

			result, err := fixture.reconcile(t)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if result.RequeueAfter != 2*time.Second {
				t.Fatalf("requeueAfter = %s, want 2s", result.RequeueAfter)
			}
			assertMappingFinalizer(t, fixture, true)
			assertReadyCondition(
				t,
				fixture.getMapping(t),
				metav1.ConditionFalse,
				projectMappingReasonCleanupBlocked,
			)
			assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)
		})
	}
}

func TestProjectMappingFinalizerRecoversPendingReturnedIDBeforeDeleting(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping, request := newUncertainDeletingMapping(
		t,
		agent,
		infrastructurev1.MappingCreationPending,
		mappingCleanupID,
		"",
	)
	fixture := newMappingReconcilerFixture(t, agent, mapping, false)
	fixture.mappingAPI.listResults = [][]ProjectMapping{{
		exactMappingForRequest(request, mappingCleanupID, "account."+mappingControllerAgentID),
	}}

	result, err := fixture.reconcile(t)
	if err != nil {
		t.Fatalf("recover reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("ownership recovery did not request an immediate cleanup reconcile")
	}
	assertMappingFinalizer(t, fixture, true)
	current := fixture.getMapping(t)
	if current.Status.CreationState != "" ||
		current.Status.Remote == nil ||
		current.Status.Remote.MappingID != mappingCleanupID ||
		current.Status.Remote.Ownership != infrastructurev1.OwnershipManaged {
		t.Fatalf("recovered status = %#v, want Managed mapping %q", current.Status, mappingCleanupID)
	}
	assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)

	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("cleanup reconcile: %v", err)
	}
	assertMappingFinalizer(t, fixture, false)
	assertMappingCleanupCalls(t, fixture.mappingAPI, 2, 1)
}

func TestProjectMappingFinalizerRemovesWhenPendingReturnedIDIsAbsent(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping, _ := newUncertainDeletingMapping(
		t,
		agent,
		infrastructurev1.MappingCreationPending,
		mappingCleanupID,
		"",
	)
	oldDeletion := metav1.NewTime(
		time.Now().Add(-projectMappingCreateVisibilityGracePeriod - time.Second),
	)
	mapping.DeletionTimestamp = &oldDeletion
	fixture := newMappingReconcilerFixture(t, agent, mapping, false)
	fixture.mappingAPI.listResults = [][]ProjectMapping{nil}

	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertMappingFinalizer(t, fixture, false)
	assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)
}

func TestProjectMappingFinalizerWaitsForPendingCreateVisibility(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping, request := newUncertainDeletingMapping(
		t,
		agent,
		infrastructurev1.MappingCreationPending,
		mappingCleanupID,
		"",
	)
	fixture := newMappingReconcilerFixture(t, agent, mapping, false)
	fixture.mappingAPI.listResults = [][]ProjectMapping{
		nil,
		{
			exactMappingForRequest(
				request,
				mappingCleanupID,
				"account."+mappingControllerAgentID,
			),
		},
	}

	result, err := fixture.reconcile(t)
	if err != nil {
		t.Fatalf("visibility reconcile: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("requeueAfter = %s, want 1s", result.RequeueAfter)
	}
	assertMappingFinalizer(t, fixture, true)
	assertReadyCondition(
		t,
		fixture.getMapping(t),
		metav1.ConditionFalse,
		projectMappingReasonCleanupBlocked,
	)
	assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)

	result, err = fixture.reconcile(t)
	if err != nil {
		t.Fatalf("ownership recovery reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("ownership recovery did not request immediate cleanup")
	}
	current := fixture.getMapping(t)
	if current.Status.CreationState != "" ||
		current.Status.Remote == nil ||
		current.Status.Remote.Ownership != infrastructurev1.OwnershipManaged {
		t.Fatalf("recovered status = %#v, want Managed", current.Status)
	}
	assertMappingFinalizer(t, fixture, true)
	assertMappingCleanupCalls(t, fixture.mappingAPI, 2, 0)

	if _, err := fixture.reconcile(t); err != nil {
		t.Fatalf("cleanup reconcile: %v", err)
	}
	assertMappingFinalizer(t, fixture, false)
	assertMappingCleanupCalls(t, fixture.mappingAPI, 3, 1)
}

func TestProjectMappingFinalizerRecoversExactAdoptionBeforeDeleting(t *testing.T) {
	tests := []struct {
		name       string
		state      infrastructurev1.MappingCreationState
		remembered string
	}{
		{
			name:       "Pending",
			state:      infrastructurev1.MappingCreationPending,
			remembered: mappingCleanupID,
		},
		{
			name:  "OutcomeUnknown",
			state: infrastructurev1.MappingCreationOutcomeUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeAccount)
			mapping, request := newUncertainDeletingMapping(
				t,
				agent,
				test.state,
				test.remembered,
				mappingCleanupID,
			)
			fixture := newMappingReconcilerFixture(t, agent, mapping, false)
			fixture.mappingAPI.listResults = [][]ProjectMapping{{
				exactMappingForRequest(
					request,
					mappingCleanupID,
					"account."+mappingControllerAgentID,
				),
			}}

			result, err := fixture.reconcile(t)
			if err != nil {
				t.Fatalf("recover reconcile: %v", err)
			}
			if result.RequeueAfter <= 0 {
				t.Fatal("adoption recovery did not request an immediate cleanup reconcile")
			}
			assertMappingFinalizer(t, fixture, true)
			current := fixture.getMapping(t)
			if current.Status.CreationState != "" ||
				current.Status.Remote == nil ||
				current.Status.Remote.MappingID != mappingCleanupID ||
				current.Status.Remote.Ownership != infrastructurev1.OwnershipAdopted {
				t.Fatalf("recovered status = %#v, want Adopted mapping %q", current.Status, mappingCleanupID)
			}
			assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)

			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("cleanup reconcile: %v", err)
			}
			assertMappingFinalizer(t, fixture, false)
			assertMappingCleanupCalls(t, fixture.mappingAPI, 2, 1)
		})
	}
}

func TestProjectMappingFinalizerDifferentReturnedCandidateStaysBlockedAndCorrectable(t *testing.T) {
	agent := newMappingControllerAgent(agentScopeAccount)
	mapping, request := newUncertainDeletingMapping(
		t,
		agent,
		infrastructurev1.MappingCreationPending,
		mappingCleanupID,
		"wrong-mapping-id",
	)
	fixture := newMappingReconcilerFixture(t, agent, mapping, false)
	fixture.mappingAPI.listResults = [][]ProjectMapping{{
		exactMappingForRequest(request, mappingCleanupID, "account."+mappingControllerAgentID),
	}}

	result, err := fixture.reconcile(t)
	if err != nil {
		t.Fatalf("wrong adoption reconcile: %v", err)
	}
	if result.RequeueAfter != 2*time.Second {
		t.Fatalf("requeueAfter = %s, want 2s", result.RequeueAfter)
	}
	assertMappingFinalizer(t, fixture, true)
	assertReadyCondition(
		t,
		fixture.getMapping(t),
		metav1.ConditionFalse,
		projectMappingReasonCleanupBlocked,
	)
	assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)

	current := fixture.getMapping(t)
	current.Spec.AdoptMappingID = mappingCleanupID
	if err := fixture.reconciler.Update(context.Background(), current); err != nil {
		t.Fatalf("correct adoptMappingId: %v", err)
	}

	result, err = fixture.reconcile(t)
	if err != nil {
		t.Fatalf("corrected adoption reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("corrected adoption did not request an immediate cleanup reconcile")
	}
	current = fixture.getMapping(t)
	if current.Status.CreationState != "" ||
		current.Status.Remote == nil ||
		current.Status.Remote.Ownership != infrastructurev1.OwnershipAdopted {
		t.Fatalf("corrected adoption status = %#v", current.Status)
	}
	assertMappingFinalizer(t, fixture, true)
	assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)
}

func TestProjectMappingFinalizerAdoptionRequiresExactIDAndTuple(t *testing.T) {
	tests := []struct {
		name      string
		adoptID   string
		first     func(ProjectMappingRequest) ProjectMapping
		correctID bool
	}{
		{
			name:    "missing ID",
			adoptID: "missing-mapping-id",
			first: func(request ProjectMappingRequest) ProjectMapping {
				return exactMappingForRequest(
					request,
					mappingCleanupID,
					"account."+mappingControllerAgentID,
				)
			},
			correctID: true,
		},
		{
			name:    "tuple mismatch",
			adoptID: mappingCleanupID,
			first: func(request ProjectMappingRequest) ProjectMapping {
				observed := exactMappingForRequest(
					request,
					mappingCleanupID,
					"account."+mappingControllerAgentID,
				)
				observed.AutoCreateServiceEnv = !request.AutoCreateServiceEnv
				return observed
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeAccount)
			mapping, request := newUncertainDeletingMapping(
				t,
				agent,
				infrastructurev1.MappingCreationOutcomeUnknown,
				"",
				test.adoptID,
			)
			fixture := newMappingReconcilerFixture(t, agent, mapping, false)
			fixture.mappingAPI.listResults = [][]ProjectMapping{
				{test.first(request)},
				{exactMappingForRequest(
					request,
					mappingCleanupID,
					"account."+mappingControllerAgentID,
				)},
			}

			result, err := fixture.reconcile(t)
			if err != nil {
				t.Fatalf("invalid adoption reconcile: %v", err)
			}
			if result.RequeueAfter != 2*time.Second {
				t.Fatalf("requeueAfter = %s, want 2s", result.RequeueAfter)
			}
			assertMappingFinalizer(t, fixture, true)
			assertReadyCondition(
				t,
				fixture.getMapping(t),
				metav1.ConditionFalse,
				projectMappingReasonCleanupBlocked,
			)
			assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)

			if test.correctID {
				current := fixture.getMapping(t)
				current.Spec.AdoptMappingID = mappingCleanupID
				if err := fixture.reconciler.Update(context.Background(), current); err != nil {
					t.Fatalf("correct adoptMappingId: %v", err)
				}
			}

			result, err = fixture.reconcile(t)
			if err != nil {
				t.Fatalf("corrected adoption reconcile: %v", err)
			}
			if result.RequeueAfter <= 0 {
				t.Fatal("corrected adoption did not request an immediate cleanup reconcile")
			}
			current := fixture.getMapping(t)
			if current.Status.CreationState != "" ||
				current.Status.Remote == nil ||
				current.Status.Remote.Ownership != infrastructurev1.OwnershipAdopted {
				t.Fatalf("corrected adoption status = %#v", current.Status)
			}
			assertMappingFinalizer(t, fixture, true)
			assertMappingCleanupCalls(t, fixture.mappingAPI, 2, 0)
		})
	}
}

func TestProjectMappingFinalizerRetainsUncertainMappingOnRecoveryFailure(t *testing.T) {
	t.Run("Harness List", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, _ := newUncertainDeletingMapping(
			t,
			agent,
			infrastructurev1.MappingCreationPending,
			mappingCleanupID,
			"",
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)
		fixture.mappingAPI.listErr = errors.New("temporary recovery list failure")

		if _, err := fixture.reconcile(t); err == nil {
			t.Fatal("expected recovery List error")
		}
		assertMappingFinalizer(t, fixture, true)
		assertReadyCondition(
			t,
			fixture.getMapping(t),
			metav1.ConditionFalse,
			projectMappingReasonCleanupFailed,
		)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)
	})

	t.Run("status update", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, request := newUncertainDeletingMapping(
			t,
			agent,
			infrastructurev1.MappingCreationOutcomeUnknown,
			"",
			mappingCleanupID,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)
		fixture.mappingAPI.listResults = [][]ProjectMapping{{
			exactMappingForRequest(request, mappingCleanupID, "account."+mappingControllerAgentID),
		}}
		*fixture.statusFailAt = 1
		*fixture.statusFailErr = errors.New("temporary recovery status failure")

		if _, err := fixture.reconcile(t); err == nil {
			t.Fatal("expected recovery status error")
		}
		assertMappingFinalizer(t, fixture, true)
		current := fixture.getMapping(t)
		if current.Status.CreationState != infrastructurev1.MappingCreationOutcomeUnknown ||
			current.Status.Remote == nil ||
			current.Status.Remote.Ownership != "" {
			t.Fatalf("uncertain status was not retained: %#v", current.Status)
		}
		assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)
	})
}

func TestProjectMappingFinalizerDeletesManagedAndAdoptedMappings(t *testing.T) {
	for _, ownership := range []infrastructurev1.ResourceOwnership{
		infrastructurev1.OwnershipManaged,
		infrastructurev1.OwnershipAdopted,
	} {
		t.Run(string(ownership), func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeAccount)
			mapping, storedRequest := newOwnedDeletingMapping(t, agent, ownership)

			// Cleanup identity comes from status. The deleting Agent is retained
			// only as the source of its API-key Secret reference.
			agent.Spec.AccountId = "current-account-must-not-be-used"
			agent.Spec.OrgId = "current-org-must-not-be-used"
			agent.Spec.ProjectId = "current-project-must-not-be-used"
			agent.Spec.Scope = agentScopeProject
			agent.Status.AgentIdentifier = "current-agent-must-not-be-used"
			now := metav1.NewTime(time.Now())
			agent.DeletionTimestamp = &now
			agent.Finalizers = []string{"tests.infrastructure.kandylis.co.uk/agent"}

			fixture := newMappingReconcilerFixture(t, agent, mapping, false)
			fixture.mappingAPI.listResults = [][]ProjectMapping{{
				exactMappingForRequest(
					storedRequest,
					mappingCleanupID,
					"account."+mappingControllerAgentID,
				),
			}}

			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			assertMappingFinalizer(t, fixture, false)
			assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 1)
			if got := fixture.mappingAPI.deleteIDs[0]; got != mappingCleanupID {
				t.Fatalf("deleted mapping ID = %q, want %q", got, mappingCleanupID)
			}
			for index, got := range fixture.mappingAPI.requests {
				if !reflect.DeepEqual(got, storedRequest) {
					t.Fatalf(
						"Harness request %d = %#v, want stored snapshot %#v",
						index,
						got,
						storedRequest,
					)
				}
			}
		})
	}
}

func TestProjectMappingFinalizerArbitratesDuplicateClaims(t *testing.T) {
	for _, test := range []struct {
		name              string
		removeCredentials func(*testing.T, *mappingReconcilerFixture, *infrastructurev1.HarnessGitopsAgent)
	}{
		{
			name: "loser without its Agent",
			removeCredentials: func(
				t *testing.T,
				fixture *mappingReconcilerFixture,
				agent *infrastructurev1.HarnessGitopsAgent,
			) {
				deleteFixtureObject(t, fixture, agent)
			},
		},
		{
			name: "loser without its API-key Secret",
			removeCredentials: func(
				t *testing.T,
				fixture *mappingReconcilerFixture,
				_ *infrastructurev1.HarnessGitopsAgent,
			) {
				deleteFixtureObject(t, fixture, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      mappingControllerAPISecret,
						Namespace: mappingControllerNamespace,
					},
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeAccount)
			mapping, request := newOwnedDeletingMapping(
				t,
				agent,
				infrastructurev1.OwnershipAdopted,
			)
			winner := duplicateMappingClaim(
				t,
				"a-winning-namespace",
				"winner",
				request,
				infrastructurev1.OwnershipManaged,
			)
			fixture := newMappingReconcilerFixture(t, agent, mapping, false, winner)
			test.removeCredentials(t, fixture, agent)

			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile loser: %v", err)
			}
			assertMappingFinalizer(t, fixture, false)
			assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)
		})
	}

	t.Run("cross-namespace loser never deletes", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, request := newOwnedDeletingMapping(
			t,
			agent,
			infrastructurev1.OwnershipAdopted,
		)
		winner := duplicateMappingClaim(
			t,
			"a-winning-namespace",
			"winner",
			request,
			infrastructurev1.OwnershipManaged,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false, winner)
		fixture.mappingAPI.listResults = [][]ProjectMapping{{
			exactMappingForRequest(
				request,
				mappingCleanupID,
				"account."+mappingControllerAgentID,
			),
		}}

		if _, err := fixture.reconcile(t); err != nil {
			t.Fatalf("reconcile loser: %v", err)
		}
		assertMappingFinalizer(t, fixture, false)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)
	})

	t.Run("deterministic winner deletes once", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, request := newOwnedDeletingMapping(
			t,
			agent,
			infrastructurev1.OwnershipManaged,
		)
		loser := duplicateMappingClaim(
			t,
			"z-losing-namespace",
			"loser",
			request,
			infrastructurev1.OwnershipAdopted,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false, loser)
		fixture.mappingAPI.listResults = [][]ProjectMapping{{
			exactMappingForRequest(
				request,
				mappingCleanupID,
				"account."+mappingControllerAgentID,
			),
		}}

		if _, err := fixture.reconcile(t); err != nil {
			t.Fatalf("reconcile winner: %v", err)
		}
		assertMappingFinalizer(t, fixture, false)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 1)
	})

	for _, test := range []struct {
		name    string
		current func(
			*testing.T,
			*infrastructurev1.HarnessGitopsAgent,
		) (*infrastructurev1.HarnessGitopsProjectMapping, ProjectMappingRequest)
	}{
		{
			name: "managed delete",
			current: func(
				t *testing.T,
				agent *infrastructurev1.HarnessGitopsAgent,
			) (*infrastructurev1.HarnessGitopsProjectMapping, ProjectMappingRequest) {
				return newOwnedDeletingMapping(t, agent, infrastructurev1.OwnershipManaged)
			},
		},
		{
			name: "uncertain adoption recovery",
			current: func(
				t *testing.T,
				agent *infrastructurev1.HarnessGitopsAgent,
			) (*infrastructurev1.HarnessGitopsProjectMapping, ProjectMappingRequest) {
				return newUncertainDeletingMapping(
					t,
					agent,
					infrastructurev1.MappingCreationOutcomeUnknown,
					"",
					mappingCleanupID,
				)
			},
		},
	} {
		t.Run("claim appearing during Harness List blocks "+test.name, func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeAccount)
			mapping, request := test.current(t, agent)
			winner := duplicateMappingClaim(
				t,
				"a-winning-namespace",
				"winner",
				request,
				infrastructurev1.OwnershipManaged,
			)
			fixture := newMappingReconcilerFixture(t, agent, mapping, false)
			fixture.mappingAPI.listResults = [][]ProjectMapping{{
				exactMappingForRequest(
					request,
					mappingCleanupID,
					"account."+mappingControllerAgentID,
				),
			}}
			fixture.mappingAPI.onList = func() {
				fixture.mappingAPI.onList = nil
				persistDuplicateMappingClaim(t, fixture, winner)
			}

			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile claim race: %v", err)
			}
			assertMappingFinalizer(t, fixture, false)
			assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)
			current := fixture.getMappingOrNil(t)
			if current != nil &&
				current.Status.Remote != nil &&
				test.name == "uncertain adoption recovery" &&
				isDeletionOwnership(current.Status.Remote.Ownership) {
				t.Fatalf("recovery loser recorded deletion ownership: %#v", current.Status.Remote)
			}
		})
	}

	t.Run("uncertain adoption loser never records ownership", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, request := newUncertainDeletingMapping(
			t,
			agent,
			infrastructurev1.MappingCreationOutcomeUnknown,
			"",
			mappingCleanupID,
		)
		winner := duplicateMappingClaim(
			t,
			"a-winning-namespace",
			"winner",
			request,
			infrastructurev1.OwnershipManaged,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false, winner)
		fixture.mappingAPI.listResults = [][]ProjectMapping{{
			exactMappingForRequest(
				request,
				mappingCleanupID,
				"account."+mappingControllerAgentID,
			),
		}}

		if _, err := fixture.reconcile(t); err != nil {
			t.Fatalf("reconcile recovery loser: %v", err)
		}
		assertMappingFinalizer(t, fixture, false)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)
		current := fixture.getMappingOrNil(t)
		if current != nil &&
			current.Status.Remote != nil &&
			isDeletionOwnership(current.Status.Remote.Ownership) {
			t.Fatalf("loser recorded deletion ownership: %#v", current.Status.Remote)
		}
	})

	t.Run("claim List error retains finalizer", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, request := newOwnedDeletingMapping(
			t,
			agent,
			infrastructurev1.OwnershipManaged,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)
		claimErr := errors.New("claim list unavailable")
		fixture.reconciler.APIReader = projectMappingClaimErrorReader{
			Reader:  fixture.reconciler.APIReader,
			listErr: claimErr,
		}
		fixture.mappingAPI.listResults = [][]ProjectMapping{{
			exactMappingForRequest(
				request,
				mappingCleanupID,
				"account."+mappingControllerAgentID,
			),
		}}

		if _, err := fixture.reconcile(t); !errors.Is(err, claimErr) {
			t.Fatalf("reconcile error = %v, want %v", err, claimErr)
		}
		assertMappingFinalizer(t, fixture, true)
		assertReadyCondition(
			t,
			fixture.getMapping(t),
			metav1.ConditionFalse,
			projectMappingReasonCleanupFailed,
		)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)
	})
}

func TestProjectMappingCleanupRequestPreservesEveryScope(t *testing.T) {
	tests := []struct {
		scope        string
		configureMap func(*infrastructurev1.HarnessGitopsProjectMapping)
	}{
		{scope: agentScopeProject},
		{
			scope: agentScopeOrg,
			configureMap: func(mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				mapping.Spec.ProjectID = mappingControllerTargetProject
			},
		},
		{
			scope: agentScopeAccount,
			configureMap: func(mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				mapping.Spec.OrgID = mappingControllerTargetOrgID
				mapping.Spec.ProjectID = mappingControllerTargetProject
				mapping.Spec.AutoCreateServiceEnv = true
			},
		},
	}

	for _, test := range tests {
		t.Run(test.scope, func(t *testing.T) {
			agent := newMappingControllerAgent(test.scope)
			mapping := newMappingControllerResource()
			if test.configureMap != nil {
				test.configureMap(mapping)
			}
			want := resolvedRequestForTest(t, agent, mapping)
			remote := remoteStatusForRequest(want)
			remote.MappingID = mappingCleanupID
			remote.Ownership = infrastructurev1.OwnershipManaged

			got, gotID, err := projectMappingCleanupRequest(remote)
			if err != nil {
				t.Fatalf("cleanup request: %v", err)
			}
			if gotID != mappingCleanupID {
				t.Fatalf("mapping ID = %q, want %q", gotID, mappingCleanupID)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("cleanup request = %#v, want %#v", got, want)
			}
		})
	}
}

func TestProjectMappingFinalizerRequiresStoredIDAndFullTuple(t *testing.T) {
	t.Run("same tuple with a different ID is not deleted", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, request := newOwnedDeletingMapping(
			t,
			agent,
			infrastructurev1.OwnershipManaged,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)
		fixture.mappingAPI.listResults = [][]ProjectMapping{{
			exactMappingForRequest(request, "replacement-id", "account."+mappingControllerAgentID),
		}}

		if _, err := fixture.reconcile(t); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertMappingFinalizer(t, fixture, false)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)
	})

	mutations := []struct {
		name   string
		mutate func(*ProjectMapping)
	}{
		{
			name: "agent ID",
			mutate: func(mapping *ProjectMapping) {
				mapping.AgentIdentifier = "different-agent"
			},
		},
		{
			name: "account ID",
			mutate: func(mapping *ProjectMapping) {
				mapping.AccountIdentifier = "different-account"
			},
		},
		{
			name: "target organization",
			mutate: func(mapping *ProjectMapping) {
				mapping.OrgIdentifier = "different-org"
			},
		},
		{
			name: "target project",
			mutate: func(mapping *ProjectMapping) {
				mapping.ProjectIdentifier = "different-project"
			},
		},
		{
			name: "AppProject",
			mutate: func(mapping *ProjectMapping) {
				mapping.ArgoProjectName = "different-appproject"
			},
		},
		{
			name: "auto-create option",
			mutate: func(mapping *ProjectMapping) {
				mapping.AutoCreateServiceEnv = !mapping.AutoCreateServiceEnv
			},
		},
	}

	for _, mutation := range mutations {
		t.Run("same ID with different "+mutation.name, func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeAccount)
			mapping, request := newOwnedDeletingMapping(
				t,
				agent,
				infrastructurev1.OwnershipAdopted,
			)
			observed := exactMappingForRequest(
				request,
				mappingCleanupID,
				"account."+mappingControllerAgentID,
			)
			mutation.mutate(&observed)
			fixture := newMappingReconcilerFixture(t, agent, mapping, false)
			fixture.mappingAPI.listResults = [][]ProjectMapping{{observed}}

			if _, err := fixture.reconcile(t); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			assertMappingFinalizer(t, fixture, false)
			assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)
		})
	}
}

func TestProjectMappingFinalizerRetainsOnTransientHarnessFailure(t *testing.T) {
	t.Run("List", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, _ := newOwnedDeletingMapping(
			t,
			agent,
			infrastructurev1.OwnershipManaged,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)
		fixture.mappingAPI.listErr = errors.New("temporary list failure")

		if _, err := fixture.reconcile(t); err == nil {
			t.Fatal("expected list error")
		}
		assertMappingFinalizer(t, fixture, true)
		assertReadyCondition(
			t,
			fixture.getMapping(t),
			metav1.ConditionFalse,
			projectMappingReasonCleanupFailed,
		)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 1, 0)
	})

	t.Run("Delete then retry", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, request := newOwnedDeletingMapping(
			t,
			agent,
			infrastructurev1.OwnershipManaged,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)
		fixture.mappingAPI.listResults = [][]ProjectMapping{{
			exactMappingForRequest(request, mappingCleanupID, "account."+mappingControllerAgentID),
		}}
		fixture.mappingAPI.deleteErr = errors.New("temporary delete failure")

		if _, err := fixture.reconcile(t); err == nil {
			t.Fatal("expected delete error")
		}
		assertMappingFinalizer(t, fixture, true)
		assertReadyCondition(
			t,
			fixture.getMapping(t),
			metav1.ConditionFalse,
			projectMappingReasonCleanupFailed,
		)

		fixture.mappingAPI.deleteErr = nil
		if _, err := fixture.reconcile(t); err != nil {
			t.Fatalf("retry reconcile: %v", err)
		}
		assertMappingFinalizer(t, fixture, false)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 2, 2)
	})
}

func TestProjectMappingFinalizerBlocksWithoutCleanupCredentials(t *testing.T) {
	t.Run("Agent missing", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, _ := newOwnedDeletingMapping(
			t,
			agent,
			infrastructurev1.OwnershipManaged,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)
		deleteFixtureObject(t, fixture, agent)

		if _, err := fixture.reconcile(t); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertMappingFinalizer(t, fixture, true)
		assertReadyCondition(
			t,
			fixture.getMapping(t),
			metav1.ConditionFalse,
			projectMappingReasonCleanupBlocked,
		)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)
	})

	t.Run("API-key Secret missing", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, _ := newOwnedDeletingMapping(
			t,
			agent,
			infrastructurev1.OwnershipAdopted,
		)
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)
		deleteFixtureObject(t, fixture, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mappingControllerAPISecret,
				Namespace: mappingControllerNamespace,
			},
		})

		if _, err := fixture.reconcile(t); err == nil {
			t.Fatal("expected missing Secret error")
		}
		assertMappingFinalizer(t, fixture, true)
		assertReadyCondition(
			t,
			fixture.getMapping(t),
			metav1.ConditionFalse,
			projectMappingReasonCleanupBlocked,
		)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)
	})

	t.Run("owned snapshot is incomplete", func(t *testing.T) {
		agent := newMappingControllerAgent(agentScopeAccount)
		mapping, _ := newOwnedDeletingMapping(
			t,
			agent,
			infrastructurev1.OwnershipManaged,
		)
		mapping.Status.Remote.MappingID = ""
		fixture := newMappingReconcilerFixture(t, agent, mapping, false)

		if _, err := fixture.reconcile(t); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertMappingFinalizer(t, fixture, true)
		assertReadyCondition(
			t,
			fixture.getMapping(t),
			metav1.ConditionFalse,
			projectMappingReasonCleanupBlocked,
		)
		assertMappingCleanupCalls(t, fixture.mappingAPI, 0, 0)
	})
}

func newDeletingMapping() *infrastructurev1.HarnessGitopsProjectMapping {
	mapping := newMappingControllerResource()
	now := metav1.NewTime(time.Now())
	mapping.DeletionTimestamp = &now
	mapping.Finalizers = []string{harnessProjectMappingFinalizer}
	return mapping
}

func newOwnedDeletingMapping(
	t *testing.T,
	agent *infrastructurev1.HarnessGitopsAgent,
	ownership infrastructurev1.ResourceOwnership,
) (*infrastructurev1.HarnessGitopsProjectMapping, ProjectMappingRequest) {
	t.Helper()
	mapping := newDeletingMapping()
	switch agent.Spec.Scope {
	case agentScopeAccount:
		mapping.Spec.OrgID = mappingControllerTargetOrgID
		mapping.Spec.ProjectID = mappingControllerTargetProject
		mapping.Spec.AutoCreateServiceEnv = true
	case agentScopeOrg:
		mapping.Spec.ProjectID = mappingControllerTargetProject
	}
	request := resolvedRequestForTest(t, agent, mapping)
	mapping.Status.Remote = remoteStatusForRequest(request)
	mapping.Status.Remote.MappingID = mappingCleanupID
	mapping.Status.Remote.Ownership = ownership
	return mapping, request
}

func newUncertainDeletingMapping(
	t *testing.T,
	agent *infrastructurev1.HarnessGitopsAgent,
	state infrastructurev1.MappingCreationState,
	rememberedID string,
	adoptID string,
) (*infrastructurev1.HarnessGitopsProjectMapping, ProjectMappingRequest) {
	t.Helper()
	mapping, request := newOwnedDeletingMapping(t, agent, "")
	mapping.Status.CreationState = state
	mapping.Status.Remote.MappingID = rememberedID
	mapping.Status.Remote.Ownership = ""
	mapping.Spec.AdoptMappingID = adoptID
	return mapping, request
}

func duplicateMappingClaim(
	t *testing.T,
	namespace string,
	name string,
	request ProjectMappingRequest,
	ownership infrastructurev1.ResourceOwnership,
) *infrastructurev1.HarnessGitopsProjectMapping {
	t.Helper()
	mapping := newMappingControllerResource()
	mapping.Namespace = namespace
	mapping.Name = name
	mapping.Spec.OrgID = request.Mapping.OrgIdentifier
	mapping.Spec.ProjectID = request.Mapping.ProjectIdentifier
	mapping.Spec.AutoCreateServiceEnv = request.AutoCreateServiceEnv
	mapping.Status.Remote = remoteStatusForRequest(request)
	mapping.Status.Remote.MappingID = mappingCleanupID
	mapping.Status.Remote.Ownership = ownership
	return mapping
}

func persistDuplicateMappingClaim(
	t *testing.T,
	fixture *mappingReconcilerFixture,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
) {
	t.Helper()
	status := mapping.Status.DeepCopy()
	if err := fixture.reconciler.Create(context.Background(), mapping); err != nil {
		t.Fatalf("create duplicate claim: %v", err)
	}
	current := &infrastructurev1.HarnessGitopsProjectMapping{}
	if err := fixture.reconciler.Get(
		context.Background(),
		client.ObjectKeyFromObject(mapping),
		current,
	); err != nil {
		t.Fatalf("get duplicate claim: %v", err)
	}
	current.Status = *status
	if err := fixture.reconciler.Status().Update(context.Background(), current); err != nil {
		t.Fatalf("record duplicate claim status: %v", err)
	}
}

func deleteFixtureObject(
	t *testing.T,
	fixture *mappingReconcilerFixture,
	object client.Object,
) {
	t.Helper()
	if err := fixture.reconciler.Delete(context.Background(), object); err != nil {
		t.Fatalf("delete %T: %v", object, err)
	}
}

func assertMappingFinalizer(
	t *testing.T,
	fixture *mappingReconcilerFixture,
	want bool,
) {
	t.Helper()
	mapping := &infrastructurev1.HarnessGitopsProjectMapping{}
	err := fixture.reconciler.Get(context.Background(), fixture.key, mapping)
	if k8serrors.IsNotFound(err) {
		if want {
			t.Fatal("mapping was deleted while its finalizer should have been retained")
		}
		return
	}
	if err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	if got := controllerutil.ContainsFinalizer(mapping, harnessProjectMappingFinalizer); got != want {
		t.Fatalf("mapping finalizer present = %t, want %t", got, want)
	}
}

func assertMappingCleanupCalls(
	t *testing.T,
	api *fakeProjectMappingReconcileAPI,
	wantList int,
	wantDelete int,
) {
	t.Helper()
	if api.listCalls != wantList || api.deleteCalls != wantDelete || api.createCalls != 0 {
		t.Fatalf(
			"mapping cleanup calls = (List=%d, Create=%d, Delete=%d), want (List=%d, Create=0, Delete=%d)",
			api.listCalls,
			api.createCalls,
			api.deleteCalls,
			wantList,
			wantDelete,
		)
	}
}
