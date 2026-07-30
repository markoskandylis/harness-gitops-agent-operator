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
	"sort"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

const projectMappingReasonOwnershipConflict = "OwnershipConflict"

type projectMappingClaimPriority int

const (
	projectMappingClaimOwned projectMappingClaimPriority = iota
	projectMappingClaimCreateCandidate
	projectMappingClaimExternal
	projectMappingClaimAdoption
)

type projectMappingClaim struct {
	resource *infrastructurev1.HarnessGitopsProjectMapping
	priority projectMappingClaimPriority
}

type projectMappingClaimDecision struct {
	currentWins bool
	winner      projectMappingClaim
}

// requireProjectMappingClaim prevents two Kubernetes resources from claiming
// the same Harness mapping. The uncached API reader makes the decision from a
// cluster-wide view; deterministic ordering also closes concurrent races.
func (r *Reconciler) requireProjectMappingClaim(
	ctx context.Context,
	current *infrastructurev1.HarnessGitopsProjectMapping,
	request ProjectMappingRequest,
	mappingID string,
) (bool, ctrl.Result, error) {
	return r.requireProjectMappingClaimWithProvisional(
		ctx,
		current,
		request,
		mappingID,
		nil,
		nil,
	)
}

func (r *Reconciler) requireNewExternalProjectMappingClaim(
	ctx context.Context,
	current *infrastructurev1.HarnessGitopsProjectMapping,
	request ProjectMappingRequest,
	mappingID string,
) (bool, ctrl.Result, error) {
	priority := projectMappingClaimExternal
	return r.requireProjectMappingClaimWithProvisional(
		ctx,
		current,
		request,
		mappingID,
		&priority,
		func(status *infrastructurev1.HarnessGitopsProjectMappingStatus) {
			if status.Remote != nil &&
				strings.TrimSpace(status.Remote.MappingID) == strings.TrimSpace(mappingID) &&
				status.Remote.Ownership == infrastructurev1.OwnershipExternal {
				status.Remote = status.Remote.DeepCopy()
				status.Remote.Ownership = ""
			}
		},
	)
}

func (r *Reconciler) requireProjectMappingClaimWithProvisional(
	ctx context.Context,
	current *infrastructurev1.HarnessGitopsProjectMapping,
	request ProjectMappingRequest,
	mappingID string,
	provisionalPriority *projectMappingClaimPriority,
	onConflict func(*infrastructurev1.HarnessGitopsProjectMappingStatus),
) (bool, ctrl.Result, error) {
	decision, err := r.resolveProjectMappingClaimWithProvisional(
		ctx,
		current,
		request,
		mappingID,
		provisionalPriority,
	)
	if err != nil {
		statusErr := r.setReady(
			ctx,
			current,
			metav1.ConditionFalse,
			projectMappingReasonOwnershipConflict,
			fmt.Sprintf(
				"Unable to verify exclusive ownership of Harness mapping %q",
				strings.TrimSpace(mappingID),
			),
			nil,
		)
		return false, ctrl.Result{}, errors.Join(err, statusErr)
	}
	if decision.currentWins {
		return true, ctrl.Result{}, nil
	}

	winner := decision.winner.resource
	statusErr := r.setReady(
		ctx,
		current,
		metav1.ConditionFalse,
		projectMappingReasonOwnershipConflict,
		fmt.Sprintf(
			"Harness mapping %q is claimed by HarnessGitopsProjectMapping %s/%s (%s)",
			strings.TrimSpace(mappingID),
			winner.Namespace,
			winner.Name,
			decision.winner.priority,
		),
		onConflict,
	)
	return false, ctrl.Result{RequeueAfter: r.resyncInterval()}, statusErr
}

func (r *Reconciler) resolveProjectMappingClaim(
	ctx context.Context,
	current *infrastructurev1.HarnessGitopsProjectMapping,
	request ProjectMappingRequest,
	mappingID string,
) (projectMappingClaimDecision, error) {
	return r.resolveProjectMappingClaimWithProvisional(
		ctx,
		current,
		request,
		mappingID,
		nil,
	)
}

