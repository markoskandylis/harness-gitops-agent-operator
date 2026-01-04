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
			if err == nil {
				_, _, err = harnessSession.Client.AgentApi.AgentServiceForServerDelete(
					harnessSession.AuthCtx,
					agentCR.Status.AgentIdentifier,
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
					if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
						log.Error(err, "Failed to delete agent from Harness",
							"body", string(swaggerErr.Body()))
						// Optionally parse body if you want structured fields.
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

	// 4. CHECK IF REGISTERED (Idempotency)
	if agentCR.Status.AgentIdentifier != "" {
		return ctrl.Result{}, nil
	}

	// 5. REGISTER NEW AGENT (Create Logic)
	log.Info("Registering new Harness GitOps Agent...", "Name", agentCR.Spec.Name)

	harnessSession, err := r.getHarnessClient(ctx, agentCR)
	if err != nil {
		log.Error(err, "Failed to initialize Harness Session")
		// Do not requeue immediately, wait for secret to be created
		return ctrl.Result{}, err
	}

	gitopsAgentType := nextgen.V1AgentType(agentCR.Spec.Type)
	gitopsAgentSclope := nextgen.V1AgentScope(agentCR.Spec.Scope)
	gitopsOperator := nextgen.V1AgentOperator(agentCR.Spec.Operator)

	createReq := &nextgen.V1Agent{
		Name:              agentCR.Spec.Name,
		Identifier:        agentCR.Spec.Identifier,
		Operator:          &gitopsOperator,
		AccountIdentifier: agentCR.Spec.AccountId,
		OrgIdentifier:     agentCR.Spec.OrgId,
		ProjectIdentifier: agentCR.Spec.ProjectId,
		Type_:             &gitopsAgentType,
		Scope:             &gitopsAgentSclope,
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

	log.Info("Registering new Harness GitOps Agent succeeded", "AgentID", resp.Identifier)
	agentCR.Status.AgentIdentifier = resp.Identifier
	if err := r.Status().Update(ctx, agentCR); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Successfully registered new Harness GitOps Agent", "AgentID", resp.Identifier)

	// 6. CREATE SECRET WITH AGENT TOKEN
	tokenSecretName := agentCR.Spec.TokenSecretRef
	if tokenSecretName == "" {
		tokenSecretName = agentCR.Name + "-agent-token"
	}

	newTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tokenSecretName,
			Namespace: req.Namespace,
		},
		StringData: map[string]string{
			"token": resp.Credentials.PrivateKey, // Corrected from resp.Credentials.Cert
		},
	}

	if err := r.Create(ctx, newTokenSecret); err != nil {
		if !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
	}

	// Set owner reference for the Secret to ensure it's deleted with the agent CR
	if err := ctrl.SetControllerReference(agentCR, newTokenSecret, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Update(ctx, newTokenSecret); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
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
