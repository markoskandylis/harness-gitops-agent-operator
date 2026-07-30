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
	"reflect"
	"strings"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

type projectMappingSelection struct {
	mapping       *ProjectMapping
	conflict      bool
	adoptConflict bool
	duplicate     bool
}

func selectProjectMapping(
	mappings []ProjectMapping,
	request ProjectMappingRequest,
	resource *infrastructurev1.HarnessGitopsProjectMapping,
) projectMappingSelection {
	rememberedID := ""
	rememberedOwnership := infrastructurev1.ResourceOwnership("")
	if resource.Status.Remote != nil {
		rememberedID = strings.TrimSpace(resource.Status.Remote.MappingID)
		rememberedOwnership = resource.Status.Remote.Ownership
	}
	rememberedOwned := rememberedID != "" && isDeletionOwnership(rememberedOwnership)
	rememberedOwnedMismatch := false

	exact := make([]ProjectMapping, 0, 1)
	conflict := false
	adoptID := strings.TrimSpace(resource.Spec.AdoptMappingID)
	adoptConflict := false
	for _, mapping := range mappings {
		if rememberedOwned &&
			strings.TrimSpace(mapping.Identifier) == rememberedID &&
			!projectMappingMatches(mapping, request) {
			rememberedOwnedMismatch = true
		}
		if strings.TrimSpace(mapping.ArgoProjectName) != strings.TrimSpace(request.ArgoProjectName) {
			continue
		}
		if projectMappingMatches(mapping, request) {
			exact = append(exact, mapping)
		} else {
			conflict = true
			if adoptID != "" && strings.TrimSpace(mapping.Identifier) == adoptID {
				adoptConflict = true
			}
		}
	}
	if rememberedOwnedMismatch {
		// A replacement exact row must not hide a still-present owned row whose
		// tuple drifted. Preserve its cleanup identity and fail closed.
		return projectMappingSelection{conflict: true}
	}
	if len(exact) == 0 {
		return projectMappingSelection{
			conflict:      conflict,
			adoptConflict: adoptConflict,
		}
	}

	rememberedHasPriority := rememberedID != "" &&
		(isDeletionOwnership(rememberedOwnership) ||
			isUncertainCreationState(resource.Status.CreationState))
	if rememberedHasPriority {
		if selected := projectMappingWithID(exact, rememberedID); selected != nil {
			return projectMappingSelection{mapping: selected}
		}
	}

	if adoptID != "" {
		if selected := projectMappingWithID(exact, adoptID); selected != nil {
			return projectMappingSelection{mapping: selected}
		}
	}

	if selected := projectMappingWithID(exact, rememberedID); selected != nil {
		return projectMappingSelection{mapping: selected}
	}

	if len(exact) == 1 {
		selected := exact[0]
		return projectMappingSelection{mapping: &selected}
	}
	return projectMappingSelection{
		conflict:      conflict,
		adoptConflict: adoptConflict,
		duplicate:     true,
	}
}

func projectMappingWithID(
	mappings []ProjectMapping,
	id string,
) *ProjectMapping {
	if id == "" {
		return nil
	}
	for i := range mappings {
		if strings.TrimSpace(mappings[i].Identifier) == id {
			selected := mappings[i]
			return &selected
		}
	}
	return nil
}

func projectMappingMatches(
	mapping ProjectMapping,
	request ProjectMappingRequest,
) bool {
	return strings.TrimSpace(mapping.Identifier) != "" &&
		harnessapi.IdentifiersEquivalent(
			request.AgentScope,
			mapping.AgentIdentifier,
			request.AgentIdentifier,
		) &&
		strings.TrimSpace(mapping.AccountIdentifier) == strings.TrimSpace(request.AccountIdentifier) &&
		strings.TrimSpace(mapping.OrgIdentifier) == strings.TrimSpace(request.Mapping.OrgIdentifier) &&
		strings.TrimSpace(mapping.ProjectIdentifier) == strings.TrimSpace(request.Mapping.ProjectIdentifier) &&
		strings.TrimSpace(mapping.ArgoProjectName) == strings.TrimSpace(request.ArgoProjectName) &&
		mapping.AutoCreateServiceEnv == request.AutoCreateServiceEnv
}

func remoteStatusForRequest(
	request ProjectMappingRequest,
) *infrastructurev1.HarnessGitopsProjectMappingRemoteStatus {
	return &infrastructurev1.HarnessGitopsProjectMappingRemoteStatus{
		Agent: infrastructurev1.HarnessGitopsProjectMappingAgentStatus{
			Identifier: strings.TrimSpace(request.AgentIdentifier),
			AccountID:  strings.TrimSpace(request.AccountIdentifier),
			Scope:      strings.ToUpper(strings.TrimSpace(request.AgentScope)),
			OrgID:      strings.TrimSpace(request.Agent.OrgIdentifier),
			ProjectID:  strings.TrimSpace(request.Agent.ProjectIdentifier),
		},
		Target: infrastructurev1.HarnessGitopsProjectMappingTargetStatus{
			OrgID:                strings.TrimSpace(request.Mapping.OrgIdentifier),
			ProjectID:            strings.TrimSpace(request.Mapping.ProjectIdentifier),
			AppProject:           strings.TrimSpace(request.ArgoProjectName),
			AutoCreateServiceEnv: request.AutoCreateServiceEnv,
		},
	}
}

