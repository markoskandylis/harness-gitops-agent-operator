package agent

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	resourceutil "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource"
)

type forbiddenAgentReader struct{ client.Reader }

func (forbiddenAgentReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	panic("unapproved request attempted an uncached read")
}

func (forbiddenAgentReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	panic("unapproved request attempted an uncached list")
}

func TestUnapprovedAgentHasNoSideEffectsAcrossLifecycle(t *testing.T) {
	for _, phase := range []string{"new", "registration", "health", "recovery", "external", "deletion"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
			current := fixture.getAgent(t)
			if phase == "new" {
				current.Finalizers = nil
			}
			if phase == "external" {
				current.Spec.ExistingAgentIdentifier = "external-agent"
			}
			if err := fixture.client.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			current = fixture.getAgent(t)
			current.Status.AgentIdentifier = current.Spec.Identifier
			current.Status.AgentOwnership = infrastructurev1.OwnershipManaged
			if phase == "registration" {
				current.Status.CreationState = infrastructurev1.AgentCreationOutcomeUnknown
			}
			if err := fixture.client.Status().Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			if phase == "health" {
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: current.Spec.TokenSecretRef, Namespace: current.Namespace},
					Data: map[string][]byte{gitopsAgentTokenSecretKey: []byte("existing-runtime-token")}}
				if err := fixture.client.Create(context.Background(), secret); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "deletion" {
				if err := fixture.client.Delete(context.Background(), current); err != nil {
					t.Fatal(err)
				}
			}
			before := fixture.getAgent(t)
			fixture.reconciler.NamespacePolicy = resourceutil.NamespacePolicy{}
			fixture.reconciler.APIReader = forbiddenAgentReader{}
			for range 2 {
				result, err := fixture.reconciler.Reconcile(context.Background(), ctrlRequestFor(before))
				if err != nil || !result.IsZero() {
					t.Fatalf("namespace denial must not hot-loop: %v, %v", result, err)
				}
			}
			api := fixture.agentAPI
			if api.lookupCalls+api.createCalls+api.resolveCalls+api.readinessCalls+api.deleteCalls != 0 {
				t.Fatal("unapproved request called Harness")
			}
			after := fixture.getAgent(t)
			if !reflect.DeepEqual(before.Finalizers, after.Finalizers) || before.Status.AgentOwnership != after.Status.AgentOwnership ||
				before.Status.AgentIdentifier != after.Status.AgentIdentifier || before.Status.CreationState != after.Status.CreationState {
				t.Fatal("namespace denial changed lifecycle ownership")
			}
			condition := apiMeta.FindStatusCondition(after.Status.Conditions, harnessAgentHealthyCondition)
			if condition == nil || condition.Reason != resourceutil.NamespaceNotAllowed {
				t.Fatal("namespace denial was not reported")
			}
		})
	}
}

func TestMissingLocalKeyDoesNotUseGlobalKeyOrAddFinalizer(t *testing.T) {
	fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
	agent := fixture.getAgent(t)
	agent.Finalizers = nil
	if err := fixture.client.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Name: agent.Spec.ApiKeySecretRef, Namespace: agent.Namespace}
	if err := fixture.client.Get(context.Background(), key, secret); err != nil {
		t.Fatal(err)
	}
	if err := fixture.client.Delete(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	secret.ResourceVersion = ""
	secret.Namespace = "hga-system"
	if err := fixture.client.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.reconciler.Reconcile(context.Background(), ctrlRequestFor(agent))
	if err != nil || result.RequeueAfter != agentHealthFastResync {
		t.Fatalf("missing key should retry without contacting Harness: %v, %v", result, err)
	}
	if len(fixture.getAgent(t).Finalizers) != 0 || fixture.agentAPI.createCalls+fixture.agentAPI.lookupCalls != 0 {
		t.Fatal("missing local key started external lifecycle management")
	}
	// A later local key should unblock the request without restarting the operator.
	secret.ResourceVersion = ""
	secret.Namespace = agent.Namespace
	if err := fixture.client.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.reconciler.Reconcile(context.Background(), ctrlRequestFor(agent)); err != nil {
		t.Fatal(err)
	}
	if len(fixture.getAgent(t).Finalizers) != 1 {
		t.Fatal("available local key did not allow the finalizer pass")
	}
}
