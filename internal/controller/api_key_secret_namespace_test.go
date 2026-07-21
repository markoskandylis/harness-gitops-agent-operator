package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const (
	apiKeyTestAgentNamespace      = "controller-e2e"
	apiKeyTestControllerNamespace = "hga-system"
	apiKeyTestSecretName          = "harness-api-key-secret"
)

func TestHarnessClientUsesConfiguredAPIKeySecretNamespace(t *testing.T) {
	reconciler := newAPIKeyNamespaceTestReconciler(t, apiKeyTestControllerNamespace, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiKeyTestSecretName,
			Namespace: apiKeyTestControllerNamespace,
		},
		Data: map[string][]byte{"api_key": []byte("test-api-key")},
	})

	if _, err := reconciler.getHarnessClient(context.Background(), newAPIKeyNamespaceTestAgent()); err != nil {
		t.Fatalf("get Harness client from configured Secret namespace: %v", err)
	}
}

func TestHarnessClientDefaultsAPIKeySecretToAgentNamespace(t *testing.T) {
	reconciler := newAPIKeyNamespaceTestReconciler(t, "", &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiKeyTestSecretName,
			Namespace: apiKeyTestAgentNamespace,
		},
		Data: map[string][]byte{"api_key": []byte("test-api-key")},
	})

	if _, err := reconciler.getHarnessClient(context.Background(), newAPIKeyNamespaceTestAgent()); err != nil {
		t.Fatalf("get Harness client from agent Secret namespace: %v", err)
	}
}

func TestConfiguredAPIKeySecretNamespaceDoesNotFallBack(t *testing.T) {
	reconciler := newAPIKeyNamespaceTestReconciler(t, apiKeyTestControllerNamespace, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiKeyTestSecretName,
			Namespace: apiKeyTestAgentNamespace,
		},
		Data: map[string][]byte{"api_key": []byte("test-api-key")},
	})

	_, err := reconciler.getHarnessClient(context.Background(), newAPIKeyNamespaceTestAgent())
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected configured Secret namespace to be authoritative, got %v", err)
	}
}

func newAPIKeyNamespaceTestReconciler(
	t *testing.T,
	secretNamespace string,
	objects ...client.Object,
) *HarnessGitopsAgentReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core Kubernetes scheme: %v", err)
	}
	return &HarnessGitopsAgentReconciler{
		Client:                fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Scheme:                scheme,
		APIKeySecretNamespace: secretNamespace,
	}
}

func newAPIKeyNamespaceTestAgent() *infrastructurev1.HarnessGitopsAgent {
	return &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: apiKeyTestAgentNamespace,
		},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{
			ApiKeySecretRef: apiKeyTestSecretName,
		},
	}
}
