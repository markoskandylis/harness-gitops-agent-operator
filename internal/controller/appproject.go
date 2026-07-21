package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
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

func newAppProjectObject(namespace string, name string) *unstructured.Unstructured {
	appProject := &unstructured.Unstructured{}
	appProject.SetGroupVersionKind(appProjectGVK)
	appProject.SetNamespace(namespace)
	appProject.SetName(name)
	return appProject
}

func (r *HarnessGitopsAgentReconciler) appProjectExists(
	ctx context.Context,
	namespace string,
	name string,
) (bool, error) {
	if r.appProjectClient != nil {
		_, err := r.appProjectClient.Resource(appProjectGVR).Namespace(namespace).Get(
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
	err := r.Get(ctx, client.ObjectKeyFromObject(appProject), appProject)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get AppProject %s/%s: %w", namespace, name, err)
	}
	return true, nil
}

// SetupWithManager configures polling-based AppProject reconciliation. The
// dynamic client keeps manager startup independent of the optional AppProject CRD.
func (r *HarnessGitopsAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	dynamicClient, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("create AppProject dynamic client: %w", err)
	}
	r.appProjectClient = dynamicClient

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1.HarnessGitopsAgent{}).
		Owns(&corev1.Secret{}).
		Named("harnessgitopsagent").
		Complete(r)
}
