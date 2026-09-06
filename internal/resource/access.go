package resource

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

// NamespaceNotAllowed is the condition reason for a rejected management namespace.
const NamespaceNotAllowed = "NamespaceNotAllowed"

// APIKeySecretKey is the input credential field, never a token output field.
const APIKeySecretKey = "api_key"

// NamespacePolicy is platform-owned process configuration, never a field or
// label supplied by a tenant CR. Its zero value deliberately denies everything.
type NamespacePolicy struct {
	namespaces map[string]struct{}
}

// NewNamespacePolicy validates exact namespace names; no wildcard grants exist.
func NewNamespacePolicy(namespaces []string) (NamespacePolicy, error) {
	policy := NamespacePolicy{namespaces: make(map[string]struct{}, len(namespaces))}
	for _, namespace := range namespaces {
		namespace = strings.TrimSpace(namespace)
		if problems := validation.IsDNS1123Label(namespace); len(problems) != 0 {
			return NamespacePolicy{}, fmt.Errorf("invalid managed namespace %q: %s", namespace, strings.Join(problems, "; "))
		}
		policy.namespaces[namespace] = struct{}{}
	}
	return policy, nil
}

// Allows reports whether the platform approved this exact namespace.
func (p NamespacePolicy) Allows(namespace string) bool {
	_, allowed := p.namespaces[namespace]
	return allowed
}

// NamespaceDeniedMessage explains a refusal without exposing credential details.
func NamespaceDeniedMessage(namespace string) string {
	return fmt.Sprintf("Namespace %q is not approved for Harness resource management; no credentials or Harness APIs will be used", namespace)
}

// SessionForAgent is the sole credential-resolution path for both controllers,
// including health, token recovery and finalization. There is no cross-namespace
// override or fallback. Recheck policy here even after the reconcile entry gate.
// endpoint is the platform-configured Harness API gateway; empty selects the
// default gateway. It is never read from the CR or the pod environment.
func SessionForAgent(
	ctx context.Context,
	reader client.Reader,
	policy NamespacePolicy,
	agent *infrastructurev1.HarnessGitopsAgent,
	endpoint string,
) (*harnessapi.Session, error) {
	if agent == nil {
		return nil, fmt.Errorf("HarnessGitopsAgent is required for credential resolution")
	}
	if !policy.Allows(agent.Namespace) {
		return nil, fmt.Errorf("%s", NamespaceDeniedMessage(agent.Namespace))
	}
	name := agent.Spec.ApiKeySecretRef
	if problems := validation.IsDNS1123Subdomain(name); len(problems) != 0 {
		return nil, fmt.Errorf("spec.apiKeySecretRef must be a valid same-namespace Secret name")
	}
	if reader == nil {
		return nil, fmt.Errorf("kubernetes Secret reader is nil")
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{
		Namespace: agent.Namespace,
		Name:      name,
	}, secret); err != nil {
		return nil, err
	}
	apiKey := strings.TrimSpace(string(secret.Data[APIKeySecretKey]))
	if apiKey == "" {
		return nil, k8serrors.NewBadRequest("api_key is missing or empty in the namespace-local Secret")
	}
	return harnessapi.NewSessionWithEndpoint(apiKey, endpoint)
}
