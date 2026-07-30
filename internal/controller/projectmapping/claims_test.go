package projectmapping

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

const mappingClaimRemoteID = "shared-harness-mapping"

func TestProjectMappingClaimConcurrentExternalObservationHasOneWinner(t *testing.T) {
	tests := []struct {
		name      string
		reconcile []string
	}{
		{
			name:      "winner observes first",
			reconcile: []string{"winner", "loser"},
		},
		{
			name:      "loser observes first",
			reconcile: []string{"loser", "winner"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent, winner, request := newProjectMappingClaimObjects(
				t,
				"claim-namespace",
				"external-winner",
				time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
			)
			winner.Spec.AdoptMappingID = ""
			_, loser, loserRequest := newProjectMappingClaimObjects(
				t,
				"claim-namespace",
				"external-loser",
				time.Date(2025, 1, 2, 3, 4, 6, 0, time.UTC),
			)
			loser.Spec.AdoptMappingID = ""
			reconciler := newProjectMappingClaimReconciler(t, agent, winner, loser)
			observed := exactMappingForRequest(
				request,
				mappingClaimRemoteID,
				"account."+mappingControllerAgentID,
			)

			for _, name := range test.reconcile {
				resource := winner
				resourceRequest := request
				if name == "loser" {
					resource = loser
					resourceRequest = loserRequest
				}
				resource = getProjectMappingClaimResource(t, reconciler.Client, resource)
				if _, err := reconciler.reconcileSelectedMapping(
					context.Background(),
					resource,
					resourceRequest,
					observed,
				); err != nil {
					t.Fatalf("reconcile %s: %v", name, err)
				}
			}

			winner = getProjectMappingClaimResource(t, reconciler.Client, winner)
			if winner.Status.Remote == nil ||
				winner.Status.Remote.Ownership != infrastructurev1.OwnershipExternal {
				t.Fatalf("winner ownership = %#v, want External", winner.Status.Remote)
			}
			assertReadyCondition(
				t,
				winner,
				metav1.ConditionTrue,
				projectMappingReasonMappingExternal,
			)

			loser = getProjectMappingClaimResource(t, reconciler.Client, loser)
			if loser.Status.Remote != nil &&
				loser.Status.Remote.Ownership == infrastructurev1.OwnershipExternal {
				t.Fatalf("loser retained External ownership: %#v", loser.Status.Remote)
			}
			assertReadyCondition(
				t,
				loser,
				metav1.ConditionFalse,
				projectMappingReasonOwnershipConflict,
			)
		})
	}
}

func TestProjectMappingWrongAdoptionDoesNotBlockCorrectAdopter(t *testing.T) {
	agent, wrong, request := newProjectMappingClaimObjects(
		t,
		"claim-namespace",
		"wrong-adopter",
		time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
	)
	wrong.Spec.AdoptMappingID = "different-mapping"
	_, correct, correctRequest := newProjectMappingClaimObjects(
		t,
		"claim-namespace",
		"correct-adopter",
		time.Date(2025, 1, 2, 3, 4, 6, 0, time.UTC),
	)
	reconciler := newProjectMappingClaimReconciler(t, agent, wrong, correct)
	observed := exactMappingForRequest(
		request,
		mappingClaimRemoteID,
		"account."+mappingControllerAgentID,
	)

	wrong = getProjectMappingClaimResource(t, reconciler.Client, wrong)
	if _, err := reconciler.reconcileSelectedMapping(
		context.Background(),
		wrong,
		request,
		observed,
	); err != nil {
		t.Fatalf("reconcile wrong adopter: %v", err)
	}
	wrong = getProjectMappingClaimResource(t, reconciler.Client, wrong)
	if wrong.Status.Remote == nil ||
		wrong.Status.Remote.MappingID != mappingClaimRemoteID ||
		wrong.Status.Remote.Ownership != "" {
		t.Fatalf("wrong adopter remote status = %#v, want observed ID without ownership", wrong.Status.Remote)
	}
	assertReadyCondition(
		t,
		wrong,
		metav1.ConditionFalse,
		projectMappingReasonAdoptionFailed,
	)

	correct = getProjectMappingClaimResource(t, reconciler.Client, correct)
	if _, err := reconciler.reconcileSelectedMapping(
		context.Background(),
		correct,
		correctRequest,
		observed,
	); err != nil {
		t.Fatalf("reconcile correct adopter: %v", err)
	}
	correct = getProjectMappingClaimResource(t, reconciler.Client, correct)
	if correct.Status.Remote == nil ||
		correct.Status.Remote.Ownership != infrastructurev1.OwnershipAdopted {
		t.Fatalf("correct adopter ownership = %#v, want Adopted", correct.Status.Remote)
	}
	assertReadyCondition(
		t,
		correct,
		metav1.ConditionTrue,
		projectMappingReasonMappingAdopted,
	)
}

