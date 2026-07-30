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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

// Harness mapping reads can lag a successful create. A Mapping deleted
// immediately after POST must remain finalizable long enough for the returned
// ID to become visible instead of treating one empty List as proof of absence.
const projectMappingCreateVisibilityGracePeriod = 30 * time.Second

type mappingCleanupIdentity struct {
	remote               *infrastructurev1.HarnessGitopsProjectMappingRemoteStatus
	mappingID            string
	recoveryOwnership    infrastructurev1.ResourceOwnership
	pendingReturnedID    bool
	removeFinalizer      bool
	ownershipBlockReason string
}

func (r *Reconciler) finalizeProjectMapping(
	ctx context.Context,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mapping, harnessProjectMappingFinalizer) {
		return ctrl.Result{}, nil
	}

	identity := resolveMappingCleanupIdentity(mapping)
	if identity.removeFinalizer {
		return r.removeProjectMappingFinalizer(ctx, mapping)
	}
	if identity.ownershipBlockReason != "" {
		return r.blockMappingCleanup(
			ctx,
			mapping,
			identity.ownershipBlockReason,
			nil,
		)
	}

	recoveryRemote := identity.remote.DeepCopy()
	recoveryRemote.MappingID = identity.mappingID
	request, mappingID, err := projectMappingCleanupRequest(recoveryRemote)
	if err != nil {
		return r.blockMappingCleanup(
			ctx,
			mapping,
			fmt.Sprintf("Stored mapping cleanup identity is invalid: %v", err),
			nil,
		)
	}

	// A deterministic loser needs no Harness credentials or remote read to
	// relinquish its finalizer. Keep the later arbitration as well so a claim
	// created while the Harness List is in flight cannot race the delete.
	claimWon, claimResult, claimErr := r.authorizeProjectMappingCleanup(
		ctx,
		mapping,
		request,
		mappingID,
	)
	if !claimWon {
		return claimResult, claimErr
	}

	agent := &infrastructurev1.HarnessGitopsAgent{}
	agentKey := client.ObjectKey{
		Namespace: mapping.Namespace,
		Name:      strings.TrimSpace(mapping.Spec.AgentRef.Name),
	}
	if err := r.apiReader().Get(ctx, agentKey, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			return r.blockMappingCleanup(
				ctx,
				mapping,
				fmt.Sprintf(
					"Cannot read API-key configuration because HarnessGitopsAgent %s/%s does not exist",
					agentKey.Namespace,
					agentKey.Name,
				),
				nil,
			)
		}
		return r.blockMappingCleanup(
			ctx,
			mapping,
			"Unable to read the referenced Agent during mapping cleanup",
			err,
		)
	}

	// A deleting Agent remains the authority for its API-key Secret until all
	// referencing Mapping finalizers have completed.
	session, err := harnessapi.SessionForAgent(
		ctx,
		r.apiReader(),
		r.APIKeySecretNamespace,
		agent,
	)
	if err != nil {
		return r.blockMappingCleanup(
			ctx,
			mapping,
			"Unable to initialize the Harness session during mapping cleanup",
			err,
		)
	}

	mappings, err := r.projectMappingAPI().List(ctx, session, request)
	if err != nil {
		return r.failMappingCleanup(
			ctx,
			mapping,
			"Unable to verify the Harness mapping before deletion",
			err,
		)
	}

	observed := projectMappingWithID(mappings, mappingID)
	if observed == nil {
		if identity.recoveryOwnership != "" {
			if identity.pendingReturnedID {
				if remaining := projectMappingCreateVisibilityGraceRemaining(mapping); remaining > 0 {
					statusErr := r.setReady(
						ctx,
						mapping,
						metav1.ConditionFalse,
						projectMappingReasonCleanupBlocked,
						fmt.Sprintf(
							"Harness mapping %q is not visible yet; waiting for create propagation before completing cleanup",
							mappingID,
						),
						nil,
					)
					return ctrl.Result{
						RequeueAfter: min(r.pendingRetryInterval(), remaining),
					}, statusErr
				}
				return r.removeProjectMappingFinalizer(ctx, mapping)
			}
			return r.blockMappingCleanup(
				ctx,
				mapping,
				fmt.Sprintf(
					"Cannot recover ownership because Harness mapping %q was not found",
					mappingID,
				),
				nil,
			)
		}
		return r.removeProjectMappingFinalizer(ctx, mapping)
	}
	if !projectMappingMatches(*observed, request) {
		if identity.recoveryOwnership != "" {
			return r.blockMappingCleanup(
				ctx,
				mapping,
				fmt.Sprintf(
					"Cannot recover ownership because Harness mapping %q does not match the complete stored tuple",
					mappingID,
				),
				nil,
			)
		}
		logf.FromContext(ctx).Info(
			"Refusing to delete a Harness mapping whose stored ID no longer matches the stored tuple",
			"mappingId", mappingID,
		)
		return r.removeProjectMappingFinalizer(ctx, mapping)
	}

	claimWon, claimResult, claimErr = r.authorizeProjectMappingCleanup(
		ctx,
		mapping,
		request,
		mappingID,
	)
	if !claimWon {
		return claimResult, claimErr
	}

	if identity.recoveryOwnership != "" {
		reason := projectMappingReasonMappingVerified
		message := "Recovered ownership of the controller-created Harness mapping before cleanup"
		if identity.recoveryOwnership == infrastructurev1.OwnershipAdopted {
			reason = projectMappingReasonMappingAdopted
			message = "Recovered ownership of the explicitly adopted Harness mapping before cleanup"
		}
		if err := r.setReady(
			ctx,
			mapping,
			metav1.ConditionTrue,
			reason,
			message,
			func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
				status.CreationState = ""
				status.Remote = identity.remote.DeepCopy()
				status.Remote.MappingID = mappingID
				status.Remote.Ownership = identity.recoveryOwnership
			},
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: immediateRequeueInterval}, nil
	}

	if err := r.projectMappingAPI().Delete(ctx, session, request, mappingID); err != nil {
		return r.failMappingCleanup(
			ctx,
			mapping,
			"Unable to delete the verified Harness mapping",
			err,
		)
	}
	return r.removeProjectMappingFinalizer(ctx, mapping)
}

