package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

const (
	harnessAgentCRUIDTag           = "hga_cr_uid"
	agentRegistrationRetryInterval = time.Second
)

var (
	errHarnessAgentAlreadyExists    = errors.New("harness GitOps agent already exists")
	errHarnessAgentOwnershipUnknown = errors.New("harness GitOps agent ownership is not proven")
)

type agentRegistrationOutcome struct {
	identifier   string
	initialToken string
	result       ctrl.Result
	done         bool
}

// reconcileAgentRegistration persists intent before any create and uses an
// exact UID-tagged lookup to recover every ambiguous create window.
func (r *Reconciler) reconcileAgentRegistration(
	ctx context.Context,
	session *harnessapi.Session,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	namespace string,
) (agentRegistrationOutcome, error) {
	if strings.TrimSpace(string(agentCR.UID)) == "" {
		return agentRegistrationOutcome{done: true}, fmt.Errorf(
			"cannot register Harness Agent for %s/%s: Kubernetes resource UID is empty",
			agentCR.Namespace,
			agentCR.Name,
		)
	}

	switch agentCR.Status.CreationState {
	case "":
		if err := r.setAgentCreationState(
			ctx,
			agentCR,
			infrastructurev1.AgentCreationPending,
		); err != nil {
			return agentRegistrationOutcome{done: true}, err
		}
	case infrastructurev1.AgentCreationPending,
		infrastructurev1.AgentCreationOutcomeUnknown:
	default:
		return agentRegistrationOutcome{done: true}, fmt.Errorf(
			"unsupported Agent creation state %q",
			agentCR.Status.CreationState,
		)
	}

	expected := harnessAgentFor(agentCR, agentCR.Spec.Identifier)
	lookup, err := r.harnessAgentAPI().Lookup(ctx, session, expected)
	if err != nil {
		return agentRegistrationOutcome{done: true}, err
	}
	if lookup.Exists {
		if !agentOwnedByCR(agentCR, expected, lookup.Agent) {
			if statusErr := r.setAgentCreationState(
				ctx,
				agentCR,
				infrastructurev1.AgentCreationOutcomeUnknown,
			); statusErr != nil {
				return agentRegistrationOutcome{done: true}, errors.Join(
					agentRegistrationConflict(agentCR),
					statusErr,
				)
			}
			return agentRegistrationOutcome{done: true}, agentRegistrationConflict(agentCR)
		}

		agentCR.Status.AgentIdentifier = strings.TrimSpace(lookup.Agent.Identifier)
		agentCR.Status.AgentOwnership = infrastructurev1.OwnershipManaged
		agentCR.Status.CreationState = ""
		if err := r.Status().Update(ctx, agentCR); err != nil {
			return agentRegistrationOutcome{done: true}, err
		}
		return agentRegistrationOutcome{identifier: agentCR.Status.AgentIdentifier}, nil
	}

	identifier, initialToken, err := r.createHarnessAgent(
		ctx,
		session,
		agentCR,
		namespace,
	)
	if err != nil {
		if errors.Is(err, errHarnessAgentAlreadyExists) ||
			errors.Is(err, ErrAgentCreateOutcomeUnknown) {
			if statusErr := r.setAgentCreationState(
				ctx,
				agentCR,
				infrastructurev1.AgentCreationOutcomeUnknown,
			); statusErr != nil {
				return agentRegistrationOutcome{done: true}, errors.Join(err, statusErr)
			}
			return agentRegistrationOutcome{
				result: ctrl.Result{RequeueAfter: agentRegistrationRetryInterval},
				done:   true,
			}, nil
		}

		if statusErr := r.setAgentCreationState(ctx, agentCR, ""); statusErr != nil {
			return agentRegistrationOutcome{done: true}, errors.Join(err, statusErr)
		}
		return agentRegistrationOutcome{done: true}, err
	}

	agentCR.Status.AgentIdentifier = identifier
	agentCR.Status.AgentOwnership = infrastructurev1.OwnershipManaged
	agentCR.Status.CreationState = ""
	if err := r.Status().Update(ctx, agentCR); err != nil {
		// The API still contains Pending. The next reconcile verifies the
		// UID-tagged Agent before it can issue another successful create.
		return agentRegistrationOutcome{done: true}, err
	}

	return agentRegistrationOutcome{
		identifier:   identifier,
		initialToken: initialToken,
	}, nil
}

func (r *Reconciler) setAgentCreationState(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	state infrastructurev1.AgentCreationState,
) error {
	if agentCR.Status.CreationState == state {
		return nil
	}
	agentCR.Status.CreationState = state
	return r.Status().Update(ctx, agentCR)
}

