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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	// 1. KUBERNETES IMPORTS
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil" // REQUIRED for Finalizers
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	// 2. HARNESS SDK IMPORTS
	"github.com/antihax/optional"
	"github.com/harness/harness-go-sdk/harness/nextgen"

	// 3. YOUR API DEFINITION
	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const harnessAgentFinalizer = "infrastructure.kandylis.co.uk/finalizer"

const gitopsAgentTokenSecretKey = "GITOPS_AGENT_TOKEN"

// ManagedByLabelKey/ManagedByLabelValue mark the token Secrets this controller
// owns so the manager cache can watch only those Secrets instead of every Secret
// in the cluster (least-privilege + lower memory). The API key Secret is
// user-created and unlabeled, so it is read via the uncached API reader.
const (
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "harness-gitops-agent-operator"
)

// HarnessGitopsAgentReconciler reconciles a HarnessGitopsAgent object
type HarnessGitopsAgentReconciler struct {
	client.Client
	Scheme                         *runtime.Scheme
	APIReader                      client.Reader
	APIKeySecretNamespace          string
	AppProjectPendingRetryInterval time.Duration
	HarnessMappingResyncInterval   time.Duration
	mappingAPI                     appProjectMappingAPI
	agentReadinessChecker          agentReadinessChecker
	appProjectClient               dynamic.Interface
}

// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsagents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsagents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsagents/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=argoproj.io,resources=appprojects,verbs=get

// HarnessSession contains the client and authentication context for Harness API calls
type HarnessSession struct {
	Client  *nextgen.APIClient
	AuthCtx context.Context
}

