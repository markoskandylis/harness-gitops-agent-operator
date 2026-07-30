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

package projectmapping

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

const (
	harnessProjectMappingFinalizer = "infrastructure.kandylis.co.uk/project-mapping-finalizer"
	projectMappingReadyCondition   = "Ready"

	projectMappingReasonResolutionInvalid  = "ResolutionInvalid"
	projectMappingReasonAgentRefNotFound   = "AgentRefNotFound"
	projectMappingReasonAgentDeleting      = "AgentDeleting"
	projectMappingReasonAppProjectNotFound = "AppProjectNotFound"
	projectMappingReasonAgentNotFound      = "AgentNotFound"
	projectMappingReasonAgentNotHealthy    = "AgentNotHealthy"
	projectMappingReasonVerificationFailed = "VerificationFailed"
	projectMappingReasonMappingMismatch    = "MappingMismatch"
	projectMappingReasonDuplicateMapping   = "DuplicateMapping"
	projectMappingReasonAdoptionFailed     = "AdoptionFailed"
	projectMappingReasonCreatePending      = "CreatePending"
	projectMappingReasonCreateUnknown      = "CreateOutcomeUnknown"
	projectMappingReasonMappingCreated     = "MappingCreated"
	projectMappingReasonMappingVerified    = "MappingVerified"
	projectMappingReasonMappingExternal    = "MappingExternal"
	projectMappingReasonMappingAdopted     = "MappingAdopted"
	projectMappingReasonCleanupBlocked     = "CleanupBlocked"
	projectMappingReasonCleanupFailed      = "CleanupFailed"
)

// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsprojectmappings,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsprojectmappings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.kandylis.co.uk,resources=harnessgitopsprojectmappings/finalizers,verbs=update
// +kubebuilder:rbac:groups=argoproj.io,resources=appprojects,verbs=get

// mappingReconcileAPI is owned by the reconciler so its tests do not depend on
// the complete Harness SDK surface.
type mappingReconcileAPI interface {
	List(
		context.Context,
		*harnessapi.Session,
		harnessapi.ProjectMappingRequest,
	) ([]harnessapi.ProjectMapping, error)
	Create(
		context.Context,
		*harnessapi.Session,
		harnessapi.ProjectMappingRequest,
	) (harnessapi.ProjectMapping, error)
	Delete(
		context.Context,
		*harnessapi.Session,
		harnessapi.ProjectMappingRequest,
		string,
	) error
}

type mappingAgentReadinessAPI interface {
	Readiness(
		context.Context,
		*harnessapi.Session,
		harnessapi.Agent,
	) (harnessapi.AgentReadiness, error)
}

// Reconciler reconciles one AppProject mapping.
type Reconciler struct {
	client.Client
	APIReader                      client.Reader
	APIKeySecretNamespace          string
	AppProjectPendingRetryInterval time.Duration
	HarnessMappingResyncInterval   time.Duration

	mappingAPI       mappingReconcileAPI
	agentAPI         mappingAgentReadinessAPI
	appProjectClient dynamic.Interface
}