// createHarnessAgent registers an Agent tagged to this exact Kubernetes
// resource. Conflicts are intentionally resolved by the next lookup.
func (r *Reconciler) createHarnessAgent(
	ctx context.Context,
	session *harnessapi.Session,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	namespace string,
) (string, string, error) {
	uid := strings.TrimSpace(string(agentCR.UID))
	if uid == "" {
		return "", "", fmt.Errorf(
			"cannot create Harness Agent %q: Kubernetes resource UID is empty",
			strings.TrimSpace(agentCR.Spec.Identifier),
		)
	}

	agent := harnessAgentFor(agentCR, agentCR.Spec.Identifier)
	agent.Tags = map[string]string{harnessAgentCRUIDTag: uid}
	result, err := r.harnessAgentAPI().Create(
		ctx,
		session,
		CreateAgentRequest{
			Agent:     agent,
			Namespace: namespace,
		},
	)
	if err != nil {
		if errors.Is(err, ErrAgentAlreadyExists) {
			return "", "", fmt.Errorf(
				"%w: %q; create a replacement HarnessGitopsAgent CR with "+
					"spec.existingAgentIdentifier set to reference the existing Agent",
				errHarnessAgentAlreadyExists,
				strings.TrimSpace(agentCR.Spec.Identifier),
			)
		}
		return "", "", err
	}
	identifier := strings.TrimSpace(result.Identifier)
	if identifier != strings.TrimSpace(agent.Identifier) {
		return "", "", fmt.Errorf(
			"%w: Harness returned Agent identifier %q for requested identifier %q",
			ErrAgentCreateOutcomeUnknown,
			identifier,
			agent.Identifier,
		)
	}
	return identifier, result.InitialToken, nil
}

func (r *Reconciler) lookupAgentOwnedByCR(
	ctx context.Context,
	session *harnessapi.Session,
	agentCR *infrastructurev1.HarnessGitopsAgent,
) (bool, error) {
	expected := harnessAgentFor(agentCR, agentCR.Spec.Identifier)
	lookup, err := r.harnessAgentAPI().Lookup(ctx, session, expected)
	if err != nil {
		return false, err
	}
	return lookup.Exists && agentOwnedByCR(agentCR, expected, lookup.Agent), nil
}

func harnessAgentFor(
	agentCR *infrastructurev1.HarnessGitopsAgent,
	identifier string,
) Agent {
	return Agent{
		Identifier:        strings.TrimSpace(identifier),
		Name:              strings.TrimSpace(agentCR.Spec.Name),
		AccountIdentifier: strings.TrimSpace(agentCR.Spec.AccountId),
		OrgIdentifier: harnessapi.OrgIdentifierForScope(
			agentCR.Spec.Scope,
			agentCR.Spec.OrgId,
		),
		ProjectIdentifier: harnessapi.ProjectIdentifierForScope(
			agentCR.Spec.Scope,
			agentCR.Spec.ProjectId,
		),
		Scope:    strings.TrimSpace(agentCR.Spec.Scope),
		Type:     strings.TrimSpace(agentCR.Spec.Type),
		Operator: strings.TrimSpace(agentCR.Spec.Operator),
	}
}

func agentOwnedByCR(
	agentCR *infrastructurev1.HarnessGitopsAgent,
	expected Agent,
	observed Agent,
) bool {
	uid := strings.TrimSpace(string(agentCR.UID))
	if uid == "" || strings.TrimSpace(observed.Tags[harnessAgentCRUIDTag]) != uid {
		return false
	}
	return agentTupleEqual(expected, observed)
}

func agentTupleEqual(expected, observed Agent) bool {
	return harnessapi.IdentifiersEquivalent(
		expected.Scope,
		observed.Identifier,
		expected.Identifier,
	) &&
		strings.TrimSpace(observed.Name) == strings.TrimSpace(expected.Name) &&
		strings.TrimSpace(observed.AccountIdentifier) == strings.TrimSpace(expected.AccountIdentifier) &&
		strings.TrimSpace(observed.OrgIdentifier) == strings.TrimSpace(expected.OrgIdentifier) &&
		strings.TrimSpace(observed.ProjectIdentifier) == strings.TrimSpace(expected.ProjectIdentifier) &&
		strings.TrimSpace(observed.Scope) == strings.TrimSpace(expected.Scope) &&
		strings.TrimSpace(observed.Type) == strings.TrimSpace(expected.Type) &&
		strings.TrimSpace(observed.Operator) == strings.TrimSpace(expected.Operator)
}

func agentCreationIsUncertain(state infrastructurev1.AgentCreationState) bool {
	return state == infrastructurev1.AgentCreationPending ||
		state == infrastructurev1.AgentCreationOutcomeUnknown
}

func agentRegistrationConflict(
	agentCR *infrastructurev1.HarnessGitopsAgent,
) error {
	return fmt.Errorf(
		"%w: Harness Agent %q does not match this CR's immutable tuple and %s tag; "+
			"create a replacement HarnessGitopsAgent CR with spec.existingAgentIdentifier set",
		errHarnessAgentAlreadyExists,
		strings.TrimSpace(agentCR.Spec.Identifier),
		harnessAgentCRUIDTag,
	)
}