func resolveMappingCleanupIdentity(
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
) mappingCleanupIdentity {
	remote := mapping.Status.Remote
	if remote == nil {
		if isUncertainCreationState(mapping.Status.CreationState) {
			return mappingCleanupIdentity{
				ownershipBlockReason: "Cannot recover mapping ownership because the stored remote tuple is empty",
			}
		}
		return mappingCleanupIdentity{removeFinalizer: true}
	}

	identity := mappingCleanupIdentity{remote: remote}
	if !isUncertainCreationState(mapping.Status.CreationState) {
		if !isDeletionOwnership(remote.Ownership) {
			identity.removeFinalizer = true
			return identity
		}
		identity.mappingID = strings.TrimSpace(remote.MappingID)
		return identity
	}

	adoptID := strings.TrimSpace(mapping.Spec.AdoptMappingID)
	rememberedID := strings.TrimSpace(remote.MappingID)
	identity.pendingReturnedID =
		mapping.Status.CreationState == infrastructurev1.MappingCreationPending &&
			rememberedID != ""
	if adoptID != "" && rememberedID != "" && adoptID != rememberedID {
		identity.ownershipBlockReason = fmt.Sprintf(
			"Cannot adopt Harness mapping %q because create candidate %q was already returned",
			adoptID,
			rememberedID,
		)
		return identity
	}

	switch {
	case adoptID != "":
		identity.mappingID = adoptID
		identity.recoveryOwnership = infrastructurev1.OwnershipAdopted
	case mapping.Status.CreationState == infrastructurev1.MappingCreationPending &&
		rememberedID != "":
		identity.mappingID = rememberedID
		identity.recoveryOwnership = infrastructurev1.OwnershipManaged
	default:
		identity.ownershipBlockReason = fmt.Sprintf(
			"Cannot safely clean up a mapping while its create state is %s; set spec.adoptMappingId to an exact Harness mapping ID",
			mapping.Status.CreationState,
		)
	}
	return identity
}

