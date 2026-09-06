package projectmapping

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	resourceutil "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource"
)

type forbiddenMappingReader struct{ client.Reader }

func (forbiddenMappingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	panic("unapproved mapping attempted an uncached read")
}

func (forbiddenMappingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	panic("unapproved mapping attempted an uncached list")
}

func TestUnapprovedMappingsDoNotReadKeysCallHarnessOrChangeFinalizers(t *testing.T) {
	for _, phase := range []string{"new", "registered", "deletion"} {
		t.Run(phase, func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeProject)
			mapping, _ := newOwnedDeletingMapping(t, agent, infrastructurev1.OwnershipManaged)
			if phase != "deletion" {
				mapping.DeletionTimestamp = nil
			}
			if phase == "new" {
				mapping.Finalizers = nil
				mapping.Status = infrastructurev1.HarnessGitopsProjectMappingStatus{}
			}
			fixture := newMappingReconcilerFixture(t, agent, mapping, true)
			before := fixture.getMapping(t)
			fixture.reconciler.NamespacePolicy = resourceutil.NamespacePolicy{}
			fixture.reconciler.APIReader = forbiddenMappingReader{}
			for range 2 {
				result, err := fixture.reconcile(t)
				if err != nil || !result.IsZero() {
					t.Fatalf("denied mapping must not hot-loop: %v, %v", result, err)
				}
			}
			api := fixture.mappingAPI
			if api.listCalls+api.createCalls+api.deleteCalls != 0 {
				t.Fatal("unapproved mapping called Harness")
			}
			after := fixture.getMapping(t)
			if !reflect.DeepEqual(before.Finalizers, after.Finalizers) || !reflect.DeepEqual(before.Status.Remote, after.Status.Remote) ||
				before.Status.CreationState != after.Status.CreationState {
				t.Fatal("namespace denial changed finalizers or recorded ownership")
			}
			assertReadyCondition(t, after, metav1.ConditionFalse, resourceutil.NamespaceNotAllowed)
		})
	}
}

func TestMappingDoesNotUseGlobalKeyForReconcileOrDeletion(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "reconcile", true: "deletion"}[deleting], func(t *testing.T) {
			agent := newMappingControllerAgent(agentScopeProject)
			mapping, request := newOwnedDeletingMapping(t, agent, infrastructurev1.OwnershipManaged)
			if !deleting {
				mapping.DeletionTimestamp = nil
			}
			fixture := newMappingReconcilerFixture(t, agent, mapping, true)
			fixture.mappingAPI.listResults = [][]ProjectMapping{{exactMappingForRequest(request, mappingCleanupID, agent.Spec.Identifier)}}
			local := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: mappingControllerAPISecret, Namespace: mappingControllerNamespace}}
			if err := fixture.reconciler.Delete(context.Background(), local); err != nil {
				t.Fatal(err)
			}
			global := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: mappingControllerAPISecret, Namespace: "hga-system"},
				Data: map[string][]byte{"api_key": []byte("global-test-key")}}
			if err := fixture.reconciler.Create(context.Background(), global); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.reconcile(t); err == nil {
				t.Fatal("expected missing local credential error")
			}
			if fixture.mappingAPI.listCalls+fixture.mappingAPI.createCalls+fixture.mappingAPI.deleteCalls != 0 {
				t.Fatal("mapping used a global credential")
			}
			assertMappingFinalizer(t, fixture, true)
		})
	}
}

func TestUnapprovedClaimCannotCompeteButRecordedBindingIsPreserved(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(map[bool]string{false: "unapproved request", true: "revoked recorded owner"}[recorded], func(t *testing.T) {
			currentAgent, current, request := newProjectMappingClaimObjects(t, "approved", "current", time.Unix(20, 0))
			otherAgent, other, otherRequest := newProjectMappingClaimObjects(t, "unapproved", "other", time.Unix(10, 0))
			if recorded {
				other.Status.Remote = remoteStatusForRequest(otherRequest)
				other.Status.Remote.MappingID = mappingClaimRemoteID
				other.Status.Remote.Ownership = infrastructurev1.OwnershipManaged
			}
			reconciler := newProjectMappingClaimReconciler(t, currentAgent, current, otherAgent, other)
			reconciler.NamespacePolicy = mappingPolicyForObjects(t, current)
			decision, err := reconciler.resolveProjectMappingClaim(context.Background(), current, request, mappingClaimRemoteID)
			if recorded {
				if err == nil {
					t.Fatal("revoking approval must not allow takeover of a recorded binding")
				}
			} else if err != nil || !decision.currentWins {
				t.Fatalf("unapproved request influenced authorized claim: %v, %v", decision, err)
			}
		})
	}
}