func TestProjectMappingClaimConcurrentAdoptionHasOneDeterministicWinner(t *testing.T) {
	agent, first, request := newProjectMappingClaimObjects(
		t,
		"claim-namespace",
		"first-adopter",
		time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
	)
	_, second, secondRequest := newProjectMappingClaimObjects(
		t,
		"claim-namespace",
		"second-adopter",
		time.Date(2025, 1, 2, 3, 4, 6, 0, time.UTC),
	)
	reconciler := newProjectMappingClaimReconciler(t, agent, first, second)

	firstDecision, err := reconciler.resolveProjectMappingClaim(
		context.Background(),
		first,
		request,
		mappingClaimRemoteID,
	)
	if err != nil {
		t.Fatalf("resolve first claim: %v", err)
	}
	secondDecision, err := reconciler.resolveProjectMappingClaim(
		context.Background(),
		second,
		secondRequest,
		mappingClaimRemoteID,
	)
	if err != nil {
		t.Fatalf("resolve second claim: %v", err)
	}
	if !firstDecision.currentWins || secondDecision.currentWins {
		t.Fatalf(
			"decisions = (first=%t, second=%t), want exactly the first resource",
			firstDecision.currentWins,
			secondDecision.currentWins,
		)
	}

	observed := exactMappingForRequest(
		request,
		mappingClaimRemoteID,
		"account."+mappingControllerAgentID,
	)
	second = getProjectMappingClaimResource(t, reconciler.Client, second)
	if _, err := reconciler.reconcileSelectedMapping(
		context.Background(),
		second,
		secondRequest,
		observed,
	); err != nil {
		t.Fatalf("reconcile losing adopter: %v", err)
	}
	second = getProjectMappingClaimResource(t, reconciler.Client, second)
	if second.Status.Remote != nil && isDeletionOwnership(second.Status.Remote.Ownership) {
		t.Fatalf("loser acquired deletion ownership: %#v", second.Status.Remote)
	}
	assertReadyCondition(
		t,
		second,
		metav1.ConditionFalse,
		projectMappingReasonOwnershipConflict,
	)

	first = getProjectMappingClaimResource(t, reconciler.Client, first)
	if _, err := reconciler.reconcileSelectedMapping(
		context.Background(),
		first,
		request,
		observed,
	); err != nil {
		t.Fatalf("reconcile winning adopter: %v", err)
	}
	first = getProjectMappingClaimResource(t, reconciler.Client, first)
	if first.Status.Remote == nil ||
		first.Status.Remote.Ownership != infrastructurev1.OwnershipAdopted {
		t.Fatalf("winner ownership = %#v, want Adopted", first.Status.Remote)
	}
}

