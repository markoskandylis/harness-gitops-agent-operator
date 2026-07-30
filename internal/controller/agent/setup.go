package agent

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

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
