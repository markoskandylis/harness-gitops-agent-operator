package agent

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

type createConflictAgentAPI struct{}

func (createConflictAgentAPI) Create(
	context.Context,
	*harnessapi.Session,
	CreateAgentRequest,
) (CreateAgentResult, error) {
	return CreateAgentResult{}, ErrAgentAlreadyExists
}

func (createConflictAgentAPI) Lookup(
	context.Context,
	*harnessapi.Session,
	Agent,
) (AgentLookupResult, error) {
	return AgentLookupResult{}, nil
}

func (createConflictAgentAPI) Delete(context.Context, *harnessapi.Session, Agent) error {
	return nil
}

func (createConflictAgentAPI) ResolveToken(
	context.Context,
	*harnessapi.Session,
	Agent,
	string,
) (string, error) {
	return "", nil
}

func (createConflictAgentAPI) Readiness(
	context.Context,
	*harnessapi.Session,
	Agent,
) (AgentReadiness, error) {
	return AgentReadiness{}, nil
}

func TestCreateHarnessAgentRequiresExplicitAdoption(t *testing.T) {
	agent := newAgentOwnershipTestResource("")
	reconciler := &Reconciler{agentAPI: createConflictAgentAPI{}}

	identifier, credentials, err := reconciler.createHarnessAgent(
		context.Background(),
		nil,
		agent,
		agent.Namespace,
	)
	if !errors.Is(err, errHarnessAgentAlreadyExists) {
		t.Fatalf("expected an explicit-adoption conflict, got %v", err)
	}
	if identifier != "" || credentials != "" {
		t.Fatalf("an existing agent must not be adopted implicitly: identifier=%q credentials=%#v",
			identifier, credentials)
	}
}

func TestDeletionSkipsAgentWithoutManagedOwnership(t *testing.T) {
	for _, ownership := range []infrastructurev1.ResourceOwnership{
		"",
		infrastructurev1.OwnershipExternal,
	} {
		t.Run(string(ownership), func(t *testing.T) {
			reconciler, agent := newAgentOwnershipTestReconciler(t, ownership)

			if _, err := reconciler.reconcileDeletion(
				context.Background(),
				agent,
				"",
				false,
			); err != nil {
				t.Fatalf("unowned agent finalization should not contact Harness: %v", err)
			}

			updated := &infrastructurev1.HarnessGitopsAgent{}
			if err := reconciler.Get(
				context.Background(),
				client.ObjectKeyFromObject(agent),
				updated,
			); err != nil {
				t.Fatalf("get finalized resource: %v", err)
			}
			for _, finalizer := range updated.Finalizers {
				if finalizer == harnessAgentFinalizer {
					t.Fatal("finalizer was not removed from an unowned agent")
				}
			}
		})
	}
}

func TestDeletionOfExternalAgentDoesNotRequireAPIKey(t *testing.T) {
	reconciler, agent := newAgentOwnershipTestReconciler(
		t,
		infrastructurev1.OwnershipExternal,
	)

	if _, err := reconciler.reconcileDeletion(
		context.Background(),
		agent,
		agent.Status.AgentIdentifier,
		true,
	); err != nil {
		t.Fatalf("external resources should finalize without an API key: %v", err)
	}

	updated := &infrastructurev1.HarnessGitopsAgent{}
	if err := reconciler.Get(
		context.Background(),
		client.ObjectKeyFromObject(agent),
		updated,
	); err != nil {
		t.Fatalf("get finalized resource: %v", err)
	}
	if len(updated.Finalizers) != 0 {
		t.Fatalf("external resource retained finalizers: %v", updated.Finalizers)
	}
}

func TestExistingAgentRecordsExternalOwnershipWithoutAPIKey(t *testing.T) {
	reconciler, agent := newAgentOwnershipTestReconciler(t, "")
	agent.Spec.ExistingAgentIdentifier = "existing-agent"
	agent.Status.AgentIdentifier = ""

	if err := reconciler.Update(context.Background(), agent); err != nil {
		t.Fatalf("update existing agent spec: %v", err)
	}
	if err := reconciler.Status().Update(context.Background(), agent); err != nil {
		t.Fatalf("clear existing agent status: %v", err)
	}

	if _, err := reconciler.Reconcile(
		context.Background(),
		ctrlRequestFor(agent),
	); err != nil {
		t.Fatalf("existing agent should not require an API key: %v", err)
	}

	updated := &infrastructurev1.HarnessGitopsAgent{}
	if err := reconciler.Get(
		context.Background(),
		client.ObjectKeyFromObject(agent),
		updated,
	); err != nil {
		t.Fatalf("get existing agent: %v", err)
	}
	if updated.Status.AgentIdentifier != "existing-agent" {
		t.Fatalf("agent identifier = %q, want existing-agent", updated.Status.AgentIdentifier)
	}
	if updated.Status.AgentOwnership != infrastructurev1.OwnershipExternal {
		t.Fatalf("agent ownership = %q, want External", updated.Status.AgentOwnership)
	}
}