func (r *HarnessGitopsAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	agentCR := &infrastructurev1.HarnessGitopsAgent{}
	if err := r.Get(ctx, req.NamespacedName, agentCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	existingAgentIdentifier := strings.TrimSpace(agentCR.Spec.ExistingAgentIdentifier)
	existingAgentMode := existingAgentIdentifier != ""

	// Deletion is handled before validation on purpose. A CR whose spec cannot
	// pass validation must still be able to drop its finalizer, otherwise an
	// invalid mapping strands the resource forever. The target is best effort
	// here: nil simply means there is no mapping to clean up.
	if agentCR.GetDeletionTimestamp() != nil {
		target, err := projectMappingDetails(agentCR)
		if err != nil {
			// Deliberately not fatal, but never silent: in existing-agent mode a
			// nil target skips mapping cleanup, which can orphan a live mapping.
			logf.FromContext(ctx).Error(err,
				"Proceeding with deletion despite an invalid spec.projectMapping; skipping mapping cleanup")
		}
		return r.reconcileDeletion(ctx, agentCR, existingAgentIdentifier, existingAgentMode, target)
	}

	target, err := projectMappingDetails(agentCR)
	if err != nil {
		// Terminal: only a spec edit can fix this, and an edit produces a fresh
		// reconcile. Returning the error instead would spin on backoff forever
		// and report the failure only in the logs.
		if condErr := r.setMappingCondition(ctx, agentCR, mappingReasonInvalidProjectMapping, err.Error()); condErr != nil {
			return ctrl.Result{}, condErr
		}
		logf.FromContext(ctx).Error(err, "Invalid spec.projectMapping; not contacting Harness")
		return ctrl.Result{}, nil
	}

	if result, done, err := r.ensureFinalizer(ctx, agentCR); done {
		return result, err
	}

	return r.reconcileReady(ctx, req, agentCR, existingAgentIdentifier, existingAgentMode, target)
}

// projectMappingTarget is the validated projectMapping input. OrgID is the org
// that owns ProjectID -- a property of the mapping, not of the agent, because an
// ACCOUNT-scoped agent has no org of its own.
type projectMappingTarget struct {
	OrgID      string
	ProjectID  string
	AppProject string
}

func projectMappingDetails(agentCR *infrastructurev1.HarnessGitopsAgent) (*projectMappingTarget, error) {
	mappingSpec := agentCR.Spec.ProjectMapping
	if mappingSpec == nil {
		return nil, nil
	}

	scope := strings.TrimSpace(agentCR.Spec.Scope)
	if !strings.EqualFold(scope, "ORG") &&
		!strings.EqualFold(scope, "ACCOUNT") &&
		!strings.EqualFold(scope, "PROJECT") {
		return nil, fmt.Errorf("spec.projectMapping is only supported for ACCOUNT, ORG, or PROJECT scope")
	}

	// The mapping org prefers the per-mapping projectMapping.orgId and falls
	// back to the agent's own org, so ORG- and PROJECT-scoped CRs that never
	// set projectMapping.orgId behave exactly as before.
	orgID := strings.TrimSpace(mappingSpec.OrgId)
	if orgID == "" {
		orgID = strings.TrimSpace(agentCR.Spec.OrgId)
	}

	target := &projectMappingTarget{
		OrgID:      orgID,
		ProjectID:  strings.TrimSpace(mappingSpec.ProjectId),
		AppProject: strings.TrimSpace(mappingSpec.AppProject),
	}
	if target.ProjectID == "" || target.AppProject == "" {
		return nil, fmt.Errorf("spec.projectMapping.projectId and spec.projectMapping.AppProject are both required when projectMapping is set")
	}
	// An unresolvable project reference is worse than no mapping at all: the
	// AppProject is still created on the cluster, so it looks like it worked.
	// Fail here rather than sending {orgIdentifier: "", projectIdentifier: X},
	// which Harness silently maps to nothing.
	if target.OrgID == "" {
		if strings.EqualFold(scope, "ACCOUNT") {
			return nil, fmt.Errorf(
				"spec.projectMapping.orgId is required: an ACCOUNT-scoped agent has no org of its own, "+
					"so the org that owns spec.projectMapping.projectId %q cannot be inferred",
				target.ProjectID,
			)
		}
		return nil, fmt.Errorf(
			"spec.orgId is required: a %s-scoped agent supplies the org that owns "+
				"spec.projectMapping.projectId %q from its own org, and spec.projectMapping.orgId is not set either",
			strings.ToUpper(scope),
			target.ProjectID,
		)
	}
	return target, nil
}

func (r *HarnessGitopsAgentReconciler) ensureFinalizer(
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
	return ctrl.Result{Requeue: true}, true, nil
}

func (r *HarnessGitopsAgentReconciler) reconcileDeletion(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	existingAgentIdentifier string,
	existingAgentMode bool,
	target *projectMappingTarget,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(agentCR, harnessAgentFinalizer) {
		return ctrl.Result{}, nil
	}

	log := logf.FromContext(ctx)
	if existingAgentMode {
		// Do not delete shared agents. Best effort cleanup for mapping created by this CR.
		if target != nil {
			harnessSession, err := r.getHarnessClient(ctx, agentCR)
			if err != nil {
				log.Error(err, "Failed to initialize Harness session for mapping delete; retaining finalizer")
				return ctrl.Result{}, err
			}
			if err := r.deleteAppProjectMapping(
				ctx,
				harnessSession,
				agentCR,
				existingAgentIdentifier,
				target,
			); err != nil {
				log.Error(err, "Failed to delete AppProject mapping; retaining finalizer")
				return ctrl.Result{}, err
			}
		}

		log.Info("Skipping Harness agent delete because existingAgentIdentifier is set", "existingAgentIdentifier", existingAgentIdentifier)
		controllerutil.RemoveFinalizer(agentCR, harnessAgentFinalizer)
		return ctrl.Result{}, r.Update(ctx, agentCR)
	}

	log.Info("Deleting agent from Harness Platform...")
	harnessSession, err := r.getHarnessClient(ctx, agentCR)
	if err != nil {
		// Keep finalizer until cleanup in Harness succeeds.
		log.Error(err, "Failed to initialize Harness session for delete; retaining finalizer")
		return ctrl.Result{}, err
	}

	agentIdentifier := agentCR.Status.AgentIdentifier
	if agentIdentifier == "" {
		// Fallback handles cases where status was never written.
		agentIdentifier = agentCR.Spec.Identifier
	}
	if agentIdentifier == "" {
		return ctrl.Result{}, fmt.Errorf("cannot delete Harness agent: no identifier in status or spec for %s/%s", agentCR.Namespace, agentCR.Name)
	}

	err = r.deleteHarnessAgent(harnessSession, agentCR, agentIdentifier)
	if err != nil {
		if isHarnessAgentNotFound(err) {
			log.Info("Harness agent already absent, proceeding with finalizer removal", "agentIdentifier", agentIdentifier)
		} else {
			if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
				log.Error(err, "Failed to delete agent from Harness",
					"body", string(swaggerErr.Body()))
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

func (r *HarnessGitopsAgentReconciler) reconcileReady(
	ctx context.Context,
	req ctrl.Request,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	existingAgentIdentifier string,
	existingAgentMode bool,
	target *projectMappingTarget,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	needsMapping := target != nil

	agentDone := agentCR.Status.AgentIdentifier != ""
	tokenSecretName := agentCR.Spec.TokenSecretRef
	if tokenSecretName == "" {
		tokenSecretName = agentCR.Name + "-agent-token"
	}
	tokenSecretReady := existingAgentMode || r.tokenSecretExists(ctx, agentCR, tokenSecretName)

	// Mapping-enabled resources must verify Harness state on every event or
	// periodic resync. Local status fields are observations, not completion guards.
	if agentDone && !needsMapping && tokenSecretReady {
		return ctrl.Result{}, nil
	}

	harnessSession, err := r.getHarnessClient(ctx, agentCR)
	if err != nil {
		log.Error(err, "Failed to initialize Harness Session")
		return ctrl.Result{}, err
	}

	agentIdentifier := agentCR.Status.AgentIdentifier
	var agentCredentials *nextgen.V1AgentCredentials

	if existingAgentMode {
		agentIdentifier = existingAgentIdentifier
		if agentCR.Status.AgentIdentifier == "" {
			agentCR.Status.AgentIdentifier = agentIdentifier
			if err := r.Status().Update(ctx, agentCR); err != nil {
				return ctrl.Result{}, err
			}
		}
		log.Info("Using existing Harness GitOps Agent for AppProject mapping", "agentIdentifier", agentIdentifier)
	} else if agentIdentifier == "" {
		log.Info("Registering new Harness GitOps Agent...", "Name", agentCR.Spec.Name)

		var alreadyExists bool
		agentIdentifier, agentCredentials, alreadyExists, err = r.createHarnessAgent(harnessSession, agentCR, req.Namespace)
		if err != nil {
			log.Error(err, "Harness API Call Failed")
			if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
				log.Error(err, "Harness API Response Body", "body", string(swaggerErr.Body()))
			}
			return ctrl.Result{}, err
		}
		if alreadyExists {
			log.Info("Harness GitOps Agent already exists; continuing with existing identifier", "AgentID", agentIdentifier)
		} else {
			log.Info("Registered new Harness GitOps Agent", "AgentID", agentIdentifier)
		}

		agentCR.Status.AgentIdentifier = agentIdentifier
		if err := r.Status().Update(ctx, agentCR); err != nil {
			return ctrl.Result{}, err
		}
	}

	if agentCR.Spec.ExistingAgentIdentifier == "" {
		// Skip if already written to avoid invalidating the running agent.
		if !tokenSecretReady {
			agentToken, err := r.resolveAgentDetails(harnessSession, agentCR, agentIdentifier, agentCredentials)
			if err != nil {
				log.Error(err, "Failed to resolve agent token from Harness")
				return ctrl.Result{}, err
			}
			if err := r.upsertAgentTokenSecret(ctx, agentCR, tokenSecretName, agentToken); err != nil {
				log.Error(err, "Failed to create or update token secret", "secret", tokenSecretName)
				return ctrl.Result{}, err
			}
			// Re-read the Secret before mapping so Kubernetes, not local flow state,
			// is the source of truth for the token prerequisite.
			tokenSecretReady = r.tokenSecretExists(ctx, agentCR, tokenSecretName)
			log.Info("Wrote agent token secret", "secret", tokenSecretName)
		}
	}

	if needsMapping {
		if !tokenSecretReady {
			return ctrl.Result{RequeueAfter: r.appProjectPendingRetryInterval()}, nil
		}
		return r.reconcileAppProjectMapping(
			ctx,
			harnessSession,
			agentCR,
			agentIdentifier,
			target,
		)
	}

	return ctrl.Result{}, nil
}

// optionalStr returns optional.NewString(s) when s is non-empty, otherwise
// optional.EmptyString(). Use this for OrgId/ProjectId which are omitted at
// ORG or ACCOUNT scope so the Harness API does not receive an empty string.
func optionalStr(s string) optional.String {
	if s == "" {
		return optional.EmptyString()
	}
	return optional.NewString(s)
}

// projectIdentifierForAgentScope limits projectIdentifier usage to PROJECT-scope agent APIs.
// ORG/ACCOUNT agent APIs must omit projectIdentifier; projectId is still used in mapping APIs.
func projectIdentifierForAgentScope(scope string, projectID string) string {
	if strings.EqualFold(scope, "PROJECT") {
		return strings.TrimSpace(projectID)
	}
	return ""
}

func optionalProjectIdentifierForAgentScope(scope string, projectID string) optional.String {
	return optionalStr(projectIdentifierForAgentScope(scope, projectID))
}

// scopedPathAgentIdentifierCandidates returns agent identifier variants used by APIs
// that take the identifier in the URL path. Harness often expects ORG/ACCOUNT agents
// as "org.<id>" / "account.<id>" on these endpoints, while other endpoints accept raw IDs.
func scopedPathAgentIdentifierCandidates(scope string, identifier string) []string {
	id := strings.TrimSpace(identifier)
	if id == "" {
		return nil
	}

	candidates := make([]string, 0, 2)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		for _, existing := range candidates {
			if existing == v {
				return
			}
		}
		candidates = append(candidates, v)
	}

	if strings.Contains(id, ".") {
		add(id)
		parts := strings.SplitN(id, ".", 2)
		if len(parts) == 2 {
			add(parts[1])
		}
		return candidates
	}

	switch {
	case strings.EqualFold(scope, "ORG"):
		add("org." + id)
	case strings.EqualFold(scope, "ACCOUNT"):
		add("account." + id)
	}
	add(id)
	return candidates
}

func wrapHarnessAPIError(message string, err error) error {
	if err == nil {
		return nil
	}
	if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
		body := strings.TrimSpace(string(swaggerErr.Body()))
		if body != "" {
			return fmt.Errorf("%s: %w (body: %s)", message, err, body)
		}
	}
	return fmt.Errorf("%s: %w", message, err)
}

func isHarnessAgentNotFound(err error) bool {
	swaggerErr, ok := err.(nextgen.GenericSwaggerError)
	if !ok {
		return false
	}
	body := strings.ToLower(string(swaggerErr.Body()))
	return strings.Contains(body, "agent not found")
}

func isHarnessAgentAlreadyExists(err error) bool {
	swaggerErr, ok := err.(nextgen.GenericSwaggerError)
	if !ok {
		return false
	}
	body := strings.ToLower(string(swaggerErr.Body()))
	return strings.Contains(body, "agent already exists")
}

// apiReader returns a reader that bypasses the manager's label-scoped Secret
// cache so the controller can read the user-created (unlabeled) API key Secret
// and detect pre-existing token Secrets. Falls back to the cached client when
// APIReader is unset (e.g. in unit tests).
func (r *HarnessGitopsAgentReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *HarnessGitopsAgentReconciler) getHarnessClient(ctx context.Context, agentCR *infrastructurev1.HarnessGitopsAgent) (*HarnessSession, error) {
	secret := &corev1.Secret{}
	secretNamespace := strings.TrimSpace(r.APIKeySecretNamespace)
	if secretNamespace == "" {
		secretNamespace = agentCR.Namespace
	}
	secretKey := client.ObjectKey{Name: agentCR.Spec.ApiKeySecretRef, Namespace: secretNamespace}
	if err := r.apiReader().Get(ctx, secretKey, secret); err != nil {
		return nil, err
	}

	apiKey, ok := secret.Data["api_key"]
	if !ok || len(apiKey) == 0 {
		return nil, k8serrors.NewBadRequest("api_key not found in secret")
	}

	cfg := nextgen.NewConfiguration()
	// Let controller-runtime own retries and rate limiting. The SDK default of
	// ten internal retries can otherwise block the sole reconcile worker for
	// minutes during a Harness 5xx response.
	cfg.HTTPClient.RetryMax = 0
	// A response that never sends headers would otherwise hang the sole
	// reconcile worker forever: the underlying client sets dial and TLS
	// timeouts but no overall request deadline.
	cfg.HTTPClient.HTTPClient.Timeout = DefaultHarnessHTTPTimeout
	apiClient := nextgen.NewAPIClient(cfg)

	authCtx := context.WithValue(ctx, nextgen.ContextAPIKey, nextgen.APIKey{
		Key: string(apiKey),
	})

	return &HarnessSession{
		Client:  apiClient,
		AuthCtx: authCtx,
	}, nil
}
