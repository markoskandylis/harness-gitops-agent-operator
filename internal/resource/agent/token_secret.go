package agent

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

// tokenSecretKey centralizes the output namespace, default name and input/output
// separation. A token Secret must never adopt the provisioning API-key Secret.
func (r *Reconciler) tokenSecretKey(agent *infrastructurev1.HarnessGitopsAgent) (client.ObjectKey, error) {
	if agent == nil || !r.NamespacePolicy.Allows(agent.Namespace) || agent.UID == "" {
		return client.ObjectKey{}, fmt.Errorf("token Secret requires an Agent UID in an approved namespace")
	}
	name := agent.Spec.TokenSecretRef
	if name == "" {
		name = agent.Name + "-agent-token"
	}
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		return client.ObjectKey{}, fmt.Errorf("token Secret must have a valid same-namespace name")
	}
	if name == agent.Spec.ApiKeySecretRef {
		return client.ObjectKey{}, fmt.Errorf("token Secret must be different from the API-key Secret")
	}
	return client.ObjectKey{Namespace: agent.Namespace, Name: name}, nil
}

// readAgentTokenSecret uses a fresh read, not the label-filtered informer cache.
// Only NotFound means absent; an unreadable or foreign Secret must not trigger
// Harness credential regeneration. Labels alone do not establish ownership.
func (r *Reconciler) readAgentTokenSecret(
	ctx context.Context,
	agent *infrastructurev1.HarnessGitopsAgent,
) (*corev1.Secret, error) {
	key, err := r.tokenSecretKey(agent)
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{}
	if err := r.apiReader().Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read token Secret %s: %w", key, err)
	}
	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.UID != agent.UID || owner.Name != agent.Name ||
		owner.Kind != "HarnessGitopsAgent" || owner.APIVersion != infrastructurev1.GroupVersion.String() {
		return nil, fmt.Errorf("token Secret %s is not controlled by this Agent CR; refusing reuse or adoption", key)
	}
	if !secret.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("token Secret %s is being deleted", key)
	}
	// gitops-helm uses envFrom: other keys must not hitchhike into the runtime.
	for name := range secret.Data {
		if name != gitopsAgentTokenSecretKey {
			return nil, fmt.Errorf("token Secret %s contains non-token data; refusing reuse or mutation", key)
		}
	}
	if secret.Immutable != nil && *secret.Immutable && len(secret.Data[gitopsAgentTokenSecretKey]) == 0 {
		return nil, fmt.Errorf("token Secret %s is empty and immutable", key)
	}
	return secret, nil
}

func (r *Reconciler) upsertAgentTokenSecret(
	ctx context.Context,
	agent *infrastructurev1.HarnessGitopsAgent,
	token string,
) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("refusing to write an empty agent token")
	}
	// Recheck ownership after the Harness call. Kubernetes resourceVersion
	// protects updates if the target is replaced between this read and write.
	secret, err := r.readAgentTokenSecret(ctx, agent)
	if err != nil {
		return err
	}
	if secret != nil {
		if string(secret.Data[gitopsAgentTokenSecretKey]) == token {
			return nil
		}
		secret.Data = map[string][]byte{gitopsAgentTokenSecretKey: []byte(token)}
		return r.Update(ctx, secret)
	}
	key, err := r.tokenSecretKey(agent)
	if err != nil {
		return err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Namespace: key.Namespace,
			Labels: map[string]string{ManagedByLabelKey: ManagedByLabelValue},
		},
		Type: corev1.SecretTypeOpaque,
		// Preserve Harness's base64-encoded PEM exactly for gitops-helm.
		Data: map[string][]byte{gitopsAgentTokenSecretKey: []byte(token)},
	}
	if err := ctrl.SetControllerReference(agent, secret, r.Scheme); err != nil {
		return err
	}
	// A concurrent create is an error, never authority to adopt another Secret.
	return r.Create(ctx, secret)
}