func projectMappingCreateVisibilityGraceRemaining(
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
) time.Duration {
	if mapping == nil ||
		mapping.DeletionTimestamp == nil ||
		mapping.DeletionTimestamp.IsZero() {
		return projectMappingCreateVisibilityGracePeriod
	}
	remaining := projectMappingCreateVisibilityGracePeriod -
		time.Since(mapping.DeletionTimestamp.Time)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// authorizeProjectMappingCleanup makes the same fresh, cluster-wide claim
// decision used by active reconciliation. A deterministic loser relinquishes
// only its Kubernetes finalizer; it never deletes the shared remote row.
func (r *Reconciler) authorizeProjectMappingCleanup(
	ctx context.Context,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
	request harnessapi.ProjectMappingRequest,
	mappingID string,
) (bool, ctrl.Result, error) {
	decision, err := r.resolveProjectMappingClaim(ctx, mapping, request, mappingID)
	if err != nil {
		result, cleanupErr := r.failMappingCleanup(
			ctx,
			mapping,
			"Unable to verify exclusive Kubernetes ownership before deleting the Harness mapping",
			err,
		)
		return false, result, cleanupErr
	}
	if decision.currentWins {
		return true, ctrl.Result{}, nil
	}

	winner := decision.winner.resource
	logf.FromContext(ctx).Info(
		"Skipping Harness mapping deletion because another Kubernetes resource owns the claim",
		"mappingId", mappingID,
		"winner", client.ObjectKeyFromObject(winner),
	)
	result, err := r.removeProjectMappingFinalizer(ctx, mapping)
	return false, result, err
}

func projectMappingCleanupRequest(
	remote *infrastructurev1.HarnessGitopsProjectMappingRemoteStatus,
) (harnessapi.ProjectMappingRequest, string, error) {
	if remote == nil {
		return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf("remote status is empty")
	}

	mappingID := strings.TrimSpace(remote.MappingID)
	accountID := strings.TrimSpace(remote.Agent.AccountID)
	agentID := strings.TrimSpace(remote.Agent.Identifier)
	scope := strings.ToUpper(strings.TrimSpace(remote.Agent.Scope))
	agentOrgID := strings.TrimSpace(remote.Agent.OrgID)
	agentProjectID := strings.TrimSpace(remote.Agent.ProjectID)
	targetOrgID := strings.TrimSpace(remote.Target.OrgID)
	targetProjectID := strings.TrimSpace(remote.Target.ProjectID)
	appProject := strings.TrimSpace(remote.Target.AppProject)

	switch {
	case mappingID == "":
		return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf("mapping ID is empty")
	case accountID == "":
		return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf("account ID is empty")
	case agentID == "":
		return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf("agent ID is empty")
	case targetOrgID == "":
		return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf("target organization ID is empty")
	case targetProjectID == "":
		return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf("target project ID is empty")
	case appProject == "":
		return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf("appProject is empty")
	}

	switch scope {
	case agentScopeAccount:
		if agentOrgID != "" || agentProjectID != "" {
			return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf(
				"ACCOUNT-scoped agent identity must not contain an organization or project",
			)
		}
	case agentScopeOrg:
		if agentOrgID == "" {
			return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf(
				"ORG-scoped agent identity has an empty organization ID",
			)
		}
		if agentProjectID != "" {
			return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf(
				"ORG-scoped agent identity must not contain a project",
			)
		}
		if targetOrgID != agentOrgID {
			return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf(
				"ORG-scoped agent and target organization IDs differ",
			)
		}
	case agentScopeProject:
		if agentOrgID == "" || agentProjectID == "" {
			return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf(
				"PROJECT-scoped agent identity requires organization and project IDs",
			)
		}
		if targetOrgID != agentOrgID || targetProjectID != agentProjectID {
			return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf(
				"PROJECT-scoped agent and target scopes differ",
			)
		}
	default:
		return harnessapi.ProjectMappingRequest{}, "", fmt.Errorf(
			"unsupported agent scope %q",
			remote.Agent.Scope,
		)
	}

	return harnessapi.ProjectMappingRequest{
		AccountIdentifier: accountID,
		AgentIdentifier:   agentID,
		AgentScope:        scope,
		Agent: harnessapi.Scope{
			OrgIdentifier:     agentOrgID,
			ProjectIdentifier: agentProjectID,
		},
		Mapping: harnessapi.Scope{
			OrgIdentifier:     targetOrgID,
			ProjectIdentifier: targetProjectID,
		},
		ArgoProjectName:      appProject,
		AutoCreateServiceEnv: remote.Target.AutoCreateServiceEnv,
	}, mappingID, nil
}

func (r *Reconciler) removeProjectMappingFinalizer(
	ctx context.Context,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(mapping, harnessProjectMappingFinalizer)
	return ctrl.Result{}, r.Update(ctx, mapping)
}

func (r *Reconciler) blockMappingCleanup(
	ctx context.Context,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
	message string,
	cause error,
) (ctrl.Result, error) {
	statusErr := r.setReady(
		ctx,
		mapping,
		metav1.ConditionFalse,
		projectMappingReasonCleanupBlocked,
		message,
		nil,
	)
	if cause != nil {
		return ctrl.Result{}, errors.Join(cause, statusErr)
	}
	return ctrl.Result{RequeueAfter: r.resyncInterval()}, statusErr
}

func (r *Reconciler) failMappingCleanup(
	ctx context.Context,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
	message string,
	cause error,
) (ctrl.Result, error) {
	statusErr := r.setReady(
		ctx,
		mapping,
		metav1.ConditionFalse,
		projectMappingReasonCleanupFailed,
		message,
		nil,
	)
	return ctrl.Result{}, errors.Join(cause, statusErr)
}
