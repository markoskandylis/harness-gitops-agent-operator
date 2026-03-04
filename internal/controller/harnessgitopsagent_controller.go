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
	"encoding/base64"
	"fmt"
	"strings"

	// 1. KUBERNETES IMPORTS
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
const argoProjectIdSecretKey = "ARGO_PROJECT_ID"

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

	if isAgentMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(agentCR, harnessAgentFinalizer) {
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

			_, _, err = harnessSession.Client.AgentApi.AgentServiceForServerDelete(
				harnessSession.AuthCtx,
				agentIdentifier,
				&nextgen.AgentsApiAgentServiceForServerDeleteOpts{
					AccountIdentifier: optional.NewString(agentCR.Spec.AccountId),
					OrgIdentifier:     optional.NewString(agentCR.Spec.OrgId),
					ProjectIdentifier: optional.NewString(agentCR.Spec.ProjectId),
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
	// Cluster registration is required only when spec.clusterRegistration.enabled is true.
	clusterRegEnabled := agentCR.Spec.ClusterRegistration != nil && agentCR.Spec.ClusterRegistration.Enabled
	agentDone := agentCR.Status.AgentIdentifier != ""
	clusterDone := !clusterRegEnabled || agentCR.Status.ClusterIdentifier != ""
	argoProjectDone := agentCR.Status.ArgoProjectId != ""

	if agentDone && clusterDone && argoProjectDone {
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

	if agentIdentifier == "" {
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
			ProjectIdentifier: agentCR.Spec.ProjectId,
			Type_:             &gitopsAgentType,
			Scope:             &gitopsAgentScope,
			Metadata: &nextgen.V1AgentMetadata{
				Namespace:        req.Namespace,
				HighAvailability: false,
			},
		}

		resp, _, err := harnessSession.Client.AgentApi.AgentServiceForServerCreate(harnessSession.AuthCtx, *createReq)
		if err != nil {
			log.Error(err, "Harness API Call Failed")
			if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
				log.Error(err, "Harness API Response Body", "body", string(swaggerErr.Body()))
			}
			return ctrl.Result{}, err
		}

		agentIdentifier = resp.Identifier
		agentCredentials = resp.Credentials
		log.Info("Registered new Harness GitOps Agent", "AgentID", agentIdentifier)

		agentCR.Status.AgentIdentifier = agentIdentifier
		if err := r.Status().Update(ctx, agentCR); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 6. WRITE TOKEN SECRET (only on first creation to avoid invalidating the running agent).
	//    The secret must exist before cluster registration so the gitops-agent pod can
	//    start, connect to Harness, and be "seen" as connected.
	tokenSecretName := agentCR.Spec.TokenSecretRef
	if tokenSecretName == "" {
		tokenSecretName = agentCR.Name + "-agent-token"
	}

	// Check if the token secret already has a valid GITOPS_AGENT_TOKEN.
	// If so, skip token resolution to prevent regenerating credentials and
	// invalidating the currently-running gitops-agent pod.
	tokenAlreadyWritten := r.tokenSecretExists(ctx, agentCR, tokenSecretName)

	if !tokenAlreadyWritten {
		// First time (or secret was deleted) — resolve and write the token.
		agentToken, argoProjectId, err := r.resolveAgentDetails(harnessSession, agentCR, agentIdentifier, agentCredentials)
		if err != nil {
			log.Error(err, "Failed to resolve agent details from Harness")
			return ctrl.Result{}, err
		}
		if err := r.upsertAgentTokenSecret(ctx, agentCR, tokenSecretName, agentToken, argoProjectId); err != nil {
			log.Error(err, "Failed to create or update token secret", "secret", tokenSecretName)
			return ctrl.Result{}, err
		}
		log.Info("Wrote agent token secret", "secret", tokenSecretName)
	}

	// 7. REGISTER CLUSTER (if enabled and not yet done)
	// Requires the gitops-agent pod to be running and connected to Harness.
	if clusterRegEnabled && agentCR.Status.ClusterIdentifier == "" {
		clusterIdentifier, err := r.registerCluster(ctx, harnessSession, agentCR, agentIdentifier)
		if err != nil {
			log.Error(err, "Failed to register cluster with Harness")
			return ctrl.Result{}, err
		}
		log.Info("Registered cluster with Harness", "clusterIdentifier", clusterIdentifier)

		agentCR.Status.ClusterIdentifier = clusterIdentifier
		if err := r.Status().Update(ctx, agentCR); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 9. FETCH AND PERSIST ARGO PROJECT ID IN STATUS
	// Only needed if not already done. Uses a read-only GET (no token regeneration).
	if agentCR.Status.ArgoProjectId == "" {
		argoProjectId, err := r.fetchArgoProjectId(harnessSession, agentCR, agentIdentifier)
		if err != nil {
			log.Error(err, "Failed to fetch ArgoProject ID from Harness")
			return ctrl.Result{}, err
		}
		if argoProjectId != "" {
			agentCR.Status.ArgoProjectId = argoProjectId
			if err := r.Status().Update(ctx, agentCR); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("Stored ArgoCD AppProject ID in status", "argoProjectId", argoProjectId)
			// Update secret with ArgoProject ID so ApplicationSets can consume it.
			if err := r.upsertArgoProjectIdInSecret(ctx, agentCR, tokenSecretName, argoProjectId); err != nil {
				log.Error(err, "Failed to update secret with ArgoProject ID")
				return ctrl.Result{}, err
			}
		} else if clusterRegEnabled && agentCR.Status.ClusterIdentifier != "" {
			// Cluster was registered but AppProject not yet visible — requeue to retry.
			log.Info("Cluster registered but ArgoProject not yet visible; requeueing")
			return ctrl.Result{Requeue: true}, nil
		}
	}

	return ctrl.Result{}, nil
}

// registerCluster registers the cluster with Harness via the GitOps Clusters API.
// It uses the IN_CLUSTER or SERVICE_ACCOUNT connection type from the spec.
func (r *HarnessGitopsAgentReconciler) registerCluster(
	ctx context.Context,
	harnessSession *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
) (clusterIdentifier string, err error) {
	log := logf.FromContext(ctx)
	spec := agentCR.Spec.ClusterRegistration

	server := spec.Server
	if server == "" {
		server = "https://kubernetes.default.svc"
	}

	clusterName := spec.Name
	if clusterName == "" {
		clusterName = agentCR.Name
	}

	connectionType := spec.ConnectionType
	if connectionType == "" {
		connectionType = "IN_CLUSTER"
	}

	clusterConfig := &nextgen.ClustersClusterConfig{
		ClusterConnectionType: connectionType,
	}

	switch connectionType {
	case "IN_CLUSTER":
		clusterConfig.TlsClientConfig = &nextgen.ClustersTlsClientConfig{
			Insecure: true,
		}

	case "SERVICE_ACCOUNT":
		if spec.CredentialsRef == "" {
			return "", fmt.Errorf("SERVICE_ACCOUNT connection type requires credentialsRef to be set")
		}
		credsSecret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Name: spec.CredentialsRef, Namespace: agentCR.Namespace}, credsSecret); err != nil {
			return "", fmt.Errorf("failed to read credentialsRef secret %q: %w", spec.CredentialsRef, err)
		}
		bearerToken := string(credsSecret.Data["bearerToken"])
		caData := string(credsSecret.Data["caData"])
		insecureStr := string(credsSecret.Data["insecure"])

		clusterConfig.BearerToken = bearerToken
		clusterConfig.TlsClientConfig = &nextgen.ClustersTlsClientConfig{
			Insecure: strings.EqualFold(insecureStr, "true"),
			CaData:   caData,
		}

	default:
		return "", fmt.Errorf("unsupported connectionType %q; must be IN_CLUSTER or SERVICE_ACCOUNT", connectionType)
	}

	createReq := nextgen.ClustersClusterCreateRequest{
		Upsert: true,
		Cluster: &nextgen.ClustersCluster{
			Server: server,
			Name:   clusterName,
			Config: clusterConfig,
		},
	}

	// Derive a Harness-safe identifier (alphanumeric + underscore, starts with letter/underscore).
	clusterIdentifierStr := toHarnessIdentifier(clusterName)

	log.Info("Registering cluster with Harness", "server", server, "connectionType", connectionType, "identifier", clusterIdentifierStr)

	clusterResp, _, err := harnessSession.Client.ClustersApi.AgentClusterServiceCreate(
		harnessSession.AuthCtx,
		createReq,
		agentIdentifier,
		&nextgen.ClustersApiAgentClusterServiceCreateOpts{
			AccountIdentifier: optional.NewString(agentCR.Spec.AccountId),
			OrgIdentifier:     optional.NewString(agentCR.Spec.OrgId),
			ProjectIdentifier: optional.NewString(agentCR.Spec.ProjectId),
			Identifier:        optional.NewString(clusterIdentifierStr),
		},
	)
	if err != nil {
		if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
			return "", fmt.Errorf("cluster registration failed: %w (body: %s)", err, string(swaggerErr.Body()))
		}
		return "", fmt.Errorf("cluster registration failed: %w", err)
	}

	return clusterResp.Identifier, nil
}