func TestProjectMappingClaimEstablishedOwnerBeatsAdopter(t *testing.T) {
	agent, owner, request := newProjectMappingClaimObjects(
		t,
		"claim-namespace",
		"established-owner",
		time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC),
	)
	owner.Spec.AdoptMappingID = ""
	owner.Status.Remote = remoteStatusForRequest(request)
	owner.Status.Remote.MappingID = mappingClaimRemoteID
	owner.Status.Remote.Ownership = infrastructurev1.OwnershipManaged

	_, adopter, adopterRequest := newProjectMappingClaimObjects(
		t,
		"claim-namespace",
		"later-adopter",
		time.Date(2025, 2, 3, 4, 5, 7, 0, time.UTC),
	)
	reconciler := newProjectMappingClaimReconciler(t, agent, owner, adopter)
	observed := exactMappingForRequest(
		adopterRequest,
		mappingClaimRemoteID,
		"account."+mappingControllerAgentID,
	)

	adopter = getProjectMappingClaimResource(t, reconciler.Client, adopter)
	if _, err := reconciler.reconcileSelectedMapping(
		context.Background(),
		adopter,
		adopterRequest,
		observed,
	); err != nil {
		t.Fatalf("reconcile adopter: %v", err)
	}
	adopter = getProjectMappingClaimResource(t, reconciler.Client, adopter)
	if adopter.Status.Remote != nil && isDeletionOwnership(adopter.Status.Remote.Ownership) {
		t.Fatalf("adopter acquired deletion ownership: %#v", adopter.Status.Remote)
	}
	assertReadyCondition(
		t,
		adopter,
		metav1.ConditionFalse,
		projectMappingReasonOwnershipConflict,
	)
}

func TestProjectMappingClaimPriorityBeatsCreationOrder(t *testing.T) {
	t.Run("owner beats earlier create candidate", func(t *testing.T) {
		agent, owner, request := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"owner",
			time.Date(2025, 2, 3, 4, 5, 8, 0, time.UTC),
		)
		owner.Spec.AdoptMappingID = ""
		owner.Status.Remote = remoteStatusForRequest(request)
		owner.Status.Remote.MappingID = mappingClaimRemoteID
		owner.Status.Remote.Ownership = infrastructurev1.OwnershipAdopted

		_, createCandidate, createRequest := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"create-candidate",
			time.Date(2025, 2, 3, 4, 5, 7, 0, time.UTC),
		)
		createCandidate.Spec.AdoptMappingID = ""
		createCandidate.Status.CreationState = infrastructurev1.MappingCreationPending
		createCandidate.Status.Remote = remoteStatusForRequest(createRequest)
		createCandidate.Status.Remote.MappingID = mappingClaimRemoteID

		reconciler := newProjectMappingClaimReconciler(
			t,
			agent,
			owner,
			createCandidate,
		)
		decision, err := reconciler.resolveProjectMappingClaim(
			context.Background(),
			createCandidate,
			createRequest,
			mappingClaimRemoteID,
		)
		if err != nil {
			t.Fatalf("resolve claim: %v", err)
		}
		if decision.currentWins ||
			decision.winner.resource.Name != owner.Name {
			t.Fatalf("winner = %s, want owner", decision.winner.resource.Name)
		}
	})

	t.Run("create candidate beats earlier adoption request", func(t *testing.T) {
		agent, createCandidate, request := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"create-candidate",
			time.Date(2025, 2, 3, 4, 5, 8, 0, time.UTC),
		)
		createCandidate.Spec.AdoptMappingID = ""
		createCandidate.Status.CreationState = infrastructurev1.MappingCreationOutcomeUnknown
		createCandidate.Status.Remote = remoteStatusForRequest(request)
		createCandidate.Status.Remote.MappingID = mappingClaimRemoteID

		_, adopter, adopterRequest := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"adopter",
			time.Date(2025, 2, 3, 4, 5, 7, 0, time.UTC),
		)
		reconciler := newProjectMappingClaimReconciler(
			t,
			agent,
			createCandidate,
			adopter,
		)
		decision, err := reconciler.resolveProjectMappingClaim(
			context.Background(),
			adopter,
			adopterRequest,
			mappingClaimRemoteID,
		)
		if err != nil {
			t.Fatalf("resolve claim: %v", err)
		}
		if decision.currentWins ||
			decision.winner.resource.Name != createCandidate.Name {
			t.Fatalf("winner = %s, want create-candidate", decision.winner.resource.Name)
		}
	})

	t.Run("external binding beats earlier adoption request", func(t *testing.T) {
		agent, external, request := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"external",
			time.Date(2025, 2, 3, 4, 5, 8, 0, time.UTC),
		)
		external.Spec.AdoptMappingID = ""
		external.Status.Remote = remoteStatusForRequest(request)
		external.Status.Remote.MappingID = mappingClaimRemoteID
		external.Status.Remote.Ownership = infrastructurev1.OwnershipExternal

		_, adopter, adopterRequest := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"adopter",
			time.Date(2025, 2, 3, 4, 5, 7, 0, time.UTC),
		)
		reconciler := newProjectMappingClaimReconciler(t, agent, external, adopter)
		decision, err := reconciler.resolveProjectMappingClaim(
			context.Background(),
			adopter,
			adopterRequest,
			mappingClaimRemoteID,
		)
		if err != nil {
			t.Fatalf("resolve claim: %v", err)
		}
		if decision.currentWins ||
			decision.winner.resource.Name != external.Name {
			t.Fatalf("winner = %s, want external", decision.winner.resource.Name)
		}
	})
}