func remoteStatusForObserved(
	request ProjectMappingRequest,
	mapping ProjectMapping,
) *infrastructurev1.HarnessGitopsProjectMappingRemoteStatus {
	remote := remoteStatusForRequest(request)
	remote.MappingID = strings.TrimSpace(mapping.Identifier)
	if agentID := strings.TrimSpace(mapping.AgentIdentifier); agentID != "" {
		remote.Agent.Identifier = agentID
	}
	return remote
}

func unresolvedRemoteStatus(
	current *infrastructurev1.HarnessGitopsProjectMappingRemoteStatus,
	request ProjectMappingRequest,
) *infrastructurev1.HarnessGitopsProjectMappingRemoteStatus {
	if current == nil {
		return remoteStatusForRequest(request)
	}
	remote := current.DeepCopy()
	remote.Ownership = ""
	return remote
}

func isDeletionOwnership(ownership infrastructurev1.ResourceOwnership) bool {
	return ownership == infrastructurev1.OwnershipManaged ||
		ownership == infrastructurev1.OwnershipAdopted
}

func isUncertainCreationState(state infrastructurev1.MappingCreationState) bool {
	return state == infrastructurev1.MappingCreationPending ||
		state == infrastructurev1.MappingCreationOutcomeUnknown
}

func preserveMappingFailureState(
	status *infrastructurev1.HarnessGitopsProjectMappingStatus,
	request ProjectMappingRequest,
	startedCreationState infrastructurev1.MappingCreationState,
) {
	if status.Remote != nil && isDeletionOwnership(status.Remote.Ownership) {
		// A failed observation must not erase the cleanup identity for a remote
		// resource whose ownership was already proven.
		return
	}
	if isUncertainCreationState(startedCreationState) {
		status.CreationState = infrastructurev1.MappingCreationOutcomeUnknown
		status.Remote = unresolvedRemoteStatus(status.Remote, request)
		return
	}
	status.CreationState = ""
	status.Remote = remoteStatusForRequest(request)
}

func (r *Reconciler) setReady(
	ctx context.Context,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
	conditionStatus metav1.ConditionStatus,
	reason string,
	message string,
	mutate func(*infrastructurev1.HarnessGitopsProjectMappingStatus),
) error {
	before := mapping.DeepCopy().Status
	if mutate != nil {
		mutate(&mapping.Status)
	}
	mapping.Status.ObservedGeneration = mapping.Generation
	apiMeta.SetStatusCondition(&mapping.Status.Conditions, metav1.Condition{
		Type:               projectMappingReadyCondition,
		Status:             conditionStatus,
		ObservedGeneration: mapping.Generation,
		Reason:             reason,
		Message:            message,
	})
	if reflect.DeepEqual(before, mapping.Status) {
		return nil
	}
	return r.Status().Update(ctx, mapping)
}

func (r *Reconciler) returnAPIError(
	ctx context.Context,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
	message string,
	err error,
) (ctrl.Result, error) {
	statusErr := r.setReady(
		ctx,
		mapping,
		metav1.ConditionFalse,
		projectMappingReasonVerificationFailed,
		message,
		nil,
	)
	return ctrl.Result{}, errors.Join(err, statusErr)
}

func (r *Reconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *Reconciler) sessionForAgent(
	ctx context.Context,
	agent *infrastructurev1.HarnessGitopsAgent,
) (*harnessapi.Session, error) {
	secretNamespace := strings.TrimSpace(r.APIKeySecretNamespace)
	if secretNamespace == "" {
		secretNamespace = agent.Namespace
	}
	return harnessapi.SessionFromSecret(ctx, r.apiReader(), client.ObjectKey{
		Name:      agent.Spec.ApiKeySecretRef,
		Namespace: secretNamespace,
	})
}

func (r *Reconciler) projectMappingAPI() mappingReconcileAPI {
	if r.mappingAPI != nil {
		return r.mappingAPI
	}
	return SDKProjectMappingAPI{}
}

func (r *Reconciler) pendingRetryInterval() time.Duration {
	if r.AppProjectPendingRetryInterval > 0 {
		return r.AppProjectPendingRetryInterval
	}
	return DefaultAppProjectPendingRetryInterval
}

func (r *Reconciler) resyncInterval() time.Duration {
	if r.HarnessMappingResyncInterval > 0 {
		return r.HarnessMappingResyncInterval
	}
	return DefaultHarnessMappingResyncInterval
}
