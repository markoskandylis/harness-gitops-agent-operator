package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
	resourceutil "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource"
)

const (
	harnessAgentFinalizer               = "infrastructure.kandylis.co.uk/finalizer"
	agentMappingDependencyRetryInterval = 10 * time.Second
)

func (r *Reconciler) reconcileDeletion(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	existingAgentIdentifier string,
	existingAgentMode bool,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(agentCR, harnessAgentFinalizer) {
		return ctrl.Result{}, nil
	}

	if result, done, err := r.reconcileMappingDependenciesForDeletion(
		ctx,
		agentCR,
	); done {
		return result, err
	}

	log := logf.FromContext(ctx)
	if existingAgentMode {
		log.Info("Skipping Harness agent delete because existingAgentIdentifier is set", "existingAgentIdentifier", existingAgentIdentifier)
		return ctrl.Result{}, resourceutil.RemoveFinalizer(ctx, r.Client, agentCR, harnessAgentFinalizer)
	}

	var harnessSession *harnessapi.Session
	deleteAuthorized := false
	verifyOwnership := agentCR.Status.AgentOwnership == infrastructurev1.OwnershipManaged ||
		agentCreationIsUncertain(agentCR.Status.CreationState)
	if verifyOwnership {
		var err error
		harnessSession, err = SessionForAgent(
			ctx,
			r.apiReader(),
			r.APIKeySecretNamespace,
			agentCR,
		)
		if err != nil {
			log.Error(err, "Failed to initialize Harness session for Agent ownership verification; retaining finalizer")
			return ctrl.Result{}, err
		}
		deleteAuthorized, err = r.lookupAgentOwnedByCR(ctx, harnessSession, agentCR)
		if err != nil {
			log.Error(err, "Failed to verify Agent ownership; retaining finalizer")
			return ctrl.Result{}, err
		}
	}

	if !deleteAuthorized {
		log.Info(
			"Skipping Harness agent delete because controller ownership is not recorded",
			"agentIdentifier", agentCR.Status.AgentIdentifier,
			"agentOwnership", agentCR.Status.AgentOwnership,
			"creationState", agentCR.Status.CreationState,
		)
		return ctrl.Result{}, resourceutil.RemoveFinalizer(ctx, r.Client, agentCR, harnessAgentFinalizer)
	}

	log.Info("Deleting agent from Harness Platform...")
	if harnessSession == nil {
		var err error
		harnessSession, err = SessionForAgent(
			ctx,
			r.apiReader(),
			r.APIKeySecretNamespace,
			agentCR,
		)
		if err != nil {
			// Keep finalizer until cleanup in Harness succeeds.
			log.Error(err, "Failed to initialize Harness session for delete; retaining finalizer")
			return ctrl.Result{}, err
		}
	}

	agentIdentifier := strings.TrimSpace(agentCR.Spec.Identifier)
	if agentIdentifier == "" {
		agentIdentifier = strings.TrimSpace(agentCR.Status.AgentIdentifier)
	}
	if agentIdentifier == "" {
		return ctrl.Result{}, fmt.Errorf("cannot delete Harness agent: no identifier in status or spec for %s/%s", agentCR.Namespace, agentCR.Name)
	}

	err := r.harnessAgentAPI().Delete(
		ctx,
		harnessSession,
		harnessAgentFor(agentCR, agentIdentifier),
	)
	if err != nil {
		if isAgentNotFound(err) {
			log.Info("Harness agent already absent, proceeding with finalizer removal", "agentIdentifier", agentIdentifier)
		} else {
			if body := harnessapi.ErrorBody(err); body != "" {
				log.Error(err, "Failed to delete agent from Harness",
					"body", body)
			} else {
				log.Error(err, "Failed to delete agent from Harness")
			}
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, resourceutil.RemoveFinalizer(ctx, r.Client, agentCR, harnessAgentFinalizer)
}

// reconcileMappingDependenciesForDeletion starts deletion of every Mapping CR
// that references agent and waits until a fresh API read proves they are gone.
// A true done result means the Agent deletion reconcile must return immediately.
func (r *Reconciler) reconcileMappingDependenciesForDeletion(
	ctx context.Context,
	agent *infrastructurev1.HarnessGitopsAgent,
) (result ctrl.Result, done bool, err error) {
	if r.APIReader == nil {
		return ctrl.Result{}, true, fmt.Errorf(
			"cannot verify Mapping dependencies for Agent %s/%s: APIReader is not configured",
			agent.Namespace,
			agent.Name,
		)
	}

	mappings := &infrastructurev1.HarnessGitopsProjectMappingList{}
	if err := r.APIReader.List(ctx, mappings, client.InNamespace(agent.Namespace)); err != nil {
		return ctrl.Result{}, true, fmt.Errorf(
			"list Mapping dependencies for Agent %s/%s: %w",
			agent.Namespace,
			agent.Name,
			err,
		)
	}

	references := make([]*infrastructurev1.HarnessGitopsProjectMapping, 0)
	for i := range mappings.Items {
		mapping := &mappings.Items[i]
		if strings.TrimSpace(mapping.Spec.AgentRef.Name) == agent.Name {
			references = append(references, mapping)
		}
	}
	if len(references) == 0 {
		return ctrl.Result{}, false, nil
	}

	sort.Slice(references, func(i, j int) bool {
		return references[i].Name < references[j].Name
	})

	names := make([]string, 0, len(references))
	var deleteErr error
	for _, mapping := range references {
		names = append(names, mapping.Name)
		if !mapping.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, mapping); err != nil {
			deleteErr = errors.Join(
				deleteErr,
				fmt.Errorf("delete Mapping %s/%s: %w", mapping.Namespace, mapping.Name, err),
			)
		}
	}

	statusErr := r.setAgentWaitingForMappings(ctx, agent, names)
	return ctrl.Result{RequeueAfter: agentMappingDependencyRetryInterval},
		true,
		errors.Join(deleteErr, statusErr)
}