func TestProjectMappingClaimCompetesAcrossNamespaces(t *testing.T) {
	currentAgent, current, currentRequest := newProjectMappingClaimObjects(
		t,
		"z-current-namespace",
		"current-adopter",
		time.Date(2025, 3, 4, 5, 6, 8, 0, time.UTC),
	)
	otherAgent, other, _ := newProjectMappingClaimObjects(
		t,
		"a-other-namespace",
		"other-adopter",
		time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC),
	)
	reconciler := newProjectMappingClaimReconciler(
		t,
		currentAgent,
		otherAgent,
		current,
		other,
	)
	observed := exactMappingForRequest(
		currentRequest,
		mappingClaimRemoteID,
		"account."+mappingControllerAgentID,
	)

	current = getProjectMappingClaimResource(t, reconciler.Client, current)
	if _, err := reconciler.reconcileSelectedMapping(
		context.Background(),
		current,
		currentRequest,
		observed,
	); err != nil {
		t.Fatalf("reconcile cross-namespace loser: %v", err)
	}
	current = getProjectMappingClaimResource(t, reconciler.Client, current)
	assertReadyCondition(
		t,
		current,
		metav1.ConditionFalse,
		projectMappingReasonOwnershipConflict,
	)
	condition := apiMeta.FindStatusCondition(
		current.Status.Conditions,
		projectMappingReadyCondition,
	)
	if condition == nil ||
		!strings.Contains(condition.Message, "a-other-namespace/other-adopter") {
		t.Fatalf("ownership conflict did not identify cross-namespace winner: %#v", condition)
	}
}

