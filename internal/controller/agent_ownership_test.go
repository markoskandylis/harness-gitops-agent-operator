package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

func TestCreateHarnessAgentRequiresExplicitAdoption(t *testing.T) {
	session := newSDKMappingTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"message":"agent already exists"}`, http.StatusConflict)
	}))
	agent := newAgentOwnershipTestResource("")

	identifier, credentials, err := (&HarnessGitopsAgentReconciler{}).createHarnessAgent(
		session,
		agent,
		agent.Namespace,
	)
	if !errors.Is(err, errHarnessAgentAlreadyExists) {
		t.Fatalf("expected an explicit-adoption conflict, got %v", err)
	}
	if identifier != "" || credentials != nil {
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
	agent.Status.ArgoProjectId = "app-project"
	agent.Status.ArgoProjectMappingId = "external-mapping"
	agent.Status.ArgoProjectMappingOwnership = infrastructurev1.OwnershipExternal

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
) (*HarnessGitopsAgentReconciler, *infrastructurev1.HarnessGitopsAgent) {
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

	return &HarnessGitopsAgentReconciler{
		Client: k8sClient,
		Scheme: scheme,
	}, fetched
}

func newAgentOwnershipTestResource(
	ownership infrastructurev1.ResourceOwnership,
) *infrastructurev1.HarnessGitopsAgent {
	return &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "ownership-test-agent",
			Namespace:  "default",
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
