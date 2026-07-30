package agent

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
	reader := newAPIKeyNamespaceTestReader(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiKeyTestSecretName,
			Namespace: apiKeyTestControllerNamespace,
		},
		Data: map[string][]byte{"api_key": []byte("test-api-key")},
	})

	if _, err := SessionForAgent(
		context.Background(),
		reader,
		apiKeyTestControllerNamespace,
		newAPIKeyNamespaceTestAgent(),
	); err != nil {
		t.Fatalf("get Harness client from configured Secret namespace: %v", err)
	}
}

func TestHarnessClientDefaultsAPIKeySecretToAgentNamespace(t *testing.T) {
	reader := newAPIKeyNamespaceTestReader(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiKeyTestSecretName,
			Namespace: apiKeyTestAgentNamespace,
		},
		Data: map[string][]byte{"api_key": []byte("test-api-key")},
	})

	if _, err := SessionForAgent(
		context.Background(),
		reader,
		"",
		newAPIKeyNamespaceTestAgent(),
	); err != nil {
		t.Fatalf("get Harness client from agent Secret namespace: %v", err)
	}
}

func TestConfiguredAPIKeySecretNamespaceDoesNotFallBack(t *testing.T) {
	reader := newAPIKeyNamespaceTestReader(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiKeyTestSecretName,
			Namespace: apiKeyTestAgentNamespace,
		},
		Data: map[string][]byte{"api_key": []byte("test-api-key")},
	})

	_, err := SessionForAgent(
		context.Background(),
		reader,
		apiKeyTestControllerNamespace,
		newAPIKeyNamespaceTestAgent(),
	)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected configured Secret namespace to be authoritative, got %v", err)
	}
}

func newAPIKeyNamespaceTestReader(
	t *testing.T,
	objects ...client.Object,
) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core Kubernetes scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
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