func TestProjectMappingClaimExternalBindingIsExclusive(t *testing.T) {
	t.Run("another CR blocks adoption", func(t *testing.T) {
		agent, external, request := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"external-binding",
			time.Date(2025, 4, 5, 6, 7, 8, 0, time.UTC),
		)
		external.Spec.AdoptMappingID = ""
		external.Status.Remote = remoteStatusForRequest(request)
		external.Status.Remote.MappingID = mappingClaimRemoteID
		external.Status.Remote.Ownership = infrastructurev1.OwnershipExternal

		_, adopter, adopterRequest := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"new-adopter",
			time.Date(2025, 4, 5, 6, 7, 9, 0, time.UTC),
		)
		reconciler := newProjectMappingClaimReconciler(t, agent, external, adopter)
		observed := exactMappingForRequest(
			adopterRequest,
			mappingClaimRemoteID,
			"account."+mappingControllerAgentID,
		)

		adopter = getProjectMappingClaimResource(t, reconciler.Client, adopter)
		if _, err := reconciler.reconcileSelectedMapping(
			context.Background(),
			adopter,
			adopterRequest,
			observed,
		); err != nil {
			t.Fatalf("reconcile adopter: %v", err)
		}
		adopter = getProjectMappingClaimResource(t, reconciler.Client, adopter)
		assertReadyCondition(
			t,
			adopter,
			metav1.ConditionFalse,
			projectMappingReasonOwnershipConflict,
		)
	})

	t.Run("a drifted External tuple still blocks the same ID", func(t *testing.T) {
		agent, external, request := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"external-binding",
			time.Date(2025, 4, 5, 6, 7, 8, 0, time.UTC),
		)
		external.Spec.AdoptMappingID = ""
		external.Status.Remote = remoteStatusForRequest(request)
		external.Status.Remote.MappingID = mappingClaimRemoteID
		external.Status.Remote.Ownership = infrastructurev1.OwnershipExternal
		external.Status.Remote.Target.ProjectID = "out-of-band-project"

		_, adopter, adopterRequest := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"new-adopter",
			time.Date(2025, 4, 5, 6, 7, 9, 0, time.UTC),
		)
		reconciler := newProjectMappingClaimReconciler(t, agent, external, adopter)
		observed := exactMappingForRequest(
			adopterRequest,
			mappingClaimRemoteID,
			"account."+mappingControllerAgentID,
		)

		adopter = getProjectMappingClaimResource(t, reconciler.Client, adopter)
		if _, err := reconciler.reconcileSelectedMapping(
			context.Background(),
			adopter,
			adopterRequest,
			observed,
		); err != nil {
			t.Fatalf("reconcile adopter: %v", err)
		}
		adopter = getProjectMappingClaimResource(t, reconciler.Client, adopter)
		assertReadyCondition(
			t,
			adopter,
			metav1.ConditionFalse,
			projectMappingReasonOwnershipConflict,
		)
	})

	t.Run("the bound CR can adopt", func(t *testing.T) {
		agent, mapping, request := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"external-binding",
			time.Date(2025, 4, 5, 6, 7, 8, 0, time.UTC),
		)
		mapping.Status.Remote = remoteStatusForRequest(request)
		mapping.Status.Remote.MappingID = mappingClaimRemoteID
		mapping.Status.Remote.Ownership = infrastructurev1.OwnershipExternal
		reconciler := newProjectMappingClaimReconciler(t, agent, mapping)
		observed := exactMappingForRequest(
			request,
			mappingClaimRemoteID,
			"account."+mappingControllerAgentID,
		)

		mapping = getProjectMappingClaimResource(t, reconciler.Client, mapping)
		if _, err := reconciler.reconcileSelectedMapping(
			context.Background(),
			mapping,
			request,
			observed,
		); err != nil {
			t.Fatalf("reconcile bound adopter: %v", err)
		}
		mapping = getProjectMappingClaimResource(t, reconciler.Client, mapping)
		if mapping.Status.Remote == nil ||
			mapping.Status.Remote.Ownership != infrastructurev1.OwnershipAdopted {
			t.Fatalf("ownership = %#v, want Adopted", mapping.Status.Remote)
		}
	})
}

