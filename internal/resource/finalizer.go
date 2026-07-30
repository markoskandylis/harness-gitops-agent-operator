package resource

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// EnsureFinalizer persists finalizer before reconciliation can create a remote
// resource, then forces an immediate refetch of the durable object.
func EnsureFinalizer(ctx context.Context, writer client.Writer, object client.Object, finalizer string) (ctrl.Result, bool, error) {
	if controllerutil.ContainsFinalizer(object, finalizer) {
		return ctrl.Result{}, false, nil
	}

	controllerutil.AddFinalizer(object, finalizer)
	if err := writer.Update(ctx, object); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: time.Nanosecond}, true, nil
}

// RemoveFinalizer persists finalizer removal after resource-specific cleanup.
func RemoveFinalizer(ctx context.Context, writer client.Writer, object client.Object, finalizer string) error {
	controllerutil.RemoveFinalizer(object, finalizer)
	return writer.Update(ctx, object)
}