func TestDeletionRetainsFinalizerForManagedAgentWhenCleanupCannotStart(t *testing.T) {
	reconciler, agent := newAgentOwnershipTestReconciler(
		t,
		infrastructurev1.OwnershipManaged,
	)

	if _, err := reconciler.reconcileDeletion(
		context.Background(),
		agent,
		"",
		false,
	); err == nil {
		t.Fatal("managed-agent deletion should fail when the API key Secret is unavailable")
	}

	updated := &infrastructurev1.HarnessGitopsAgent{}
	if err := reconciler.Get(
		context.Background(),
		client.ObjectKeyFromObject(agent),
		updated,
	); err != nil {
		t.Fatalf("get resource after failed cleanup: %v", err)
	}
	found := false
	for _, finalizer := range updated.Finalizers {
		if finalizer == harnessAgentFinalizer {
			found = true
		}
	}
	if !found {
		t.Fatal("managed-agent finalizer was removed before remote cleanup succeeded")
	}
}

func TestCredentialRecoveryRequiresManagedOwnership(t *testing.T) {
	for _, ownership := range []infrastructurev1.ResourceOwnership{
		"",
		infrastructurev1.OwnershipExternal,
	} {
		t.Run(string(ownership), func(t *testing.T) {
			reconciler, agent := newAgentOwnershipTestReconciler(t, ownership)

			_, err := reconciler.Reconcile(
				context.Background(),
				ctrlRequestFor(agent),
			)
			if !errors.Is(err, errHarnessAgentOwnershipUnknown) {
				t.Fatalf("expected credential recovery to be blocked by ownership, got %v", err)
			}
		})
	}
}

func TestCredentialRecoveryRemainsEnabledForManagedAgent(t *testing.T) {
	reconciler, agent := newAgentOwnershipTestReconciler(
		t,
		infrastructurev1.OwnershipManaged,
	)

	_, err := reconciler.Reconcile(context.Background(), ctrlRequestFor(agent))
	if err == nil {
		t.Fatal("expected missing API key Secret to stop managed credential recovery")
	}
	if errors.Is(err, errHarnessAgentOwnershipUnknown) {
		t.Fatalf("managed agent was incorrectly blocked by the ownership guard: %v", err)
	}
}

func ctrlRequestFor(agent *infrastructurev1.HarnessGitopsAgent) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
}

func newAgentOwnershipTestReconciler(
	t *testing.T,
	ownership infrastructurev1.ResourceOwnership,
) (*Reconciler, *infrastructurev1.HarnessGitopsAgent) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add HarnessGitopsAgent scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}

	agent := newAgentOwnershipTestResource(ownership)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrastructurev1.HarnessGitopsAgent{}).
		WithObjects(agent).
		Build()

	fetched := &infrastructurev1.HarnessGitopsAgent{}
	if err := k8sClient.Get(
		context.Background(),
		client.ObjectKeyFromObject(agent),
		fetched,
	); err != nil {
		t.Fatalf("get test agent: %v", err)
	}

	return &Reconciler{
		Client:    k8sClient,
		APIReader: k8sClient,
		Scheme:    scheme,
	}, fetched
}

func newAgentOwnershipTestResource(
	ownership infrastructurev1.ResourceOwnership,
) *infrastructurev1.HarnessGitopsAgent {
	return &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "ownership-test-agent",
			Namespace:  "default",
			UID:        "ownership-test-uid",
			Finalizers: []string{harnessAgentFinalizer},
		},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{
			Name:            "ownership-test-agent",
			Identifier:      "ownership_test_agent",
			Operator:        "ARGO",
			AccountId:       "account",
			OrgId:           "org",
			ProjectId:       "project",
			Scope:           "PROJECT",
			Type:            "MANAGED_ARGO_PROVIDER",
			ApiKeySecretRef: "missing-api-key",
			TokenSecretRef:  "agent-token",
		},
		Status: infrastructurev1.HarnessGitopsAgentStatus{
			AgentIdentifier: "ownership_test_agent",
			AgentOwnership:  ownership,
		},
	}
}
