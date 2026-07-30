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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
	resourceutil "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource"
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
	Scheme                    *runtime.Scheme
	APIReader                 client.Reader
	APIKeySecretNamespace     string
	AgentHealthResyncInterval time.Duration
	agentAPI                  agentAPI
}

type agentAPI interface {
	Create(
		context.Context,
		*harnessapi.Session,
		CreateAgentRequest,
	) (CreateAgentResult, error)
	Lookup(
		context.Context,
		*harnessapi.Session,
		Agent,
	) (AgentLookupResult, error)
	Delete(context.Context, *harnessapi.Session, Agent) error
	ResolveToken(context.Context, *harnessapi.Session, Agent, string) (string, error)
	Readiness(context.Context, *harnessapi.Session, Agent) (AgentReadiness, error)
}

func (r *Reconciler) harnessAgentAPI() agentAPI {
	if r.agentAPI != nil {
		return r.agentAPI
	}
	return SDKAgentAPI{}
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

	if result, done, err := resourceutil.EnsureFinalizer(
		ctx,
		r.Client,
		agentCR,
		harnessAgentFinalizer,
	); done {
		return result, err
	}

	return r.reconcileReady(ctx, req, agentCR, existingAgentIdentifier, existingAgentMode)
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
			log.Info("Using existing Harness GitOps Agent", "agentIdentifier", existingAgentIdentifier)
		}
		return r.refreshAgentHealth(ctx, agentCR, existingAgentIdentifier)
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
		return r.refreshAgentHealth(ctx, agentCR, agentCR.Status.AgentIdentifier)
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

	harnessSession, err := SessionForAgent(
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
		agentToken, err := r.harnessAgentAPI().ResolveToken(
			ctx,
			harnessSession,
			harnessAgentFor(agentCR, agentIdentifier),
			initialAgentToken,
		)
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

	return r.agentHealthResult(ctx, agentCR, harnessSession, agentIdentifier, nil)
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

// SessionForAgent reads the Agent's API-key Secret and constructs a Harness
// SDK session. An explicit namespace is authoritative; an empty namespace
// keeps the direct-binary behavior of reading beside the Agent.
func SessionForAgent(
	ctx context.Context,
	reader client.Reader,
	apiKeySecretNamespace string,
	agent *infrastructurev1.HarnessGitopsAgent,
) (*harnessapi.Session, error) {
	secretNamespace := strings.TrimSpace(apiKeySecretNamespace)
	if secretNamespace == "" {
		secretNamespace = agent.Namespace
	}

	return harnessapi.SessionFromSecret(ctx, reader, client.ObjectKey{
		Name:      agent.Spec.ApiKeySecretRef,
		Namespace: secretNamespace,
	})
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

func (r *Reconciler) tokenSecretExists(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	secretName string,
) bool {
	existing := &corev1.Secret{}
	if err := r.apiReader().Get(
		ctx,
		client.ObjectKey{Name: secretName, Namespace: agentCR.Namespace},
		existing,
	); err != nil {
		return false
	}
	token, ok := existing.Data[gitopsAgentTokenSecretKey]
	return ok && len(token) > 0
}

// SetupWithManager registers the Agent controller and its dependent watches.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1.HarnessGitopsAgent{}).
		Owns(&corev1.Secret{}).
		Watches(
			&infrastructurev1.HarnessGitopsProjectMapping{},
			handler.EnqueueRequestsFromMapFunc(projectMappingToAgentRequests),
		).
		Named("harnessgitopsagent").
		Complete(r)
}

func projectMappingToAgentRequests(
	_ context.Context,
	object client.Object,
) []reconcile.Request {
	mapping, ok := object.(*infrastructurev1.HarnessGitopsProjectMapping)
	if !ok {
		return nil
	}
	agentName := strings.TrimSpace(mapping.Spec.AgentRef.Name)
	if agentName == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: client.ObjectKey{
			Namespace: mapping.Namespace,
			Name:      agentName,
		},
	}}
}
