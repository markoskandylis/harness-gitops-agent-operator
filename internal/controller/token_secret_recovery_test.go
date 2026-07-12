package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

func TestReconcileDoesNotShortcutWhenTokenSecretIsMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add API scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	agent := &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "token-recovery",
			Namespace:  "default",
			Finalizers: []string{harnessAgentFinalizer},
		},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{
			Name:            "token-recovery-agent",
			Identifier:      "token-recovery-agent",
			Operator:        "ARGO",
			AccountId:       "account",
			OrgId:           "org",
			ProjectId:       "project",
			Scope:           "PROJECT",
			Type:            "MANAGED_ARGO_PROVIDER",
			ApiKeySecretRef: "missing-api-key",
			TokenSecretRef:  "token-recovery-token",
			ProjectMapping: &infrastructurev1.ProjectMappingSpec{
				ProjectId:  "project",
				AppProject: "argocd-project",
			},
		},
		Status: infrastructurev1.HarnessGitopsAgentStatus{
			AgentIdentifier:      "agent-id",
			ArgoProjectId:        "argocd-project",
			ArgoProjectMappingId: "mapping-id",
		},
	}

	reconciler := &HarnessGitopsAgentReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build(),
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected reconciliation to continue and read the missing API key Secret, got %v", err)
	}
}