func (r *Reconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	mapping := &infrastructurev1.HarnessGitopsProjectMapping{}
	if err := r.Get(ctx, req.NamespacedName, mapping); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !mapping.DeletionTimestamp.IsZero() {
		return r.finalizeProjectMapping(ctx, mapping)
	}

	if !controllerutil.ContainsFinalizer(mapping, harnessProjectMappingFinalizer) {
		controllerutil.AddFinalizer(mapping, harnessProjectMappingFinalizer)
		if err := r.Update(ctx, mapping); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: immediateRequeueInterval}, nil
	}

	agent := &infrastructurev1.HarnessGitopsAgent{}
	agentKey := client.ObjectKey{
		Namespace: mapping.Namespace,
		Name:      strings.TrimSpace(mapping.Spec.AgentRef.Name),
	}
	if err := r.Get(ctx, agentKey, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			statusErr := r.setReady(
				ctx,
				mapping,
				metav1.ConditionFalse,
				projectMappingReasonAgentRefNotFound,
				fmt.Sprintf("HarnessGitopsAgent %s/%s does not exist", agentKey.Namespace, agentKey.Name),
				nil,
			)
			return ctrl.Result{RequeueAfter: r.pendingRetryInterval()}, statusErr
		}
		return r.returnAPIError(ctx, mapping, "Unable to read the referenced Agent", err)
	}
	if !agent.DeletionTimestamp.IsZero() {
		err := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonAgentDeleting,
			fmt.Sprintf("HarnessGitopsAgent %s/%s is being deleted", agent.Namespace, agent.Name),
			nil,
		)
		return ctrl.Result{RequeueAfter: r.pendingRetryInterval()}, err
	}

	request, err := resolveProjectMappingRequest(agent, mapping)
	if err != nil {
		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonResolutionInvalid,
			err.Error(),
			nil,
		)
		return ctrl.Result{}, statusErr
	}

	exists, err := appProjectExists(
		ctx,
		r.Client,
		r.appProjectClient,
		mapping.Namespace,
		request.ArgoProjectName,
	)
	if err != nil {
		return r.returnAPIError(ctx, mapping, "Unable to read the AppProject", err)
	}
	if !exists {
		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonAppProjectNotFound,
			fmt.Sprintf("AppProject %s/%s does not exist", mapping.Namespace, request.ArgoProjectName),
			nil,
		)
		return ctrl.Result{RequeueAfter: r.pendingRetryInterval()}, statusErr
	}

	session, err := harnessapi.SessionForAgent(
		ctx,
		r.apiReader(),
		r.APIKeySecretNamespace,
		agent,
	)
	if err != nil {
		return r.returnAPIError(ctx, mapping, "Unable to initialize the Harness session", err)
	}

	readiness, err := r.agentReadinessAPI().Readiness(
		ctx,
		session,
		mappingHarnessAgent(agent, request),
	)
	if err != nil {
		return r.returnAPIError(ctx, mapping, "Unable to verify the Harness GitOps agent", err)
	}
	if !readiness.Exists {
		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonAgentNotFound,
			"Harness GitOps agent does not exist yet",
			nil,
		)
		return ctrl.Result{RequeueAfter: r.pendingRetryInterval()}, statusErr
	}
	if !readiness.Ready {
		message := strings.TrimSpace(readiness.Message)
		if message == "" {
			message = "Harness GitOps agent is not Connected and Healthy yet"
		}
		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonAgentNotHealthy,
			message,
			nil,
		)
		return ctrl.Result{RequeueAfter: r.pendingRetryInterval()}, statusErr
	}

	return r.reconcileHarnessMapping(ctx, mapping, session, request)
}