func TestProjectMappingClaimReaderErrorsFailClosed(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		agent, mapping, request := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"adopter",
			time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC),
		)
		reconciler := newProjectMappingClaimReconciler(t, agent, mapping)
		readErr := errors.New("claim list unavailable")
		reconciler.APIReader = projectMappingClaimErrorReader{
			Reader:  reconciler.Client,
			listErr: readErr,
		}
		observed := exactMappingForRequest(
			request,
			mappingClaimRemoteID,
			"account."+mappingControllerAgentID,
		)

		mapping = getProjectMappingClaimResource(t, reconciler.Client, mapping)
		if _, err := reconciler.reconcileSelectedMapping(
			context.Background(),
			mapping,
			request,
			observed,
		); !errors.Is(err, readErr) {
			t.Fatalf("reconcile error = %v, want %v", err, readErr)
		}
		mapping = getProjectMappingClaimResource(t, reconciler.Client, mapping)
		if mapping.Status.Remote != nil && isDeletionOwnership(mapping.Status.Remote.Ownership) {
			t.Fatalf("reader failure acquired deletion ownership: %#v", mapping.Status.Remote)
		}
		assertReadyCondition(
			t,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonOwnershipConflict,
		)
	})

	t.Run("candidate Agent get", func(t *testing.T) {
		agent, mapping, request := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"adopter",
			time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC),
		)
		_, other, _ := newProjectMappingClaimObjects(
			t,
			"other-namespace",
			"other-adopter",
			time.Date(2025, 5, 6, 7, 8, 8, 0, time.UTC),
		)
		reconciler := newProjectMappingClaimReconciler(t, agent, mapping, other)
		readErr := errors.New("candidate Agent read unavailable")
		reconciler.APIReader = projectMappingClaimErrorReader{
			Reader:      reconciler.Client,
			agentGetErr: readErr,
		}
		observed := exactMappingForRequest(
			request,
			mappingClaimRemoteID,
			"account."+mappingControllerAgentID,
		)

		mapping = getProjectMappingClaimResource(t, reconciler.Client, mapping)
		if _, err := reconciler.reconcileSelectedMapping(
			context.Background(),
			mapping,
			request,
			observed,
		); !errors.Is(err, readErr) {
			t.Fatalf("reconcile error = %v, want %v", err, readErr)
		}
		mapping = getProjectMappingClaimResource(t, reconciler.Client, mapping)
		if mapping.Status.Remote != nil && isDeletionOwnership(mapping.Status.Remote.Ownership) {
			t.Fatalf("reader failure acquired deletion ownership: %#v", mapping.Status.Remote)
		}
	})
}

func TestProjectMappingClaimInvalidAdoptionCandidateDoesNotUsurp(t *testing.T) {
	t.Run("missing Agent", func(t *testing.T) {
		agent, current, request := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"valid-adopter",
			time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC),
		)
		_, unresolved, _ := newProjectMappingClaimObjects(
			t,
			"missing-agent-namespace",
			"invalid-adopter",
			time.Date(2025, 6, 7, 8, 9, 9, 0, time.UTC),
		)
		reconciler := newProjectMappingClaimReconciler(t, agent, current, unresolved)
		decision, err := reconciler.resolveProjectMappingClaim(
			context.Background(),
			current,
			request,
			mappingClaimRemoteID,
		)
		if err != nil {
			t.Fatalf("resolve claim: %v", err)
		}
		if !decision.currentWins {
			t.Fatalf("unresolved earlier candidate won: %#v", decision.winner.resource)
		}
	})

	t.Run("different desired tuple", func(t *testing.T) {
		agent, current, request := newProjectMappingClaimObjects(
			t,
			"claim-namespace",
			"valid-adopter",
			time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC),
		)
		otherAgent, different, _ := newProjectMappingClaimObjects(
			t,
			"other-namespace",
			"different-adopter",
			time.Date(2025, 6, 7, 8, 9, 9, 0, time.UTC),
		)
		otherAgent.Spec.AccountId = "different-account"
		reconciler := newProjectMappingClaimReconciler(
			t,
			agent,
			otherAgent,
			current,
			different,
		)
		decision, err := reconciler.resolveProjectMappingClaim(
			context.Background(),
			current,
			request,
			mappingClaimRemoteID,
		)
		if err != nil {
			t.Fatalf("resolve claim: %v", err)
		}
		if !decision.currentWins {
			t.Fatalf("different-tuple candidate won: %#v", decision.winner.resource)
		}
	})
}