// resolveAgentDetails returns the agent token (GITOPS_AGENT_TOKEN) and the
// ArgoCD AppProject ID (the key in metadata.mappedProjects.appProjMap) in a
// single GET call, falling back to credential regeneration if needed.
func (r *HarnessGitopsAgentReconciler) resolveAgentDetails(
	harnessSession *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
	credentials *nextgen.V1AgentCredentials,
) (agentToken string, argoProjectId string, err error) {
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
			OrgIdentifier:     optional.NewString(agentCR.Spec.OrgId),
			ProjectIdentifier: optional.NewString(agentCR.Spec.ProjectId),
			Scope:             optional.NewString(agentCR.Spec.Scope),
			WithCredentials:   optional.NewBool(true),
		},
	)
	if getErr != nil {
		return "", "", getErr
	}

	// Extract token from GET response if not already resolved.
	if agentToken == "" && getResp.Credentials != nil && getResp.Credentials.PrivateKey != "" {
		agentToken = getResp.Credentials.PrivateKey
	}

	// Extract ArgoCD AppProject ID — it is the key of the appProjMap.
	if getResp.Metadata != nil &&
		getResp.Metadata.MappedProjects != nil &&
		len(getResp.Metadata.MappedProjects.AppProjMap) > 0 {
		for projectId := range getResp.Metadata.MappedProjects.AppProjMap {
			argoProjectId = projectId
			break
		}
	}

	// Last resort: regenerate credentials if token still empty.
	if agentToken == "" {
		regenResp, _, regenErr := harnessSession.Client.AgentApi.AgentServiceForServerRegenerateCredentials(
			harnessSession.AuthCtx,
			agentIdentifier,
		)
		if regenErr != nil {
			return "", "", regenErr
		}
		if regenResp.Credentials == nil || regenResp.Credentials.PrivateKey == "" {
			return "", "", fmt.Errorf("harness API did not return private key for agent %q", agentIdentifier)
		}
		agentToken = regenResp.Credentials.PrivateKey
	}

	return agentToken, argoProjectId, nil
}

