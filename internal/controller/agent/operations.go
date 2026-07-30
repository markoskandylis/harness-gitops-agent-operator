package agent

import (
	"context"
	"errors"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

var (
	errHarnessAgentAlreadyExists    = errors.New("harness GitOps agent already exists")
	errHarnessAgentOwnershipUnknown = errors.New("harness GitOps agent ownership is not proven")
)

type agentAPI interface {
	Create(
		context.Context,
		*harnessapi.Session,
		harnessapi.CreateAgentRequest,
	) (harnessapi.CreateAgentResult, error)
	Lookup(
		context.Context,
		*harnessapi.Session,
		harnessapi.Agent,
	) (harnessapi.AgentLookupResult, error)
	Delete(context.Context, *harnessapi.Session, harnessapi.Agent) error
	ResolveToken(context.Context, *harnessapi.Session, harnessapi.Agent, string) (string, error)
}

func (r *Reconciler) harnessAgentAPI() agentAPI {
	if r.agentAPI != nil {
		return r.agentAPI
	}
	return harnessapi.SDKAgentAPI{}
}

// deleteHarnessAgent deletes an agent using the current API request contract.
func (r *Reconciler) deleteHarnessAgent(
	ctx context.Context,
	session *harnessapi.Session,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
) error {
	return r.harnessAgentAPI().Delete(
		ctx,
		session,
		harnessAgentFor(agentCR, agentIdentifier),
	)
}

// resolveAgentDetails returns the agent token (GITOPS_AGENT_TOKEN),
// falling back to credential regeneration if needed.
func (r *Reconciler) resolveAgentDetails(
	ctx context.Context,
	harnessSession *harnessapi.Session,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
	initialToken string,
) (agentToken string, err error) {
	return r.harnessAgentAPI().ResolveToken(
		ctx,
		harnessSession,
		harnessAgentFor(agentCR, agentIdentifier),
		initialToken,
	)
}

func harnessAgentFor(
	agentCR *infrastructurev1.HarnessGitopsAgent,
	identifier string,
) harnessapi.Agent {
	return harnessapi.Agent{
		Identifier:        strings.TrimSpace(identifier),
		Name:              strings.TrimSpace(agentCR.Spec.Name),
		AccountIdentifier: strings.TrimSpace(agentCR.Spec.AccountId),
		OrgIdentifier: harnessapi.OrgIdentifierForAgentScope(
			agentCR.Spec.Scope,
			agentCR.Spec.OrgId,
		),
		ProjectIdentifier: harnessapi.ProjectIdentifierForAgentScope(
			agentCR.Spec.Scope,
			agentCR.Spec.ProjectId,
		),
		Scope:    strings.TrimSpace(agentCR.Spec.Scope),
		Type:     strings.TrimSpace(agentCR.Spec.Type),
		Operator: strings.TrimSpace(agentCR.Spec.Operator),
	}
}

func (r *Reconciler) upsertAgentTokenSecret(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	secretName string,
	agentToken string,
) error {
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: agentCR.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, tokenSecret, func() error {
		if err := ctrl.SetControllerReference(agentCR, tokenSecret, r.Scheme); err != nil {
			return err
		}
		if tokenSecret.Labels == nil {
			tokenSecret.Labels = map[string]string{}
		}
		tokenSecret.Labels[ManagedByLabelKey] = ManagedByLabelValue
		tokenSecret.Type = corev1.SecretTypeOpaque
		if tokenSecret.Data == nil {
			tokenSecret.Data = map[string][]byte{}
		}
		// Consumed by gitops-helm via envFrom(secretRef).
		// Store exactly as returned by the Harness API (base64-encoded PEM).
		tokenSecret.Data[gitopsAgentTokenSecretKey] = []byte(agentToken)
		return nil
	})
	return err
}

// tokenSecretExists returns true if Secret/<secretName> already has GITOPS_AGENT_TOKEN set.
func (r *Reconciler) tokenSecretExists(ctx context.Context, agentCR *infrastructurev1.HarnessGitopsAgent, secretName string) bool {
	existing := &corev1.Secret{}
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: secretName, Namespace: agentCR.Namespace}, existing); err != nil {
		return false
	}
	tok, ok := existing.Data[gitopsAgentTokenSecretKey]
	return ok && len(tok) > 0
}