func (r *Reconciler) reconcileHarnessMapping(
	ctx context.Context,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
	session *harnessapi.Session,
	request harnessapi.ProjectMappingRequest,
) (ctrl.Result, error) {
	startedCreationState := mapping.Status.CreationState
	mappings, err := r.projectMappingAPI().List(ctx, session, request)
	if err != nil {
		return r.returnAPIError(ctx, mapping, "Unable to list Harness AppProject mappings", err)
	}

	selection := selectProjectMapping(mappings, request, mapping)
	if selection.mapping != nil {
		return r.reconcileSelectedMapping(ctx, mapping, request, *selection.mapping)
	}
	if selection.duplicate {
		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonDuplicateMapping,
			"Multiple exact Harness mappings exist and no recorded or adopted ID selects one",
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				preserveMappingFailureState(status, request, startedCreationState)
			},
		)
		return ctrl.Result{RequeueAfter: r.resyncInterval()}, statusErr
	}
	if selection.conflict {
		reason := projectMappingReasonMappingMismatch
		message := "A Harness mapping for this AppProject exists with a different tuple"
		if selection.adoptConflict {
			reason = projectMappingReasonAdoptionFailed
			message = fmt.Sprintf(
				"Harness mapping %q exists but does not match the complete desired tuple",
				strings.TrimSpace(mapping.Spec.AdoptMappingID),
			)
		}
		if startedCreationState == infrastructurev1.MappingCreationPending ||
			startedCreationState == infrastructurev1.MappingCreationOutcomeUnknown {
			message = "Create ownership is unresolved and the observed AppProject mapping has a different tuple"
		}
		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			reason,
			message,
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				preserveMappingFailureState(status, request, startedCreationState)
			},
		)
		return ctrl.Result{RequeueAfter: r.resyncInterval()}, statusErr
	}

	adoptID := strings.TrimSpace(mapping.Spec.AdoptMappingID)
	if adoptID != "" {
		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonAdoptionFailed,
			fmt.Sprintf("Harness mapping %q does not exist with the complete desired tuple", adoptID),
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				preserveMappingFailureState(status, request, startedCreationState)
			},
		)
		return ctrl.Result{RequeueAfter: r.resyncInterval()}, statusErr
	}

	switch startedCreationState {
	case infrastructurev1.MappingCreationPending:
		if mapping.Status.Remote != nil &&
			strings.TrimSpace(mapping.Status.Remote.MappingID) != "" {
			statusErr := r.setReady(
				ctx,
				mapping,
				metav1.ConditionFalse,
				projectMappingReasonMappingCreated,
				"Harness accepted the mapping create; waiting to verify its returned ID",
				nil,
			)
			return ctrl.Result{RequeueAfter: r.pendingRetryInterval()}, statusErr
		}
		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonCreateUnknown,
			"A persisted create attempt has no returned ID; set spec.adoptMappingId to claim an exact observed mapping",
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				status.CreationState = infrastructurev1.MappingCreationOutcomeUnknown
				status.Remote = unresolvedRemoteStatus(status.Remote, request)
			},
		)
		return ctrl.Result{RequeueAfter: r.resyncInterval()}, statusErr

	case infrastructurev1.MappingCreationOutcomeUnknown:
		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonCreateUnknown,
			"Mapping create outcome is unknown; set spec.adoptMappingId after verifying the Harness mapping",
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				status.Remote = unresolvedRemoteStatus(status.Remote, request)
			},
		)
		return ctrl.Result{RequeueAfter: r.resyncInterval()}, statusErr
	}

	// Persist the complete intent before POST. The same reconcile may create;
	// any later reconcile that starts in Pending never repeats that POST.
	if err := r.setReady(
		ctx,
		mapping,
		metav1.ConditionFalse,
		projectMappingReasonCreatePending,
		"Persisted Harness mapping create intent",
		func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
			status.CreationState = infrastructurev1.MappingCreationPending
			status.Remote = remoteStatusForRequest(request)
		},
	); err != nil {
		return ctrl.Result{}, err
	}

	created, err := r.projectMappingAPI().Create(ctx, session, request)
	if err != nil {
		if errors.Is(err, harnessapi.ErrProjectMappingCreateOutcomeUnknown) ||
			errors.Is(err, harnessapi.ErrProjectMappingAlreadyExists) {
			statusErr := r.setReady(
				ctx,
				mapping,
				metav1.ConditionFalse,
				projectMappingReasonCreateUnknown,
				"Harness mapping create may have committed; explicit adoption is required",
				func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
					status.CreationState = infrastructurev1.MappingCreationOutcomeUnknown
					status.Remote = unresolvedRemoteStatus(status.Remote, request)
				},
			)
			return ctrl.Result{RequeueAfter: r.resyncInterval()}, statusErr
		}

		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonVerificationFailed,
			"Harness rejected the mapping create request",
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				status.CreationState = ""
				status.Remote = remoteStatusForRequest(request)
			},
		)
		return ctrl.Result{}, errors.Join(err, statusErr)
	}

	createdID := strings.TrimSpace(created.Identifier)
	if createdID == "" {
		statusErr := r.setReady(
			ctx,
			mapping,
			metav1.ConditionFalse,
			projectMappingReasonCreateUnknown,
			"Harness accepted the create without returning an ID; explicit adoption is required",
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				status.CreationState = infrastructurev1.MappingCreationOutcomeUnknown
				status.Remote = unresolvedRemoteStatus(status.Remote, request)
			},
		)
		return ctrl.Result{RequeueAfter: r.resyncInterval()}, statusErr
	}

	returnedAgentID := strings.TrimSpace(created.AgentIdentifier)
	if returnedAgentID == "" {
		returnedAgentID = request.AgentIdentifier
	}
	statusErr := r.setReady(
		ctx,
		mapping,
		metav1.ConditionFalse,
		projectMappingReasonMappingCreated,
		"Harness accepted the mapping create; waiting for exact-ID verification",
		func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
			status.CreationState = infrastructurev1.MappingCreationPending
			status.Remote = remoteStatusForRequest(request)
			status.Remote.MappingID = createdID
			status.Remote.Agent.Identifier = returnedAgentID
		},
	)
	return ctrl.Result{RequeueAfter: immediateRequeueInterval}, statusErr
}

