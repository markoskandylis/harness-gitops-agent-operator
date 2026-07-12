package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/antihax/optional"
	"github.com/harness/harness-go-sdk/harness/nextgen"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

// createHarnessAgent registers an agent or adopts an already-existing agent.
// It intentionally preserves the current controller behavior; ownership and
// canonical identifier handling are addressed in later changes.
func (r *HarnessGitopsAgentReconciler) createHarnessAgent(
	session *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	namespace string,
) (string, *nextgen.V1AgentCredentials, bool, error) {
	gitopsAgentType := nextgen.V1AgentType(agentCR.Spec.Type)
	gitopsAgentScope := nextgen.V1AgentScope(agentCR.Spec.Scope)
	gitopsOperator := nextgen.V1AgentOperator(agentCR.Spec.Operator)

	createReq := &nextgen.V1Agent{
		Name:              agentCR.Spec.Name,
		Identifier:        agentCR.Spec.Identifier,
		Operator:          &gitopsOperator,
		AccountIdentifier: agentCR.Spec.AccountId,
		OrgIdentifier:     agentCR.Spec.OrgId,
		ProjectIdentifier: projectIdentifierForAgentScope(agentCR.Spec.Scope, agentCR.Spec.ProjectId),
		Scope:             &gitopsAgentScope,
		Type_:             &gitopsAgentType,
		Metadata: &nextgen.V1AgentMetadata{
			Namespace:        namespace,
			HighAvailability: false,
		},
	}

	resp, _, err := session.Client.AgentApi.AgentServiceForServerCreate(session.AuthCtx, *createReq)
	if err != nil {
		if isHarnessAgentAlreadyExists(err) {
			return scopedAgentIdentifier(agentCR.Spec.Scope, agentCR.Spec.Identifier), nil, true, nil
		}
		return "", nil, false, err
	}

	return scopedAgentIdentifier(agentCR.Spec.Scope, resp.Identifier), resp.Credentials, false, nil
}

// deleteHarnessAgent deletes an agent using the current API request contract.
func (r *HarnessGitopsAgentReconciler) deleteHarnessAgent(
	session *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
) error {
	_, _, err := session.Client.AgentApi.AgentServiceForServerDelete(
		session.AuthCtx,
		scopedAgentIdentifier(agentCR.Spec.Scope, agentIdentifier),
		&nextgen.AgentsApiAgentServiceForServerDeleteOpts{
			AccountIdentifier: optional.NewString(agentCR.Spec.AccountId),
			OrgIdentifier:     optionalStr(agentCR.Spec.OrgId),
			ProjectIdentifier: optionalProjectIdentifierForAgentScope(agentCR.Spec.Scope, agentCR.Spec.ProjectId),
			Name:              optional.NewString(agentCR.Spec.Name),
			Type_:             optional.NewString(agentCR.Spec.Type),
			Scope:             optional.NewString(agentCR.Spec.Scope),
		},
	)
	return err
}

// resolveAgentDetails returns the agent token (GITOPS_AGENT_TOKEN),
// falling back to credential regeneration if needed.
func (r *HarnessGitopsAgentReconciler) resolveAgentDetails(
	harnessSession *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
	credentials *nextgen.V1AgentCredentials,
) (agentToken string, err error) {
	// Fast path: creation response already carried the private key.
	if credentials != nil && credentials.PrivateKey != "" {
		agentToken = credentials.PrivateKey
	}

	// Always GET the full agent record to pick up mappedProjects.
	getResp, _, getErr := harnessSession.Client.AgentApi.AgentServiceForServerGet(
		harnessSession.AuthCtx,
		agentIdentifier,
		agentCR.Spec.AccountId,
		&nextgen.AgentsApiAgentServiceForServerGetOpts{
			OrgIdentifier:     optionalStr(agentCR.Spec.OrgId),
			ProjectIdentifier: optionalProjectIdentifierForAgentScope(agentCR.Spec.Scope, agentCR.Spec.ProjectId),
			Scope:             optional.NewString(agentCR.Spec.Scope),
			WithCredentials:   optional.NewBool(true),
		},
	)
	if getErr != nil {
		return "", wrapHarnessAPIError(
			fmt.Sprintf("get agent %q failed", agentIdentifier),
			getErr,
		)
	}

	// Extract token from GET response if not already resolved.
	if agentToken == "" && getResp.Credentials != nil && getResp.Credentials.PrivateKey != "" {
		agentToken = getResp.Credentials.PrivateKey
	}

	// Last resort: regenerate credentials if token still empty.
	if agentToken == "" {
		regenResp, _, regenErr := harnessSession.Client.AgentApi.AgentServiceForServerRegenerateCredentials(
			harnessSession.AuthCtx,
			agentIdentifier,
		)
		if regenErr != nil {
			return "", wrapHarnessAPIError(
				fmt.Sprintf("regenerate credentials for agent %q failed", agentIdentifier),
				regenErr,
			)
		}
		if regenResp.Credentials == nil || regenResp.Credentials.PrivateKey == "" {
			return "", fmt.Errorf("harness API did not return private key for agent %q", agentIdentifier)
		}
		agentToken = regenResp.Credentials.PrivateKey
	}

	return agentToken, nil
}

func (r *HarnessGitopsAgentReconciler) upsertAgentTokenSecret(
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
func (r *HarnessGitopsAgentReconciler) tokenSecretExists(ctx context.Context, agentCR *infrastructurev1.HarnessGitopsAgent, secretName string) bool {
	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: agentCR.Namespace}, existing); err != nil {
		return false
	}
	tok, ok := existing.Data[gitopsAgentTokenSecretKey]
	return ok && len(tok) > 0
}
