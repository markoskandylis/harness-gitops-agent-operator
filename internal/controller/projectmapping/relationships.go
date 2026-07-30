package projectmapping

import (
	"context"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const projectMappingAgentRefIndexField = ".spec.agentRef.name"

func registerProjectMappingAgentRefIndex(
	ctx context.Context,
	indexer client.FieldIndexer,
) error {
	return indexer.IndexField(
		ctx,
		&infrastructurev1.HarnessGitopsProjectMapping{},
		projectMappingAgentRefIndexField,
		projectMappingAgentRefIndexValues,
	)
}

func projectMappingAgentRefIndexValues(object client.Object) []string {
	mapping, ok := object.(*infrastructurev1.HarnessGitopsProjectMapping)
	if !ok {
		return nil
	}
	agentName := strings.TrimSpace(mapping.Spec.AgentRef.Name)
	if agentName == "" {
		return nil
	}
	return []string{agentName}
}

func agentToProjectMappingRequests(mappingClient client.Client) handler.MapFunc {
	return func(ctx context.Context, object client.Object) []reconcile.Request {
		agent, ok := object.(*infrastructurev1.HarnessGitopsAgent)
		if !ok {
			return nil
		}

		mappings := &infrastructurev1.HarnessGitopsProjectMappingList{}
		if err := mappingClient.List(
			ctx,
			mappings,
			client.InNamespace(agent.Namespace),
			client.MatchingFields{
				projectMappingAgentRefIndexField: agent.Name,
			},
		); err != nil {
			logf.FromContext(ctx).Error(
				err,
				"Unable to list project mappings for Agent event",
				"agent", client.ObjectKeyFromObject(agent),
			)
			return nil
		}

		requests := make([]reconcile.Request, 0, len(mappings.Items))
		for index := range mappings.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&mappings.Items[index]),
			})
		}
		sort.Slice(requests, func(left, right int) bool {
			if requests[left].Namespace != requests[right].Namespace {
				return requests[left].Namespace < requests[right].Namespace
			}
			return requests[left].Name < requests[right].Name
		})
		return requests
	}
}
