package projectmapping

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