func (r *Reconciler) resolveProjectMappingClaimWithProvisional(
	ctx context.Context,
	current *infrastructurev1.HarnessGitopsProjectMapping,
	request ProjectMappingRequest,
	mappingID string,
	provisionalPriority *projectMappingClaimPriority,
) (projectMappingClaimDecision, error) {
	mappingID = strings.TrimSpace(mappingID)
	if mappingID == "" {
		return projectMappingClaimDecision{}, fmt.Errorf(
			"resolve project mapping claim: mapping ID is empty",
		)
	}
	if r.APIReader == nil {
		return projectMappingClaimDecision{}, fmt.Errorf(
			"resolve project mapping claim: APIReader is not configured",
		)
	}

	resources := &infrastructurev1.HarnessGitopsProjectMappingList{}
	if err := r.APIReader.List(ctx, resources); err != nil {
		return projectMappingClaimDecision{}, fmt.Errorf(
			"list HarnessGitopsProjectMapping claims: %w",
			err,
		)
	}

	claims := make([]projectMappingClaim, 0, len(resources.Items))
	currentSeen := false
	for index := range resources.Items {
		candidate := &resources.Items[index]
		isCurrent := sameProjectMappingResource(candidate, current)
		currentSeen = currentSeen || isCurrent
		priority, eligible, err := r.projectMappingClaimPriority(
			ctx,
			current,
			candidate,
			request,
			mappingID,
			r.APIReader,
		)
		if err != nil {
			return projectMappingClaimDecision{}, err
		}
		if !eligible && provisionalPriority != nil {
			switch {
			case isCurrent:
				priority = *provisionalPriority
				eligible = true
			case *provisionalPriority == projectMappingClaimExternal:
				eligible, err = r.isProvisionalExternalProjectMappingClaim(
					ctx,
					candidate,
					request,
					r.APIReader,
				)
				if err != nil {
					return projectMappingClaimDecision{}, err
				}
				priority = *provisionalPriority
			}
		}
		if eligible {
			claims = append(claims, projectMappingClaim{
				resource: candidate,
				priority: priority,
			})
		}
	}
	if !currentSeen && provisionalPriority != nil {
		claims = append(claims, projectMappingClaim{
			resource: current.DeepCopy(),
			priority: *provisionalPriority,
		})
	}
	if len(claims) == 0 {
		return projectMappingClaimDecision{}, fmt.Errorf(
			"no eligible Kubernetes claim exists for Harness mapping %q",
			mappingID,
		)
	}

	sort.Slice(claims, func(left, right int) bool {
		return projectMappingClaimLess(claims[left], claims[right])
	})
	winner := claims[0]
	return projectMappingClaimDecision{
		currentWins: sameProjectMappingResource(winner.resource, current),
		winner:      winner,
	}, nil
}

func (r *Reconciler) projectMappingClaimPriority(
	ctx context.Context,
	current *infrastructurev1.HarnessGitopsProjectMapping,
	candidate *infrastructurev1.HarnessGitopsProjectMapping,
	request ProjectMappingRequest,
	mappingID string,
	reader client.Reader,
) (projectMappingClaimPriority, bool, error) {
	if candidate.Status.Remote != nil &&
		strings.TrimSpace(candidate.Status.Remote.MappingID) == mappingID {
		switch {
		case isDeletionOwnership(candidate.Status.Remote.Ownership):
			return projectMappingClaimOwned, true, nil
		case isUncertainCreationState(candidate.Status.CreationState):
			return projectMappingClaimCreateCandidate, true, nil
		case candidate.Status.Remote.Ownership == infrastructurev1.OwnershipExternal:
			// A remote ID has one Kubernetes binding even if its tuple later
			// drifts out of band. Delete the External binding before another
			// resource can adopt that ID.
			return projectMappingClaimExternal, true, nil
		}
	}

	if strings.TrimSpace(candidate.Spec.AdoptMappingID) != mappingID {
		return 0, false, nil
	}
	if sameProjectMappingResource(candidate, current) {
		return projectMappingClaimAdoption, true, nil
	}

	candidateRequest, resolved, err := resolveProjectMappingClaimRequest(
		ctx,
		candidate,
		reader,
	)
	if err != nil || !resolved {
		return 0, false, err
	}
	if !projectMappingRequestsEquivalent(candidateRequest, request) {
		return 0, false, nil
	}
	return projectMappingClaimAdoption, true, nil
}

