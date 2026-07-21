package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

func TestProjectScopedAgentWithoutMappingNeedsNoAppProject(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add API scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	agent := &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "project-agent-no-mapping",
			Namespace:  "default",
			Finalizers: []string{harnessAgentFinalizer},
		},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{
			Name:            "project-agent-no-mapping",
			Identifier:      "project_agent_no_mapping",
			Operator:        "ARGO",
			AccountId:       "account",
			OrgId:           "org",
			ProjectId:       "project",
			Scope:           "PROJECT",
			Type:            "MANAGED_ARGO_PROVIDER",
			ApiKeySecretRef: "intentionally-absent-api-key",
			TokenSecretRef:  "project-agent-token",
			ProjectMapping:  nil,
		},
		Status: infrastructurev1.HarnessGitopsAgentStatus{
			AgentIdentifier: "project_agent_no_mapping",
		},
	}
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agent.Spec.TokenSecretRef,
			Namespace: agent.Namespace,
		},
		Data: map[string][]byte{gitopsAgentTokenSecretKey: []byte("token")},
	}

	reconciler := &HarnessGitopsAgentReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&infrastructurev1.HarnessGitopsAgent{}).
			WithObjects(agent, tokenSecret).
			Build(),
		Scheme: scheme,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	})
	if err != nil {
		t.Fatalf("project-scoped agent without mapping should already be ready: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected no requeue for ready agent without mapping, got %#v", result)
	}
}
