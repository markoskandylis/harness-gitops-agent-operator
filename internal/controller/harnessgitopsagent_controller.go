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
	stderrors "errors"
	"fmt"
	"sort"
	"strings"
	"time"

	// 1. KUBERNETES IMPORTS
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
const argoProjectResolvedConditionType = "ArgoProjectResolved"

var (
	errArgoProjectMappingNotFound = stderrors.New("argo project mapping not found")
	errArgoProjectScopeMismatch   = stderrors.New("argo project mapping scope mismatch")
)

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
	log := logf.FromContext(ctx)

	// 1. FETCH THE OBJECT
	agentCR := &infrastructurev1.HarnessGitopsAgent{}
	if err := r.Get(ctx, req.NamespacedName, agentCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. CHECK IF "DELETE" WAS REQUESTED
	isAgentMarkedToBeDeleted := agentCR.GetDeletionTimestamp() != nil
	existingAgentIdentifier := strings.TrimSpace(agentCR.Spec.ExistingAgentIdentifier)
	existingAgentMode := existingAgentIdentifier != ""
	mappingSpec := agentCR.Spec.ProjectMapping
	needsMapping := mappingSpec != nil
	mappingProjectID := ""
	mappingAppProject := ""

	if needsMapping {
		scope := strings.TrimSpace(agentCR.Spec.Scope)
		if !strings.EqualFold(scope, "ORG") &&
			!strings.EqualFold(scope, "ACCOUNT") &&
			!strings.EqualFold(scope, "PROJECT") {
			return ctrl.Result{}, fmt.Errorf("spec.projectMapping is only supported for ACCOUNT, ORG, or PROJECT scope")
		}
		mappingProjectID = strings.TrimSpace(mappingSpec.ProjectId)
		mappingAppProject = strings.TrimSpace(mappingSpec.AppProject)
		if mappingProjectID == "" || mappingAppProject == "" {
			return ctrl.Result{}, fmt.Errorf("spec.projectMapping.projectId and spec.projectMapping.AppProject are both required when projectMapping is set")
		}
	}

	if isAgentMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(agentCR, harnessAgentFinalizer) {
			if existingAgentMode {
				// Do not delete shared agents. Best effort cleanup for mapping created by this CR.
				if agentCR.Status.ArgoProjectMappingId != "" {
					log.Info("Deleting AppProject mapping", "mappingId", agentCR.Status.ArgoProjectMappingId)
					harnessSession, err := r.getHarnessClient(ctx, agentCR)
					if err != nil {
						log.Error(err, "Failed to initialize Harness session for mapping delete; retaining finalizer")
						return ctrl.Result{}, err
					}
					_, _, delErr := harnessSession.Client.ProjectMappingsApi.AppProjectMappingServiceDeleteV2(
						harnessSession.AuthCtx,
						scopedAgentIdentifier(agentCR.Spec.Scope, existingAgentIdentifier),
						agentCR.Status.ArgoProjectMappingId,
						&nextgen.ProjectMappingsApiAppProjectMappingServiceDeleteV2Opts{
							AccountIdentifier: optional.NewString(agentCR.Spec.AccountId),
							OrgIdentifier:     optionalStr(agentCR.Spec.OrgId),
							ProjectIdentifier: optionalStr(mappingProjectID),
						},
					)
					if delErr != nil {
						if swaggerErr, ok := delErr.(nextgen.GenericSwaggerError); ok {
							body := strings.ToLower(string(swaggerErr.Body()))
							if !strings.Contains(body, "not found") {
								log.Error(delErr, "Failed to delete AppProject mapping; retaining finalizer")
								return ctrl.Result{}, delErr
							}
						} else {
							log.Error(delErr, "Failed to delete AppProject mapping; retaining finalizer")
							return ctrl.Result{}, delErr
						}
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

			agentAPIIdentifier := scopedAgentIdentifier(agentCR.Spec.Scope, agentIdentifier)
			_, _, err = harnessSession.Client.AgentApi.AgentServiceForServerDelete(
				harnessSession.AuthCtx,
				agentAPIIdentifier,
				&nextgen.AgentsApiAgentServiceForServerDeleteOpts{
					AccountIdentifier: optional.NewString(agentCR.Spec.AccountId),
					OrgIdentifier:     optionalStr(agentCR.Spec.OrgId),
					ProjectIdentifier: optionalProjectIdentifierForAgentScope(agentCR.Spec.Scope, agentCR.Spec.ProjectId),
					Name:              optional.NewString(agentCR.Spec.Name),
					Type_:             optional.NewString(agentCR.Spec.Type),
					Scope:             optional.NewString(agentCR.Spec.Scope),
				},
			)
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

			// C. Remove Finalizer
			controllerutil.RemoveFinalizer(agentCR, harnessAgentFinalizer)
			if err := r.Update(ctx, agentCR); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 3. ADD FINALIZER (If missing)
	if !controllerutil.ContainsFinalizer(agentCR, harnessAgentFinalizer) {
		controllerutil.AddFinalizer(agentCR, harnessAgentFinalizer)
		if err := r.Update(ctx, agentCR); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 4. CHECK IF FULLY RECONCILED (Idempotency)
	agentDone := agentCR.Status.AgentIdentifier != ""
	argoProjectDone := !needsMapping || agentCR.Status.ArgoProjectId != ""
	mappingDone := !needsMapping ||
		agentCR.Status.ArgoProjectMappingId != "" ||
		agentCR.Status.ArgoProjectId != ""

	if agentDone && argoProjectDone && mappingDone {
		return ctrl.Result{}, nil
	}

	// 5. REGISTER NEW AGENT (Create Logic)
	harnessSession, err := r.getHarnessClient(ctx, agentCR)
	if err != nil {
		log.Error(err, "Failed to initialize Harness Session")
		return ctrl.Result{}, err
	}

	agentIdentifier := agentCR.Status.AgentIdentifier
	var agentCredentials *nextgen.V1AgentCredentials

	if existingAgentMode {
		agentIdentifier = scopedAgentIdentifier(agentCR.Spec.Scope, existingAgentIdentifier)
		if agentCR.Status.AgentIdentifier == "" {
			agentCR.Status.AgentIdentifier = agentIdentifier
			if err := r.Status().Update(ctx, agentCR); err != nil {
				return ctrl.Result{}, err
			}
		}
		log.Info("Using existing Harness GitOps Agent for AppProject mapping", "agentIdentifier", agentIdentifier)
	} else if agentIdentifier == "" {
		log.Info("Registering new Harness GitOps Agent...", "Name", agentCR.Spec.Name)

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
				Namespace:        req.Namespace,
				HighAvailability: false,
			},
		}

		resp, _, err := harnessSession.Client.AgentApi.AgentServiceForServerCreate(harnessSession.AuthCtx, *createReq)
		if err != nil {
			if isHarnessAgentAlreadyExists(err) {
				agentIdentifier = scopedAgentIdentifier(agentCR.Spec.Scope, agentCR.Spec.Identifier)
				log.Info("Harness GitOps Agent already exists; continuing with existing identifier", "AgentID", agentIdentifier)
			} else {
				log.Error(err, "Harness API Call Failed")
				if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
					log.Error(err, "Harness API Response Body", "body", string(swaggerErr.Body()))
				}
				return ctrl.Result{}, err
			}
		} else {
			agentIdentifier = scopedAgentIdentifier(agentCR.Spec.Scope, resp.Identifier)
			agentCredentials = resp.Credentials
			log.Info("Registered new Harness GitOps Agent", "AgentID", agentIdentifier)
		}

		agentCR.Status.AgentIdentifier = agentIdentifier
		if err := r.Status().Update(ctx, agentCR); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 6. WRITE TOKEN SECRET — skipped in existing-agent mode (agent already has a token).
	tokenSecretName := agentCR.Spec.TokenSecretRef
	if agentCR.Spec.ExistingAgentIdentifier == "" {
		if tokenSecretName == "" {
			tokenSecretName = agentCR.Name + "-agent-token"
		}
		// Skip if already written to avoid invalidating the running agent.
		if !r.tokenSecretExists(ctx, agentCR, tokenSecretName) {
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

	// 8. CREATE APP PROJECT MAPPING (existing-agent mode only)
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

// toHarnessIdentifier converts a string to a Harness-safe identifier
// by replacing non-alphanumeric characters with underscores and ensuring
// it starts with a letter or underscore.
func toHarnessIdentifier(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' {
			b[i] = c
		} else {
			b[i] = '_'
		}
	}
	// Ensure the identifier starts with a letter or underscore.
	if len(b) > 0 && b[0] >= '0' && b[0] <= '9' {
		b = append([]byte{'_'}, b...)
	}
	return string(b)
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

// fetchArgoProjectId resolves the Argo AppProject name for an agent by using the
// latest v2 project-mapping endpoint, with a v1 fallback for compatibility.
func (r *HarnessGitopsAgentReconciler) fetchArgoProjectId(
	harnessSession *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
) (string, error) {
	v2Resp, _, v2Err := harnessSession.Client.ProjectMappingsApi.AppProjectMappingServiceGetAppProjectMappingsListByAgentV2(
		harnessSession.AuthCtx,
		agentIdentifier,
		&nextgen.ProjectMappingsApiAppProjectMappingServiceGetAppProjectMappingsListByAgentV2Opts{
			AccountIdentifier: optional.NewString(agentCR.Spec.AccountId),
			OrgIdentifier:     optionalStr(agentCR.Spec.OrgId),
			ProjectIdentifier: optionalStr(agentCR.Spec.ProjectId),
		},
	)
	if v2Err == nil {
		projectID, err := selectArgoProjectIDFromV2Mappings(
			v2Resp.AppProjectMappings,
			agentCR.Spec.AccountId,
			agentCR.Spec.OrgId,
			agentCR.Spec.ProjectId,
		)
		if err == nil {
			return projectID, nil
		}
	}

	v1Resp, _, v1Err := harnessSession.Client.ProjectMappingsApi.AppProjectMappingServiceGetAppProjectMappingListByAgent(
		harnessSession.AuthCtx,
		agentIdentifier,
		&nextgen.ProjectMappingsApiAppProjectMappingServiceGetAppProjectMappingListByAgentOpts{
			AccountIdentifier: optional.NewString(agentCR.Spec.AccountId),
			OrgIdentifier:     optionalStr(agentCR.Spec.OrgId),
			ProjectIdentifier: optionalStr(agentCR.Spec.ProjectId),
		},
	)
	if v1Err != nil {
		if v2Err != nil {
			return "", fmt.Errorf("project mappings v2 failed: %w; v1 fallback failed: %v", v2Err, v1Err)
		}
		return "", v1Err
	}

	projectID, selErr := selectArgoProjectIDFromV1Mapping(v1Resp.AppProjMap, agentCR.Spec.OrgId, agentCR.Spec.ProjectId)
	if selErr != nil {
		if v2Err != nil {
			return "", fmt.Errorf("project mappings v2 failed: %w; v1 fallback returned no scoped mapping: %v", v2Err, selErr)
		}
		return "", selErr
	}
	return projectID, nil
}

// createAppProjectMapping calls AppProjectMappingServiceCreateV2 to map an existing in-cluster
// ArgoCD AppProject to a specific Harness project using an already-running agent.
// Returns the mapping Identifier on success, or empty string if the mapping already exists.
func (r *HarnessGitopsAgentReconciler) createAppProjectMapping(
	ctx context.Context,
	session *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
	argoProjectName string,
	projectId string,
) (string, error) {
	candidates := scopedPathAgentIdentifierCandidates(agentCR.Spec.Scope, agentIdentifier)
	if len(candidates) == 0 {
		return "", fmt.Errorf("createAppProjectMapping failed: empty agent identifier")
	}

	var lastErr error
	for _, candidate := range candidates {
		resp, _, err := session.Client.ProjectMappingsApi.AppProjectMappingServiceCreateV2(
			session.AuthCtx,
			nextgen.V1AppProjectMappingCreateRequestV2{
				AgentIdentifier:   candidate,
				AccountIdentifier: agentCR.Spec.AccountId,
				OrgIdentifier:     agentCR.Spec.OrgId,
				ProjectIdentifier: projectId,
				ArgoProjectName:   argoProjectName,
			},
			candidate,
		)
		if err == nil {
			return resp.Identifier, nil
		}
		if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
			body := strings.ToLower(string(swaggerErr.Body()))
			if strings.Contains(body, "already exists") {
				return "", nil
			}
		}
		lastErr = fmt.Errorf(
			"createAppProjectMapping failed for agentIdentifier=%q: %s",
			candidate,
			harnessAPIErrorDetails(err),
		)
	}
	return "", lastErr
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
func scopedAgentIdentifier(scope string, identifier string) string {
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

func selectArgoProjectIDFromV2Mappings(
	mappings []nextgen.V1AppProjectMappingV2,
	accountID string,
	orgID string,
	projectID string,
) (string, error) {
	if len(mappings) == 0 {
		return "", fmt.Errorf("%w: v2 returned no mappings", errArgoProjectMappingNotFound)
	}

	candidateSet := map[string]struct{}{}
	scopeMismatch := false
	for _, mapping := range mappings {
		if mapping.AccountIdentifier == accountID &&
			mapping.OrgIdentifier == orgID &&
			mapping.ProjectIdentifier == projectID {
			name := strings.TrimSpace(mapping.ArgoProjectName)
			if name != "" {
				candidateSet[name] = struct{}{}
			}
			continue
		}
		scopeMismatch = true
	}

	if len(candidateSet) == 0 {
		if scopeMismatch {
			return "", fmt.Errorf("%w: expected account=%s org=%s project=%s", errArgoProjectScopeMismatch, accountID, orgID, projectID)
		}
		return "", fmt.Errorf("%w: no usable argoProjectName for account=%s org=%s project=%s", errArgoProjectMappingNotFound, accountID, orgID, projectID)
	}

	candidates := make([]string, 0, len(candidateSet))
	for candidate := range candidateSet {
		candidates = append(candidates, candidate)
	}
	sort.Strings(candidates)
	return candidates[0], nil
}

func selectArgoProjectIDFromV1Mapping(
	appProjMap map[string]nextgen.Servicev1Project,
	orgID string,
	projectID string,
) (string, error) {
	if len(appProjMap) == 0 {
		return "", fmt.Errorf("%w: v1 returned empty appProjMap", errArgoProjectMappingNotFound)
	}

	candidateSet := map[string]struct{}{}
	scopeMismatch := false
	for argoProjectID, project := range appProjMap {
		if project.OrgIdentifier == orgID && project.ProjectIdentifier == projectID {
			if strings.TrimSpace(argoProjectID) != "" {
				candidateSet[argoProjectID] = struct{}{}
			}
			continue
		}
		scopeMismatch = true
	}

	if len(candidateSet) == 0 {
		if scopeMismatch {
			return "", fmt.Errorf("%w: expected org=%s project=%s", errArgoProjectScopeMismatch, orgID, projectID)
		}
		return "", fmt.Errorf("%w: no scoped v1 app project mapping for org=%s project=%s", errArgoProjectMappingNotFound, orgID, projectID)
	}

	candidates := make([]string, 0, len(candidateSet))
	for candidate := range candidateSet {
		candidates = append(candidates, candidate)
	}
	sort.Strings(candidates)
	return candidates[0], nil
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
	client := nextgen.NewAPIClient(cfg)

	authCtx := context.WithValue(ctx, nextgen.ContextAPIKey, nextgen.APIKey{
		Key: string(apiKey),
	})

	return &HarnessSession{
		Client:  client,
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