func TestProjectMappingClaimTieBreakers(t *testing.T) {
	timestamp := metav1.NewTime(time.Date(2025, 7, 8, 9, 10, 11, 0, time.UTC))
	resource := func(namespace string, name string, uid string) projectMappingClaim {
		return projectMappingClaim{
			priority: projectMappingClaimAdoption,
			resource: &infrastructurev1.HarnessGitopsProjectMapping{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:         namespace,
					Name:              name,
					UID:               types.UID(uid),
					CreationTimestamp: timestamp,
				},
			},
		}
	}

	if !projectMappingClaimLess(
		resource("a-namespace", "z-name", "z-uid"),
		resource("b-namespace", "a-name", "a-uid"),
	) {
		t.Fatal("namespace did not break an equal-priority timestamp tie")
	}
	if !projectMappingClaimLess(
		resource("namespace", "a-name", "z-uid"),
		resource("namespace", "b-name", "a-uid"),
	) {
		t.Fatal("name did not break an equal-priority timestamp and namespace tie")
	}
	if !projectMappingClaimLess(
		resource("namespace", "name", "a-uid"),
		resource("namespace", "name", "b-uid"),
	) {
		t.Fatal("UID did not break an otherwise equal claim tie")
	}
}

type projectMappingClaimErrorReader struct {
	client.Reader
	listErr     error
	agentGetErr error
}

func (r projectMappingClaimErrorReader) List(
	ctx context.Context,
	list client.ObjectList,
	options ...client.ListOption,
) error {
	if _, isMappingList := list.(*infrastructurev1.HarnessGitopsProjectMappingList); isMappingList &&
		r.listErr != nil {
		return r.listErr
	}
	return r.Reader.List(ctx, list, options...)
}

func (r projectMappingClaimErrorReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, isAgent := object.(*infrastructurev1.HarnessGitopsAgent); isAgent &&
		r.agentGetErr != nil {
		return r.agentGetErr
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func newProjectMappingClaimObjects(
	t *testing.T,
	namespace string,
	name string,
	created time.Time,
) (
	*infrastructurev1.HarnessGitopsAgent,
	*infrastructurev1.HarnessGitopsProjectMapping,
	harnessapi.ProjectMappingRequest,
) {
	t.Helper()
	agent := newMappingControllerAgent(agentScopeAccount)
	agent.Namespace = namespace

	mapping := newMappingControllerResource()
	mapping.Namespace = namespace
	mapping.Name = name
	mapping.CreationTimestamp = metav1.NewTime(created)
	mapping.UID = types.UID(namespace + "-" + name)
	mapping.Finalizers = []string{harnessProjectMappingFinalizer}
	mapping.Spec.OrgID = mappingControllerTargetOrgID
	mapping.Spec.ProjectID = mappingControllerTargetProject
	mapping.Spec.AdoptMappingID = mappingClaimRemoteID
	return agent, mapping, resolvedRequestForTest(t, agent, mapping)
}

func newProjectMappingClaimReconciler(
	t *testing.T,
	objects ...client.Object,
) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add operator scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&infrastructurev1.HarnessGitopsAgent{},
			&infrastructurev1.HarnessGitopsProjectMapping{},
		).
		WithObjects(objects...).
		Build()
	return &Reconciler{
		Client:                       k8sClient,
		APIReader:                    k8sClient,
		HarnessMappingResyncInterval: time.Second,
	}
}

func getProjectMappingClaimResource(
	t *testing.T,
	reader client.Reader,
	resource *infrastructurev1.HarnessGitopsProjectMapping,
) *infrastructurev1.HarnessGitopsProjectMapping {
	t.Helper()
	current := &infrastructurev1.HarnessGitopsProjectMapping{}
	if err := reader.Get(
		context.Background(),
		client.ObjectKeyFromObject(resource),
		current,
	); err != nil {
		t.Fatalf("get project mapping claim resource: %v", err)
	}
	return current
}
