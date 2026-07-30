/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil" // REQUIRED for Finalizers
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

const (
	harnessAgentFinalizer         = "infrastructure.kandylis.co.uk/finalizer"
	agentImmediateRequeueInterval = time.Nanosecond
)

const gitopsAgentTokenSecretKey = "GITOPS_AGENT_TOKEN"

// ManagedByLabelKey/ManagedByLabelValue mark the token Secrets this controller
// owns so the manager cache can watch only those Secrets instead of every Secret
// in the cluster (least-privilege + lower memory). The API key Secret is
// user-created and unlabeled, so it is read via the uncached API reader.
const (
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "harness-gitops-agent-operator"
)

// Reconciler reconciles a HarnessGitopsAgent object.
type Reconciler struct {
	client.Client
	Scheme                *runtime.Scheme
	APIReader             client.Reader
	APIKeySecretNamespace string
	agentAPI              agentAPI
}

// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsagents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsagents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsagents/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	agentCR := &infrastructurev1.HarnessGitopsAgent{}
	if err := r.Get(ctx, req.NamespacedName, agentCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	existingAgentIdentifier := strings.TrimSpace(agentCR.Spec.ExistingAgentIdentifier)
	existingAgentMode := existingAgentIdentifier != ""

	if agentCR.GetDeletionTimestamp() != nil {
		return r.reconcileDeletion(ctx, agentCR, existingAgentIdentifier, existingAgentMode)
	}

	if result, done, err := r.ensureFinalizer(ctx, agentCR); done {
		return result, err
	}

	return r.reconcileReady(ctx, req, agentCR, existingAgentIdentifier, existingAgentMode)
}

func (r *Reconciler) ensureFinalizer(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
) (ctrl.Result, bool, error) {
	if controllerutil.ContainsFinalizer(agentCR, harnessAgentFinalizer) {
		return ctrl.Result{}, false, nil
	}

	controllerutil.AddFinalizer(agentCR, harnessAgentFinalizer)
	if err := r.Update(ctx, agentCR); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: agentImmediateRequeueInterval}, true, nil
}

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
		controllerutil.RemoveFinalizer(agentCR, harnessAgentFinalizer)
		return ctrl.Result{}, r.Update(ctx, agentCR)
	}

	var harnessSession *harnessapi.Session
	deleteAuthorized := false
	verifyOwnership := agentCR.Status.AgentOwnership == infrastructurev1.OwnershipManaged ||
		agentCreationIsUncertain(agentCR.Status.CreationState)
	if verifyOwnership {
		var err error
		harnessSession, err = harnessapi.SessionForAgent(
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
		controllerutil.RemoveFinalizer(agentCR, harnessAgentFinalizer)
		return ctrl.Result{}, r.Update(ctx, agentCR)
	}

	log.Info("Deleting agent from Harness Platform...")
	if harnessSession == nil {
		var err error
		harnessSession, err = harnessapi.SessionForAgent(
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

	err := r.deleteHarnessAgent(ctx, harnessSession, agentCR, agentIdentifier)
	if err != nil {
		if harnessapi.IsAgentNotFound(err) {
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

	controllerutil.RemoveFinalizer(agentCR, harnessAgentFinalizer)
	if err := r.Update(ctx, agentCR); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) reconcileReady(
	ctx context.Context,
	req ctrl.Request,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	existingAgentIdentifier string,
	existingAgentMode bool,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if existingAgentMode {
		if agentCR.Status.AgentIdentifier != existingAgentIdentifier ||
			agentCR.Status.AgentOwnership != infrastructurev1.OwnershipExternal {
			agentCR.Status.AgentIdentifier = existingAgentIdentifier
			agentCR.Status.AgentOwnership = infrastructurev1.OwnershipExternal
			if err := r.Status().Update(ctx, agentCR); err != nil {
				return ctrl.Result{}, err
			}
		}
		log.Info("Using existing Harness GitOps Agent", "agentIdentifier", existingAgentIdentifier)
		return ctrl.Result{}, nil
	}

	agentDone := agentCR.Status.AgentIdentifier != ""
	registrationRequired := agentCreationIsUncertain(agentCR.Status.CreationState) ||
		!agentDone
	tokenSecretName := agentCR.Spec.TokenSecretRef
	if tokenSecretName == "" {
		tokenSecretName = agentCR.Name + "-agent-token"
	}
	tokenSecretReady := r.tokenSecretExists(ctx, agentCR, tokenSecretName)

	if agentDone && tokenSecretReady && !registrationRequired {
		return ctrl.Result{}, nil
	}

	// Never recover or regenerate credentials for an agent this CR did not
	// demonstrably create. Empty ownership is intentionally fail-closed.
	if !registrationRequired &&
		agentDone &&
		!tokenSecretReady &&
		agentCR.Status.AgentOwnership != infrastructurev1.OwnershipManaged {
		return ctrl.Result{}, fmt.Errorf(
			"%w for %q; create a replacement HarnessGitopsAgent CR with "+
				"spec.existingAgentIdentifier set to reference the running Agent",
			errHarnessAgentOwnershipUnknown,
			agentIdentifierForStatus(agentCR),
		)
	}

	harnessSession, err := harnessapi.SessionForAgent(
		ctx,
		r.apiReader(),
		r.APIKeySecretNamespace,
		agentCR,
	)
	if err != nil {
		log.Error(err, "Failed to initialize Harness Session")
		return ctrl.Result{}, err
	}

	agentIdentifier := agentCR.Status.AgentIdentifier
	var initialAgentToken string

	if registrationRequired {
		log.Info("Registering new Harness GitOps Agent...", "Name", agentCR.Spec.Name)

		registration, err := r.reconcileAgentRegistration(
			ctx,
			harnessSession,
			agentCR,
			req.Namespace,
		)
		if err != nil {
			log.Error(err, "Harness API Call Failed")
			if body := harnessapi.ErrorBody(err); body != "" {
				log.Error(err, "Harness API Response Body", "body", body)
			}
			return ctrl.Result{}, err
		}
		if registration.done {
			return registration.result, nil
		}
		agentIdentifier = registration.identifier
		initialAgentToken = registration.initialToken
		log.Info("Registered new Harness GitOps Agent", "AgentID", agentIdentifier)
	}

	// Skip if already written to avoid invalidating the running agent.
	if !tokenSecretReady {
		agentToken, err := r.resolveAgentDetails(ctx, harnessSession, agentCR, agentIdentifier, initialAgentToken)
		if err != nil {
			log.Error(err, "Failed to resolve agent token from Harness")
			return ctrl.Result{}, err
		}
		if err := r.upsertAgentTokenSecret(ctx, agentCR, tokenSecretName, agentToken); err != nil {
			log.Error(err, "Failed to create or update token secret", "secret", tokenSecretName)
			return ctrl.Result{}, err
		}
		log.Info("Wrote agent token secret", "secret", tokenSecretName)
	}

	return ctrl.Result{}, nil
}

func agentIdentifierForStatus(agentCR *infrastructurev1.HarnessGitopsAgent) string {
	if identifier := strings.TrimSpace(agentCR.Status.AgentIdentifier); identifier != "" {
		return identifier
	}
	return strings.TrimSpace(agentCR.Spec.Identifier)
}

// apiReader returns a reader that bypasses the manager's label-scoped Secret
// cache so the controller can read the user-created (unlabeled) API key Secret
// and detect pre-existing token Secrets. Falls back to the cached client when
// APIReader is unset (e.g. in unit tests).
func (r *Reconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