func (r *Reconciler) reconcileSelectedMapping(
	ctx context.Context,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
	request harnessapi.ProjectMappingRequest,
	observed harnessapi.ProjectMapping,
) (ctrl.Result, error) {
	observedID := strings.TrimSpace(observed.Identifier)
	adoptID := strings.TrimSpace(mapping.Spec.AdoptMappingID)
	rememberedID := ""
	rememberedOwnership := infrastructurev1.ResourceOwnership("")
	if mapping.Status.Remote != nil {
		rememberedID = strings.TrimSpace(mapping.Status.Remote.MappingID)
		rememberedOwnership = mapping.Status.Remote.Ownership
	}

	record := func(
		status metav1.ConditionStatus,
		reason string,
		message string,
		mutate func(*infrastructurev1.HarnessGitopsProjectMappingStatus),
	) (ctrl.Result, error) {
		statusErr := r.setReady(ctx, mapping, status, reason, message, mutate)
		return ctrl.Result{RequeueAfter: r.resyncInterval()}, statusErr
	}

	ownershipProven := isDeletionOwnership(rememberedOwnership) && rememberedID != ""
	if ownershipProven {
		if adoptID != "" && adoptID != rememberedID {
			return record(
				metav1.ConditionFalse,
				projectMappingReasonAdoptionFailed,
				fmt.Sprintf(
					"Cannot transfer %s ownership from Harness mapping %q to %q",
					rememberedOwnership,
					rememberedID,
					adoptID,
				),
				nil,
			)
		}
		if observedID == rememberedID {
			won, result, err := r.requireProjectMappingClaim(
				ctx,
				mapping,
				request,
				rememberedID,
			)
			if !won {
				return result, err
			}
			return record(
				metav1.ConditionTrue,
				projectMappingReasonMappingVerified,
				"The owned Harness mapping still matches the complete desired tuple",
				func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
					status.CreationState = ""
					// Keep the exact cleanup snapshot that originally proved
					// ownership; mutable adoption input cannot rewrite it.
				},
			)
		}
		if adoptID != "" {
			return record(
				metav1.ConditionFalse,
				projectMappingReasonAdoptionFailed,
				fmt.Sprintf(
					"Owned Harness mapping %q is absent; adoption cannot transfer ownership",
					rememberedID,
				),
				nil,
			)
		}
		// The remembered owned row is gone and a single replacement exact row
		// exists. Observe it conservatively as External.
	}

	unresolvedCandidate := isUncertainCreationState(mapping.Status.CreationState) &&
		rememberedID != ""
	if unresolvedCandidate {
		if adoptID != "" && adoptID != rememberedID {
			return record(
				metav1.ConditionFalse,
				projectMappingReasonAdoptionFailed,
				fmt.Sprintf(
					"Create candidate %q cannot be replaced by adoption of %q",
					rememberedID,
					adoptID,
				),
				func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
					status.CreationState = infrastructurev1.MappingCreationOutcomeUnknown
					status.Remote = unresolvedRemoteStatus(status.Remote, request)
				},
			)
		}
		if observedID == rememberedID {
			switch {
			case adoptID == rememberedID:
				won, result, err := r.requireProjectMappingClaim(
					ctx,
					mapping,
					request,
					rememberedID,
				)
				if !won {
					return result, err
				}
				return record(
					metav1.ConditionTrue,
					projectMappingReasonMappingAdopted,
					"The exact create candidate was explicitly adopted",
					func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
						status.CreationState = ""
						status.Remote = status.Remote.DeepCopy()
						status.Remote.Ownership = infrastructurev1.OwnershipAdopted
					},
				)
			case mapping.Status.CreationState == infrastructurev1.MappingCreationPending:
				won, result, err := r.requireProjectMappingClaim(
					ctx,
					mapping,
					request,
					rememberedID,
				)
				if !won {
					return result, err
				}
				return record(
					metav1.ConditionTrue,
					projectMappingReasonMappingVerified,
					"The controller-created Harness mapping was verified by its returned ID",
					func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
						status.CreationState = ""
						status.Remote = status.Remote.DeepCopy()
						status.Remote.Ownership = infrastructurev1.OwnershipManaged
					},
				)
			}
		}

		reason := projectMappingReasonCreateUnknown
		message := "Create ownership remains unresolved; adopt the remembered candidate ID or replace the resource"
		if adoptID != "" {
			reason = projectMappingReasonAdoptionFailed
			message = fmt.Sprintf(
				"Harness mapping %q is not the remembered create candidate %q",
				adoptID,
				rememberedID,
			)
		}
		return record(
			metav1.ConditionFalse,
			reason,
			message,
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				status.CreationState = infrastructurev1.MappingCreationOutcomeUnknown
				status.Remote = unresolvedRemoteStatus(status.Remote, request)
			},
		)
	}

	if adoptID == observedID {
		won, result, err := r.requireProjectMappingClaim(
			ctx,
			mapping,
			request,
			observedID,
		)
		if !won {
			return result, err
		}
		return record(
			metav1.ConditionTrue,
			projectMappingReasonMappingAdopted,
			"The exact Harness mapping was explicitly adopted",
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				status.CreationState = ""
				status.Remote = remoteStatusForObserved(request, observed)
				status.Remote.Ownership = infrastructurev1.OwnershipAdopted
			},
		)
	}

	if isUncertainCreationState(mapping.Status.CreationState) {
		reason := projectMappingReasonCreateUnknown
		message := "An exact mapping exists, but create ownership is unresolved; set spec.adoptMappingId to claim it"
		if adoptID != "" {
			reason = projectMappingReasonAdoptionFailed
			message = fmt.Sprintf(
				"Harness mapping %q does not match the observed create candidate %q",
				adoptID,
				observedID,
			)
		}
		return record(
			metav1.ConditionFalse,
			reason,
			message,
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				status.CreationState = infrastructurev1.MappingCreationOutcomeUnknown
				status.Remote = remoteStatusForObserved(request, observed)
				status.Remote.Ownership = ""
			},
		)
	}

	if adoptID != "" {
		return record(
			metav1.ConditionFalse,
			projectMappingReasonAdoptionFailed,
			fmt.Sprintf(
				"Harness mapping %q does not match the observed exact mapping %q",
				adoptID,
				observedID,
			),
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				status.CreationState = ""
				status.Remote = remoteStatusForObserved(request, observed)
			},
		)
	}

	won, result, err := r.requireNewExternalProjectMappingClaim(
		ctx,
		mapping,
		request,
		observedID,
	)
	if !won {
		return result, err
	}
	return record(
		metav1.ConditionTrue,
		projectMappingReasonMappingExternal,
		"The exact Harness mapping exists and is treated as external",
		func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
			status.CreationState = ""
			status.Remote = remoteStatusForObserved(request, observed)
			status.Remote.Ownership = infrastructurev1.OwnershipExternal
		},
	)
}
