package controller

import (
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	resourceutil "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource"
	agentcontroller "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource/agent"
	mappingcontroller "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource/projectmapping"
)

const (
	ManagedByLabelKey   = agentcontroller.ManagedByLabelKey
	ManagedByLabelValue = agentcontroller.ManagedByLabelValue

	DefaultAppProjectPendingRetryInterval = mappingcontroller.DefaultAppProjectPendingRetryInterval
	DefaultHarnessMappingResyncInterval   = mappingcontroller.DefaultHarnessMappingResyncInterval
)

// Options contains shared configuration for both resource controllers.
type Options struct {
	ManagedNamespaces              []string
	AppProjectPendingRetryInterval time.Duration
	HarnessMappingResyncInterval   time.Duration
}

// ValidateMappingIntervals rejects reconcile intervals that would hot-loop.
func ValidateMappingIntervals(pendingRetry, harnessResync time.Duration) error {
	return mappingcontroller.ValidateMappingIntervals(pendingRetry, harnessResync)
}

// SetupWithManager registers both controllers with one manager.
func SetupWithManager(mgr ctrl.Manager, options Options) error {
	policy, err := resourceutil.NewNamespacePolicy(options.ManagedNamespaces)
	if err != nil {
		return fmt.Errorf("configure managed namespaces: %w", err)
	}
	if err := (&mappingcontroller.Reconciler{
		Client:                         mgr.GetClient(),
		APIReader:                      mgr.GetAPIReader(),
		NamespacePolicy:                policy,
		AppProjectPendingRetryInterval: options.AppProjectPendingRetryInterval,
		HarnessMappingResyncInterval:   options.HarnessMappingResyncInterval,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up HarnessGitopsProjectMapping controller: %w", err)
	}

	if err := (&agentcontroller.Reconciler{
		Client:          mgr.GetClient(),
		APIReader:       mgr.GetAPIReader(),
		Scheme:          mgr.GetScheme(),
		NamespacePolicy: policy,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up HarnessGitopsAgent controller: %w", err)
	}
	return nil
}
