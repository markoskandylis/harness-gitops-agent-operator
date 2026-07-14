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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil" // REQUIRED for Finalizers
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	// 2. HARNESS SDK IMPORTS
	"github.com/antihax/optional" // REQUIRED for Delete Options
	"github.com/harness/harness-go-sdk/harness/nextgen"

	// 3. YOUR API DEFINITION
	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const harnessAgentFinalizer = "infrastructure.kandylis.co.uk/finalizer"

const gitopsAgentTokenSecretKey = "GITOPS_AGENT_TOKEN"

// HarnessGitopsAgentReconciler reconciles a HarnessGitopsAgent object
type HarnessGitopsAgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsagents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsagents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsagents/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

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
	mappingProjectID, mappingAppProject, err := projectMappingDetails(agentCR)
	if err != nil {
		return ctrl.Result{}, err
	}

	if agentCR.GetDeletionTimestamp() != nil {
		return r.reconcileDeletion(ctx, agentCR, existingAgentIdentifier, existingAgentMode, mappingProjectID)
	}

	if result, done, err := r.ensureFinalizer(ctx, agentCR); done {
		return result, err
	}

	return r.reconcileReady(ctx, req, agentCR, existingAgentIdentifier, existingAgentMode, mappingProjectID, mappingAppProject)
}

func projectMappingDetails(agentCR *infrastructurev1.HarnessGitopsAgent) (string, string, error) {
	mappingSpec := agentCR.Spec.ProjectMapping
	if mappingSpec == nil {
		return "", "", nil
	}

	scope := strings.TrimSpace(agentCR.Spec.Scope)
	if !strings.EqualFold(scope, "ORG") &&
		!strings.EqualFold(scope, "ACCOUNT") &&
		!strings.EqualFold(scope, "PROJECT") {
		return "", "", fmt.Errorf("spec.projectMapping is only supported for ACCOUNT, ORG, or PROJECT scope")
	}

	mappingProjectID := strings.TrimSpace(mappingSpec.ProjectId)
	mappingAppProject := strings.TrimSpace(mappingSpec.AppProject)
	if mappingProjectID == "" || mappingAppProject == "" {
		return "", "", fmt.Errorf("spec.projectMapping.projectId and spec.projectMapping.AppProject are both required when projectMapping is set")
	}
	return mappingProjectID, mappingAppProject, nil
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
	mappingProjectID string,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(agentCR, harnessAgentFinalizer) {
		return ctrl.Result{}, nil
	}

	log := logf.FromContext(ctx)
	if existingAgentMode {
		// Do not delete shared agents. Best effort cleanup for mapping created by this CR.
		if agentCR.Status.ArgoProjectMappingId != "" {
			log.Info("Deleting AppProject mapping", "mappingId", agentCR.Status.ArgoProjectMappingId)
			harnessSession, err := r.getHarnessClient(ctx, agentCR)
			if err != nil {
				log.Error(err, "Failed to initialize Harness session for mapping delete; retaining finalizer")
				return ctrl.Result{}, err
			}
			if err := r.deleteAppProjectMapping(
				harnessSession,
				agentCR,
				existingAgentIdentifier,
				agentCR.Status.ArgoProjectMappingId,
				mappingProjectID,
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
	mappingProjectID string,
	mappingAppProject string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	needsMapping := agentCR.Spec.ProjectMapping != nil

	agentDone := agentCR.Status.AgentIdentifier != ""
	argoProjectDone := !needsMapping || agentCR.Status.ArgoProjectId != ""
	mappingDone := !needsMapping ||
		agentCR.Status.ArgoProjectMappingId != "" ||
		agentCR.Status.ArgoProjectId != ""
	tokenSecretName := agentCR.Spec.TokenSecretRef
	if tokenSecretName == "" {
		tokenSecretName = agentCR.Name + "-agent-token"
	}
	tokenSecretReady := existingAgentMode || r.tokenSecretExists(ctx, agentCR, tokenSecretName)

	if agentDone && argoProjectDone && mappingDone && tokenSecretReady {
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
		agentIdentifier = scopedAgentIdentifier(existingAgentIdentifier)
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
			log.Info("Wrote agent token secret", "secret", tokenSecretName)
		}
	}

	// Maps the in-cluster ArgoProject to the target Harness project via the API.
	if needsMapping && agentCR.Status.ArgoProjectId == "" {
		mappingId, err := r.createAppProjectMapping(
			ctx,
			harnessSession,
			agentCR,
			agentIdentifier,
			mappingAppProject,
			mappingProjectID,
		)
		if err != nil {
			log.Error(err, "Failed to create AppProject mapping")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
		agentCR.Status.ArgoProjectId = mappingAppProject
		if mappingId != "" {
			agentCR.Status.ArgoProjectMappingId = mappingId
		}
		if err := r.Status().Update(ctx, agentCR); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("AppProject mapping resolved",
			"mappingId", mappingId,
			"argoProjectName", mappingAppProject,
			"project", mappingProjectID)
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

// scopedAgentIdentifier keeps the exact identifier shape provided by users/SDK.
// Do not force org/account prefixes here; Harness may return non-dot-scoped IDs.
func scopedAgentIdentifier(identifier string) string {
	id := strings.TrimSpace(identifier)
	if id == "" {
		return ""
	}
	return id
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

func harnessAPIErrorDetails(err error) string {
	if err == nil {
		return ""
	}
	if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
		body := strings.TrimSpace(string(swaggerErr.Body()))
		if body != "" {
			return fmt.Sprintf("%v (body: %s)", err, body)
		}
	}
	return err.Error()
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

func (r *HarnessGitopsAgentReconciler) getHarnessClient(ctx context.Context, agentCR *infrastructurev1.HarnessGitopsAgent) (*HarnessSession, error) {
	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{Name: agentCR.Spec.ApiKeySecretRef, Namespace: agentCR.Namespace}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return nil, err
	}

	apiKey, ok := secret.Data["api_key"]
	if !ok || len(apiKey) == 0 {
		return nil, k8serrors.NewBadRequest("api_key not found in secret")
	}

	cfg := nextgen.NewConfiguration()
	apiClient := nextgen.NewAPIClient(cfg)

	authCtx := context.WithValue(ctx, nextgen.ContextAPIKey, nextgen.APIKey{
		Key: string(apiKey),
	})

	return &HarnessSession{
		Client:  apiClient,
		AuthCtx: authCtx,
	}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HarnessGitopsAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1.HarnessGitopsAgent{}).
		Owns(&corev1.Secret{}). // Added to watch and own Secrets
		Named("harnessgitopsagent").
		Complete(r)
}
