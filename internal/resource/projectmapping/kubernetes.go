package projectmapping

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const (
	DefaultAppProjectPendingRetryInterval = 20 * time.Second
	DefaultHarnessMappingResyncInterval   = 5 * time.Minute
	MinimumMappingInterval                = time.Second
	immediateRequeueInterval              = time.Nanosecond
	projectMappingAgentRefIndexField      = ".spec.agentRef.name"
)

var (
	appProjectGVK = schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "AppProject",
	}
	appProjectGVR = schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "appprojects",
	}
)

func ValidateMappingIntervals(pendingRetry, harnessResync time.Duration) error {
	if pendingRetry < MinimumMappingInterval {
		return fmt.Errorf(
			"appProject pending retry interval must be at least %s",
			MinimumMappingInterval,
		)
	}
	if harnessResync < MinimumMappingInterval {
		return fmt.Errorf(
			"harness mapping resync interval must be at least %s",
			MinimumMappingInterval,
		)
	}
	return nil
}

func newAppProjectObject(namespace string, name string) *unstructured.Unstructured {
	appProject := &unstructured.Unstructured{}
	appProject.SetGroupVersionKind(appProjectGVK)
	appProject.SetNamespace(namespace)
	appProject.SetName(name)
	return appProject
}

func appProjectExists(
	ctx context.Context,
	reader client.Reader,
	dynamicClient dynamic.Interface,
	namespace string,
	name string,
) (bool, error) {
	if dynamicClient != nil {
		_, err := dynamicClient.Resource(appProjectGVR).Namespace(namespace).Get(
			ctx,
			name,
			metav1.GetOptions{},
		)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("get AppProject %s/%s: %w", namespace, name, err)
		}
		return true, nil
	}

	// Unit tests can use the controller-runtime fake client without a REST config.
	appProject := newAppProjectObject(namespace, name)
	err := reader.Get(ctx, client.ObjectKeyFromObject(appProject), appProject)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get AppProject %s/%s: %w", namespace, name, err)
	}
	return true, nil
}

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