func (r *Reconciler) isProvisionalExternalProjectMappingClaim(
	ctx context.Context,
	candidate *infrastructurev1.HarnessGitopsProjectMapping,
	request ProjectMappingRequest,
	reader client.Reader,
) (bool, error) {
	if candidate.Status.Remote != nil ||
		candidate.Status.CreationState != "" ||
		strings.TrimSpace(candidate.Spec.AdoptMappingID) != "" {
		return false, nil
	}

	candidateRequest, resolved, err := resolveProjectMappingClaimRequest(
		ctx,
		candidate,
		reader,
	)
	if err != nil || !resolved {
		return false, err
	}
	if !projectMappingRequestsEquivalent(candidateRequest, request) {
		return false, nil
	}
	return true, nil
}

func resolveProjectMappingClaimRequest(
	ctx context.Context,
	candidate *infrastructurev1.HarnessGitopsProjectMapping,
	reader client.Reader,
) (ProjectMappingRequest, bool, error) {
	agentName := strings.TrimSpace(candidate.Spec.AgentRef.Name)
	if agentName == "" {
		return ProjectMappingRequest{}, false, nil
	}
	agent := &infrastructurev1.HarnessGitopsAgent{}
	if err := reader.Get(ctx, client.ObjectKey{
		Namespace: candidate.Namespace,
		Name:      agentName,
	}, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			return ProjectMappingRequest{}, false, nil
		}
		return ProjectMappingRequest{}, false, fmt.Errorf(
			"read Agent for project mapping claim %s/%s: %w",
			candidate.Namespace,
			candidate.Name,
			err,
		)
	}

	candidateRequest, err := resolveProjectMappingRequest(agent, candidate)
	if err != nil {
		return ProjectMappingRequest{}, false, nil
	}
	return candidateRequest, true, nil
}

func projectMappingClaimLess(left projectMappingClaim, right projectMappingClaim) bool {
	if left.priority != right.priority {
		return left.priority < right.priority
	}
	leftResource := left.resource
	rightResource := right.resource
	if !leftResource.CreationTimestamp.Equal(&rightResource.CreationTimestamp) {
		return leftResource.CreationTimestamp.Before(&rightResource.CreationTimestamp)
	}
	if leftResource.Namespace != rightResource.Namespace {
		return leftResource.Namespace < rightResource.Namespace
	}
	if leftResource.Name != rightResource.Name {
		return leftResource.Name < rightResource.Name
	}
	return string(leftResource.UID) < string(rightResource.UID)
}

func sameProjectMappingResource(
	left *infrastructurev1.HarnessGitopsProjectMapping,
	right *infrastructurev1.HarnessGitopsProjectMapping,
) bool {
	if left == nil || right == nil ||
		left.Namespace != right.Namespace ||
		left.Name != right.Name {
		return false
	}
	if left.UID != "" && right.UID != "" {
		return left.UID == right.UID
	}
	return true
}

func projectMappingRequestsEquivalent(
	left ProjectMappingRequest,
	right ProjectMappingRequest,
) bool {
	leftScope := strings.ToUpper(strings.TrimSpace(left.AgentScope))
	rightScope := strings.ToUpper(strings.TrimSpace(right.AgentScope))
	return leftScope == rightScope &&
		strings.TrimSpace(left.AccountIdentifier) == strings.TrimSpace(right.AccountIdentifier) &&
		harnessapi.IdentifiersEquivalent(
			leftScope,
			left.AgentIdentifier,
			right.AgentIdentifier,
		) &&
		strings.TrimSpace(left.Agent.OrgIdentifier) == strings.TrimSpace(right.Agent.OrgIdentifier) &&
		strings.TrimSpace(left.Agent.ProjectIdentifier) == strings.TrimSpace(right.Agent.ProjectIdentifier) &&
		strings.TrimSpace(left.Mapping.OrgIdentifier) == strings.TrimSpace(right.Mapping.OrgIdentifier) &&
		strings.TrimSpace(left.Mapping.ProjectIdentifier) == strings.TrimSpace(right.Mapping.ProjectIdentifier) &&
		strings.TrimSpace(left.ArgoProjectName) == strings.TrimSpace(right.ArgoProjectName) &&
		left.AutoCreateServiceEnv == right.AutoCreateServiceEnv
}

func (priority projectMappingClaimPriority) String() string {
	switch priority {
	case projectMappingClaimOwned:
		return "established owner"
	case projectMappingClaimCreateCandidate:
		return "unresolved create candidate"
	case projectMappingClaimExternal:
		return "existing external binding"
	case projectMappingClaimAdoption:
		return "adoption request"
	default:
		return "unknown claim"
	}
}
