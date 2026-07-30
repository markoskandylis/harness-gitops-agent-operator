package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const (
	harnessAgentReadyCondition           = "Ready"
	harnessAgentReasonWaitingForMappings = "WaitingForMappings"
	agentMappingDependencyRetryInterval  = 10 * time.Second
)

// reconcileMappingDependenciesForDeletion starts deletion of every Mapping CR
// that references agent and waits until a fresh API read proves they are gone.
// A true done result means the Agent deletion reconcile must return immediately.
func (r *Reconciler) reconcileMappingDependenciesForDeletion(
	ctx context.Context,
	agent *infrastructurev1.HarnessGitopsAgent,
) (result ctrl.Result, done bool, err error) {
	if r.APIReader == nil {
		return ctrl.Result{}, true, fmt.Errorf(
			"cannot verify Mapping dependencies for Agent %s/%s: APIReader is not configured",
			agent.Namespace,
			agent.Name,
		)
	}

	mappings := &infrastructurev1.HarnessGitopsProjectMappingList{}
	if err := r.APIReader.List(ctx, mappings, client.InNamespace(agent.Namespace)); err != nil {
		return ctrl.Result{}, true, fmt.Errorf(
			"list Mapping dependencies for Agent %s/%s: %w",
			agent.Namespace,
			agent.Name,
			err,
		)
	}

	references := make([]*infrastructurev1.HarnessGitopsProjectMapping, 0)
	for i := range mappings.Items {
		mapping := &mappings.Items[i]
		if strings.TrimSpace(mapping.Spec.AgentRef.Name) == agent.Name {
			references = append(references, mapping)
		}
	}
	if len(references) == 0 {
		return ctrl.Result{}, false, nil
	}

	sort.Slice(references, func(i, j int) bool {
		return references[i].Name < references[j].Name
	})

	names := make([]string, 0, len(references))
	var deleteErr error
	for _, mapping := range references {
		names = append(names, mapping.Name)
		if !mapping.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, mapping); err != nil {
			deleteErr = errors.Join(
				deleteErr,
				fmt.Errorf("delete Mapping %s/%s: %w", mapping.Namespace, mapping.Name, err),
			)
		}
	}

	statusErr := r.setAgentWaitingForMappings(ctx, agent, names)
	return ctrl.Result{RequeueAfter: agentMappingDependencyRetryInterval},
		true,
		errors.Join(deleteErr, statusErr)
}

func (r *Reconciler) setAgentWaitingForMappings(
	ctx context.Context,
	agent *infrastructurev1.HarnessGitopsAgent,
	names []string,
) error {
	before := agent.DeepCopy().Status
	apiMeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:               harnessAgentReadyCondition,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: agent.Generation,
		Reason:             harnessAgentReasonWaitingForMappings,
		Message: fmt.Sprintf(
			"Waiting for HarnessGitopsProjectMapping resources to be deleted: %s",
			strings.Join(names, ", "),
		),
	})
	if reflect.DeepEqual(before, agent.Status) {
		return nil
	}
	return r.Status().Update(ctx, agent)
}
