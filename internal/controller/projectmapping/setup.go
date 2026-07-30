package projectmapping

import (
	"context"
	"fmt"

	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

// SetupWithManager registers the Mapping controller and its Agent watch.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := registerProjectMappingAgentRefIndex(
		context.Background(),
		mgr.GetFieldIndexer(),
	); err != nil {
		return fmt.Errorf("register project Mapping Agent index: %w", err)
	}

	appProjectClient, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("create AppProject dynamic client: %w", err)
	}
	r.appProjectClient = appProjectClient

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1.HarnessGitopsProjectMapping{}).
		Watches(
			&infrastructurev1.HarnessGitopsAgent{},
			handler.EnqueueRequestsFromMapFunc(
				agentToProjectMappingRequests(mgr.GetClient()),
			),
		).
		Named("harnessgitopsprojectmapping").
		Complete(r)
}