func (r *HarnessGitopsAgentReconciler) upsertAgentTokenSecret(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	secretName string,
	agentToken string,
	argoProjectId string,
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
		// The Harness API returns the private key as a base64-encoded PEM string.
		// The gitops-agent expects raw PEM (starting with "-----BEGIN").
		// Decode if the token appears to be base64-encoded PEM.
		tokenBytes := decodeAgentToken(agentToken)
		tokenSecret.Data[gitopsAgentTokenSecretKey] = tokenBytes
		// Consumed by ApplicationSet/Application `project:` field.
		// Empty string is stored as-is; consumers should check before use.
		if argoProjectId != "" {
			tokenSecret.Data[argoProjectIdSecretKey] = []byte(argoProjectId)
		}
		return nil
	})
	return err
}

// decodeAgentToken returns the raw PEM bytes for the agent token.
// The Harness API returns the private key as a base64-encoded PEM string.
// If the input looks like base64 (doesn't start with "-----BEGIN"), decode it first.
func decodeAgentToken(token string) []byte {
	if strings.HasPrefix(token, "-----") {
		// Already raw PEM.
		return []byte(token)
	}
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		// Not valid base64 either — store as-is and let the agent fail with a clear message.
		return []byte(token)
	}
	return decoded
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

// fetchArgoProjectId performs a read-only GET on the agent to extract the mapped AppProject ID.
// It never regenerates credentials so it is safe to call on every reconcile.
func (r *HarnessGitopsAgentReconciler) fetchArgoProjectId(
	harnessSession *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
) (string, error) {
	getResp, _, err := harnessSession.Client.AgentApi.AgentServiceForServerGet(
		harnessSession.AuthCtx,
		agentIdentifier,
		agentCR.Spec.AccountId,
		&nextgen.AgentsApiAgentServiceForServerGetOpts{
			OrgIdentifier:     optional.NewString(agentCR.Spec.OrgId),
			ProjectIdentifier: optional.NewString(agentCR.Spec.ProjectId),
			Scope:             optional.NewString(agentCR.Spec.Scope),
			WithCredentials:   optional.NewBool(false),
		},
	)
	if err != nil {
		return "", err
	}
	if getResp.Metadata != nil &&
		getResp.Metadata.MappedProjects != nil &&
		len(getResp.Metadata.MappedProjects.AppProjMap) > 0 {
		for projectId := range getResp.Metadata.MappedProjects.AppProjMap {
			return projectId, nil
		}
	}
	return "", nil
}

// upsertArgoProjectIdInSecret updates an existing token secret to add/update the ARGO_PROJECT_ID key.
func (r *HarnessGitopsAgentReconciler) upsertArgoProjectIdInSecret(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	secretName string,
	argoProjectId string,
) error {
	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: agentCR.Namespace}, existing); err != nil {
		return err
	}
	if existing.Data == nil {
		existing.Data = map[string][]byte{}
	}
	existing.Data[argoProjectIdSecretKey] = []byte(argoProjectId)
	return r.Update(ctx, existing)
}

func isHarnessAgentNotFound(err error) bool {
	swaggerErr, ok := err.(nextgen.GenericSwaggerError)
	if !ok {
		return false
	}
	body := strings.ToLower(string(swaggerErr.Body()))
	return strings.Contains(body, "agent not found")
}

func (r *HarnessGitopsAgentReconciler) getHarnessClient(ctx context.Context, agentCR *infrastructurev1.HarnessGitopsAgent) (*HarnessSession, error) {
	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{Name: agentCR.Spec.ApiKeySecretRef, Namespace: agentCR.Namespace}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return nil, err
	}

	apiKey, ok := secret.Data["api_key"]
	if !ok || len(apiKey) == 0 {
		return nil, errors.NewBadRequest("api_key not found in secret")
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
