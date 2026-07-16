//go:build e2e
// +build e2e

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

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	ginkgotypes "github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const (
	liveTestNamespace    = "go-e2e-tests"
	liveAPIKeySecretName = "harness-api-key-secret"
	liveAPIKeySecretKey  = "api_key"
	liveTokenSecretKey   = "GITOPS_AGENT_TOKEN"
	liveAgentFinalizer   = "infrastructure.kandylis.co.uk/finalizer"
	liveRunIDLabel       = "infrastructure.kandylis.co.uk/e2e-run-id"
	liveOwnerIDLabel     = "infrastructure.kandylis.co.uk/e2e-owner-id"
	liveAgentCRDName     = "harnessgitopsagents.infrastructure.kandylis.co.uk"
	liveManagerContainer = "manager"
	liveCRDVersion       = "v1"
	liveRequestTimeout   = 30 * time.Second
	liveOwnerIDSuffixLen = 10
)

var liveRunIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,34}[a-z0-9])?$`)

type liveE2EConfig struct {
	accountID            string
	orgID                string
	projectID            string
	runID                string
	kubeconfig           string
	kubeconfigContext    string
	controllerNamespace  string
	controllerDeployment string
	controllerImage      string
	controllerLeaseName  string
}

type liveAgentFixture struct {
	scope           string
	runID           string
	namespace       string
	resourceName    string
	tokenSecretName string
	agentIdentifier string
	projectID       string
	ownerID         string
	namespaceUID    types.UID
	namespaceOwned  bool
	agentUID        types.UID
	agentOwned      bool
}

var _ = Describe("Live Harness agent registration", Ordered, Serial, Label("live"), func() {
	var (
		k8sClient      client.Client
		orgFixture     *liveAgentFixture
		projectFixture *liveAgentFixture
	)

	BeforeAll(func() {
		if os.Getenv("RUN_HARNESS_LIVE_E2E") != enabledEnvValue {
			if useExistingController || liveSpecsExplicitlySelected() {
				Fail("RUN_HARNESS_LIVE_E2E=true is required for live-selected E2E specs")
			}
			Skip("set RUN_HARNESS_LIVE_E2E=true to run credentialed Harness tests")
		}
		Expect(useExistingController).To(BeTrue(),
			"live registration requires E2E_USE_EXISTING_CONTROLLER=true")

		cfg, err := loadLiveE2EConfig()
		Expect(err).NotTo(HaveOccurred())

		scheme := runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(coordinationv1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(infrastructurev1.AddToScheme(scheme)).To(Succeed())

		restConfig, err := loadLiveKubernetesConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		restConfig.Timeout = liveRequestTimeout
		k8sClient, err = client.New(restConfig, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred(), "failed to create the E2E Kubernetes client")

		By("verifying the installed controller and HarnessGitopsAgent CRD")
		Eventually(func(g Gomega) {
			requestContext, cancel := context.WithTimeout(context.Background(), liveRequestTimeout)
			defer cancel()
			assertLiveControllerReady(requestContext, g, k8sClient, cfg)
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		ownerID, err := newLiveOwnerID()
		Expect(err).NotTo(HaveOccurred())
		orgFixture = newLiveAgentFixture("ORG", cfg.runID, "", ownerID)
		projectFixture = newLiveAgentFixture("PROJECT", cfg.runID, cfg.projectID, ownerID)

		By(fmt.Sprintf("creating the shared %s namespace and copying the local API key Secret", liveTestNamespace))
		Expect(createLiveNamespaceAndCopyAPISecret(
			context.Background(),
			k8sClient,
			orgFixture,
			cfg.controllerNamespace,
		)).To(Succeed())
		Expect(shareLiveNamespaceOwnership(orgFixture, projectFixture)).To(Succeed())

		for _, fixture := range []*liveAgentFixture{orgFixture, projectFixture} {
			By(fmt.Sprintf("creating the %s HarnessGitopsAgent", fixture.scope))
			Expect(createLiveAgent(
				context.Background(),
				k8sClient,
				cfg,
				fixture,
			)).To(Succeed())
		}
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() || k8sClient == nil {
			return
		}
		for _, fixture := range []*liveAgentFixture{projectFixture, orgFixture} {
			writeLiveFixtureDiagnostics(context.Background(), k8sClient, fixture)
		}
	})

	AfterAll(func() {
		if k8sClient == nil {
			return
		}

		cleanupFailures := cleanupLiveFixtures(k8sClient, projectFixture, orgFixture)
		if len(cleanupFailures) > 0 {
			Fail("live E2E cleanup failed; retained the shared namespace for controller recovery: " +
				strings.Join(cleanupFailures, "; "))
		}
	})

	It("registers ORG and PROJECT agents and writes owned token Secrets", func() {
		for _, fixture := range []*liveAgentFixture{orgFixture, projectFixture} {
			By(fmt.Sprintf("waiting for the %s agent registration", fixture.scope))
			Eventually(func(g Gomega) {
				requestContext, cancel := context.WithTimeout(context.Background(), liveRequestTimeout)
				defer cancel()
				assertLiveAgentRegistered(requestContext, g, k8sClient, fixture)
			}, 5*time.Minute, 2*time.Second).Should(Succeed())
		}
	})

	It("deletes PROJECT then ORG through the controller finalizer", func() {
		for _, fixture := range []*liveAgentFixture{projectFixture, orgFixture} {
			By(fmt.Sprintf("deleting the %s agent", fixture.scope))
			cleanupContext, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			err := deleteLiveAgentAndWait(cleanupContext, k8sClient, fixture)
			cancel()
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				requestContext, requestCancel := context.WithTimeout(context.Background(), liveRequestTimeout)
				defer requestCancel()
				secret := &corev1.Secret{}
				err := k8sClient.Get(requestContext, types.NamespacedName{
					Namespace: fixture.namespace,
					Name:      fixture.tokenSecretName,
				}, secret)
				return apierrors.IsNotFound(err)
			}, time.Minute, time.Second).Should(BeTrue(), "owned token Secret was not removed")
		}
	})
})

func loadLiveE2EConfig() (liveE2EConfig, error) {
	values := map[string]string{
		"HARNESS_ACCOUNT_ID":        strings.TrimSpace(os.Getenv("HARNESS_ACCOUNT_ID")),
		"HARNESS_ORG_ID":            strings.TrimSpace(os.Getenv("HARNESS_ORG_ID")),
		"HARNESS_PROJECT_ID":        strings.TrimSpace(os.Getenv("HARNESS_PROJECT_ID")),
		"HARNESS_E2E_RUN_ID":        strings.TrimSpace(os.Getenv("HARNESS_E2E_RUN_ID")),
		"KUBECONFIG":                strings.TrimSpace(os.Getenv("KUBECONFIG")),
		"E2E_KUBECONFIG_CONTEXT":    strings.TrimSpace(os.Getenv("E2E_KUBECONFIG_CONTEXT")),
		"E2E_CONTROLLER_NAMESPACE":  strings.TrimSpace(os.Getenv("E2E_CONTROLLER_NAMESPACE")),
		"E2E_CONTROLLER_IMAGE":      strings.TrimSpace(os.Getenv("E2E_CONTROLLER_IMAGE")),
		"E2E_CONTROLLER_LEASE_NAME": strings.TrimSpace(os.Getenv("E2E_CONTROLLER_LEASE_NAME")),
		"E2E_CONTROLLER_DEPLOYMENT": strings.TrimSpace(
			os.Getenv("E2E_CONTROLLER_DEPLOYMENT"),
		),
	}

	var missing []string
	for name, value := range values {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return liveE2EConfig{}, fmt.Errorf("missing required live E2E environment variables: %s", strings.Join(missing, ", "))
	}

	runID := values["HARNESS_E2E_RUN_ID"]
	if !liveRunIDPattern.MatchString(runID) {
		return liveE2EConfig{}, fmt.Errorf(
			"HARNESS_E2E_RUN_ID must be 1-36 lowercase letters, numbers, or internal hyphens",
		)
	}

	return liveE2EConfig{
		accountID:            values["HARNESS_ACCOUNT_ID"],
		orgID:                values["HARNESS_ORG_ID"],
		projectID:            values["HARNESS_PROJECT_ID"],
		runID:                runID,
		kubeconfig:           values["KUBECONFIG"],
		kubeconfigContext:    values["E2E_KUBECONFIG_CONTEXT"],
		controllerNamespace:  values["E2E_CONTROLLER_NAMESPACE"],
		controllerDeployment: values["E2E_CONTROLLER_DEPLOYMENT"],
		controllerImage:      values["E2E_CONTROLLER_IMAGE"],
		controllerLeaseName:  values["E2E_CONTROLLER_LEASE_NAME"],
	}, nil
}

func liveSpecsExplicitlySelected() bool {
	suiteConfig, _ := GinkgoConfiguration()
	if strings.TrimSpace(suiteConfig.LabelFilter) == "" {
		return false
	}

	filter, err := ginkgotypes.ParseLabelFilter(suiteConfig.LabelFilter)
	if err != nil {
		return false
	}
	return filter([]string{"live"})
}

func loadLiveKubernetesConfig(cfg liveE2EConfig) (*rest.Config, error) {
	kubeconfigPaths := filepath.SplitList(cfg.kubeconfig)
	if len(kubeconfigPaths) == 0 {
		return nil, fmt.Errorf("KUBECONFIG does not contain a path")
	}
	for _, kubeconfigPath := range kubeconfigPaths {
		if strings.TrimSpace(kubeconfigPath) == "" {
			return nil, fmt.Errorf("KUBECONFIG contains an empty path")
		}
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	// Restrict loading to the explicitly supplied files so the test cannot fall
	// back to a developer's default kubeconfig or an in-cluster service account.
	loadingRules.Precedence = kubeconfigPaths
	rawConfig, err := loadingRules.Load()
	if err != nil {
		return nil, fmt.Errorf("load KUBECONFIG: %w", err)
	}
	if rawConfig.CurrentContext != cfg.kubeconfigContext {
		return nil, fmt.Errorf(
			"KUBECONFIG current context is %q, expected %q",
			rawConfig.CurrentContext,
			cfg.kubeconfigContext,
		)
	}
	if _, found := rawConfig.Contexts[cfg.kubeconfigContext]; !found {
		return nil, fmt.Errorf("KUBECONFIG does not define context %q", cfg.kubeconfigContext)
	}

	overrides := &clientcmd.ConfigOverrides{CurrentContext: cfg.kubeconfigContext}
	restConfig, err := clientcmd.NewNonInteractiveClientConfig(
		*rawConfig,
		cfg.kubeconfigContext,
		overrides,
		loadingRules,
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes client config for context %q: %w", cfg.kubeconfigContext, err)
	}
	return restConfig, nil
}

func newLiveAgentFixture(scope string, runID string, projectID string, ownerID string) *liveAgentFixture {
	scopeName := strings.ToLower(scope)
	resourceName := scopeName + "-agent"
	harnessRunID := strings.ReplaceAll(runID, "-", "_")
	ownerSuffix := ownerID
	if len(ownerSuffix) > liveOwnerIDSuffixLen {
		ownerSuffix = ownerSuffix[:liveOwnerIDSuffixLen]
	}
	return &liveAgentFixture{
		scope:           scope,
		runID:           runID,
		namespace:       liveTestNamespace,
		resourceName:    resourceName,
		tokenSecretName: resourceName + "-token",
		agentIdentifier: fmt.Sprintf("hga_e2e_%s_%s_%s", scopeName, harnessRunID, ownerSuffix),
		projectID:       projectID,
		ownerID:         ownerID,
	}
}

func shareLiveNamespaceOwnership(owner *liveAgentFixture, fixture *liveAgentFixture) error {
	if owner == nil || fixture == nil {
		return fmt.Errorf("share live namespace ownership: fixture is nil")
	}
	if owner.namespace != fixture.namespace || owner.runID != fixture.runID || owner.ownerID != fixture.ownerID {
		return fmt.Errorf("share live namespace ownership: fixture execution does not match the owner")
	}
	if !owner.namespaceOwned || owner.namespaceUID == "" {
		return fmt.Errorf("share live namespace ownership: namespace %s is not owned", owner.namespace)
	}

	fixture.namespaceUID = owner.namespaceUID
	fixture.namespaceOwned = true
	return nil
}

func cleanupLiveFixtures(
	k8sClient client.Client,
	projectFixture *liveAgentFixture,
	orgFixture *liveAgentFixture,
) []string {
	var cleanupFailures []string
	allAgentsDeleted := true
	// Project is deleted first so an ORG-scoped agent remains available until its dependents are gone.
	for _, fixture := range []*liveAgentFixture{projectFixture, orgFixture} {
		if fixture == nil {
			continue
		}

		cleanupContext, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		err := deleteLiveAgentAndWait(cleanupContext, k8sClient, fixture)
		cancel()
		if err != nil {
			// Preserve the shared namespace and API Secret so the controller can retry its finalizer.
			cleanupFailures = append(cleanupFailures, err.Error())
			allAgentsDeleted = false
		}
	}

	// Both agents share one namespace. Delete it only after both finalizers complete.
	if allAgentsDeleted && orgFixture != nil {
		namespaceContext, namespaceCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := deleteOwnedLiveNamespaceAndWait(namespaceContext, k8sClient, orgFixture)
		namespaceCancel()
		if err != nil {
			cleanupFailures = append(cleanupFailures, err.Error())
		}
	}

	return cleanupFailures
}

// createLiveNamespaceAndCopyAPISecret mirrors the pipeline contract: the API
// credential is pre-provisioned locally, while the controller requires a copy
// in the CR namespace. Only the required key enters the disposable namespace.
func createLiveNamespaceAndCopyAPISecret(
	ctx context.Context,
	k8sClient client.Client,
	fixture *liveAgentFixture,
	sourceNamespace string,
) error {
	if sourceNamespace == fixture.namespace {
		return fmt.Errorf(
			"source API key Secret namespace %s must differ from the disposable E2E namespace",
			sourceNamespace,
		)
	}

	sourceSecret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: sourceNamespace,
		Name:      liveAPIKeySecretName,
	}, sourceSecret); err != nil {
		return fmt.Errorf(
			"get source API key Secret %s/%s: %w",
			sourceNamespace,
			liveAPIKeySecretName,
			err,
		)
	}
	apiKey, found := sourceSecret.Data[liveAPIKeySecretKey]
	if !found || len(apiKey) == 0 {
		return fmt.Errorf(
			"source API key Secret %s/%s must contain non-empty key %q",
			sourceNamespace,
			liveAPIKeySecretName,
			liveAPIKeySecretKey,
		)
	}

	labels := map[string]string{
		"pod-security.kubernetes.io/enforce": "restricted",
		liveRunIDLabel:                       fixture.runID,
		liveOwnerIDLabel:                     fixture.ownerID,
	}
	namespaceObject := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   fixture.namespace,
			Labels: labels,
		},
	}
	if err := k8sClient.Create(ctx, namespaceObject); err != nil {
		// A create can succeed server-side while the client receives a timeout. Claim
		// ownership only when the live object has this execution's labels.
		current := &corev1.Namespace{}
		getErr := k8sClient.Get(ctx, types.NamespacedName{Name: fixture.namespace}, current)
		if getErr != nil ||
			current.Labels[liveOwnerIDLabel] != fixture.ownerID ||
			current.Labels[liveRunIDLabel] != fixture.runID {
			return fmt.Errorf("create namespace %s without taking ownership: %w", fixture.namespace, err)
		}
		namespaceObject = current
	}
	if namespaceObject.UID == "" {
		return fmt.Errorf("namespace %s has no UID after creation", fixture.namespace)
	}
	fixture.namespaceUID = namespaceObject.UID
	fixture.namespaceOwned = true

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      liveAPIKeySecretName,
			Namespace: fixture.namespace,
			Labels: map[string]string{
				liveRunIDLabel:   labels[liveRunIDLabel],
				liveOwnerIDLabel: fixture.ownerID,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			liveAPIKeySecretKey: append([]byte(nil), apiKey...),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		current := &corev1.Secret{}
		getErr := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: fixture.namespace,
			Name:      liveAPIKeySecretName,
		}, current)
		if getErr != nil ||
			current.Labels[liveOwnerIDLabel] != fixture.ownerID ||
			current.Labels[liveRunIDLabel] != fixture.runID ||
			!bytes.Equal(current.Data[liveAPIKeySecretKey], apiKey) {
			return fmt.Errorf("create API key Secret in namespace %s: %w", fixture.namespace, err)
		}
	}
	return nil
}

func newLiveOwnerID() (string, error) {
	ownerBytes := make([]byte, 16)
	if _, err := rand.Read(ownerBytes); err != nil {
		return "", fmt.Errorf("generate live E2E owner ID: %w", err)
	}
	return hex.EncodeToString(ownerBytes), nil
}

func assertLiveControllerReady(
	ctx context.Context,
	g Gomega,
	k8sClient client.Client,
	cfg liveE2EConfig,
) {
	deployment := &appsv1.Deployment{}
	err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: cfg.controllerNamespace,
		Name:      cfg.controllerDeployment,
	}, deployment)
	g.Expect(err).NotTo(HaveOccurred(), "controller Deployment was not found")
	if err != nil {
		return
	}

	desiredReplicas := int32(1)
	if deployment.Spec.Replicas != nil {
		desiredReplicas = *deployment.Spec.Replicas
	}
	g.Expect(desiredReplicas).To(BeNumerically(">", 0), "controller Deployment is scaled to zero")
	g.Expect(deployment.Status.ObservedGeneration).To(
		BeNumerically(">=", deployment.Generation),
		"controller Deployment has not observed its current generation",
	)
	g.Expect(deployment.Status.ReadyReplicas).To(
		BeNumerically(">=", desiredReplicas),
		"controller Deployment does not have all desired replicas ready",
	)
	g.Expect(deployment.Status.UpdatedReplicas).To(
		BeNumerically(">=", desiredReplicas),
		"controller Deployment rollout has not updated all desired replicas",
	)
	g.Expect(deployment.Status.AvailableReplicas).To(
		BeNumerically(">=", desiredReplicas),
		"controller Deployment does not have all desired replicas available",
	)
	g.Expect(deployment.Status.UnavailableReplicas).To(
		BeZero(),
		"controller Deployment still has unavailable replicas",
	)

	available := false
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			available = true
			break
		}
	}
	g.Expect(available).To(BeTrue(), "controller Deployment is not Available")

	managerContainer, found := findContainer(deployment.Spec.Template.Spec.Containers, liveManagerContainer)
	g.Expect(found).To(BeTrue(), "controller Deployment has no manager container")
	if !found {
		return
	}
	g.Expect(managerContainer.Image).To(
		Equal(cfg.controllerImage),
		"controller Deployment is not running the expected tested image",
	)
	g.Expect(managerContainer.Args).To(
		ContainElement("--leader-elect"),
		"controller Deployment does not have leader election enabled",
	)

	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	g.Expect(err).NotTo(HaveOccurred(), "controller Deployment has an invalid pod selector")
	if err != nil {
		return
	}
	controllerPods := &corev1.PodList{}
	err = k8sClient.List(
		ctx,
		controllerPods,
		client.InNamespace(cfg.controllerNamespace),
		client.MatchingLabelsSelector{Selector: selector},
	)
	g.Expect(err).NotTo(HaveOccurred(), "could not list controller Deployment pods")
	if err != nil {
		return
	}

	lease := &coordinationv1.Lease{}
	err = k8sClient.Get(ctx, types.NamespacedName{
		Namespace: cfg.controllerNamespace,
		Name:      cfg.controllerLeaseName,
	}, lease)
	g.Expect(err).NotTo(HaveOccurred(), "controller leader-election Lease was not found")
	if err != nil {
		return
	}
	g.Expect(lease.Spec.HolderIdentity).NotTo(BeNil(), "controller Lease has no holder identity")
	if lease.Spec.HolderIdentity == nil {
		return
	}
	holderIdentity := strings.TrimSpace(*lease.Spec.HolderIdentity)
	g.Expect(holderIdentity).NotTo(BeEmpty(), "controller Lease has an empty holder identity")

	leaderIsExpectedPod := false
	for i := range controllerPods.Items {
		pod := &controllerPods.Items[i]
		if !leaseHolderMatchesPod(holderIdentity, pod.Name) {
			continue
		}
		podManager, hasManager := findContainer(pod.Spec.Containers, liveManagerContainer)
		leaderIsExpectedPod = hasManager &&
			podManager.Image == cfg.controllerImage &&
			pod.DeletionTimestamp.IsZero() &&
			podIsReady(pod)
		break
	}
	g.Expect(leaderIsExpectedPod).To(
		BeTrue(),
		"the expected controller Deployment does not hold the leader-election Lease",
	)

	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	})
	err = k8sClient.Get(ctx, types.NamespacedName{Name: liveAgentCRDName}, crd)
	g.Expect(err).NotTo(HaveOccurred(), "HarnessGitopsAgent CRD was not found")
	if err != nil {
		return
	}

	conditions, found, err := unstructured.NestedSlice(crd.Object, "status", "conditions")
	g.Expect(err).NotTo(HaveOccurred(), "could not read HarnessGitopsAgent CRD conditions")
	g.Expect(found).To(BeTrue(), "HarnessGitopsAgent CRD has no status conditions")
	if err != nil || !found {
		return
	}

	established := false
	for _, rawCondition := range conditions {
		condition, ok := rawCondition.(map[string]interface{})
		if !ok {
			continue
		}
		conditionType, _, _ := unstructured.NestedString(condition, "type")
		conditionStatus, _, _ := unstructured.NestedString(condition, "status")
		if conditionType == "Established" && conditionStatus == string(corev1.ConditionTrue) {
			established = true
			break
		}
	}
	g.Expect(established).To(BeTrue(), "HarnessGitopsAgent CRD is not Established")

	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	g.Expect(err).NotTo(HaveOccurred(), "could not read HarnessGitopsAgent CRD versions")
	g.Expect(found).To(BeTrue(), "HarnessGitopsAgent CRD has no versions")
	if err != nil || !found {
		return
	}
	versionReady := false
	for _, rawVersion := range versions {
		version, ok := rawVersion.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(version, "name")
		served, _, _ := unstructured.NestedBool(version, "served")
		storage, _, _ := unstructured.NestedBool(version, "storage")
		if name == liveCRDVersion && served && storage {
			versionReady = true
			break
		}
	}
	g.Expect(versionReady).To(
		BeTrue(),
		"HarnessGitopsAgent CRD v1 is not both served and the storage version",
	)
}

func findContainer(containers []corev1.Container, name string) (corev1.Container, bool) {
	for _, container := range containers {
		if container.Name == name {
			return container, true
		}
	}
	return corev1.Container{}, false
}

func leaseHolderMatchesPod(holderIdentity string, podName string) bool {
	return holderIdentity == podName || strings.HasPrefix(holderIdentity, podName+"_")
}

func podIsReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func buildLiveAgent(cfg liveE2EConfig, fixture *liveAgentFixture) *infrastructurev1.HarnessGitopsAgent {
	return &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fixture.resourceName,
			Namespace: fixture.namespace,
			Labels: map[string]string{
				liveRunIDLabel:   fixture.runID,
				liveOwnerIDLabel: fixture.ownerID,
			},
		},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{
			Name:            fixture.agentIdentifier,
			Identifier:      fixture.agentIdentifier,
			AccountId:       cfg.accountID,
			OrgId:           cfg.orgID,
			ProjectId:       fixture.projectID,
			Operator:        "ARGO",
			Scope:           fixture.scope,
			Type:            "MANAGED_ARGO_PROVIDER",
			ApiKeySecretRef: liveAPIKeySecretName,
			TokenSecretRef:  fixture.tokenSecretName,
		},
	}
}

func createLiveAgent(
	ctx context.Context,
	k8sClient client.Client,
	cfg liveE2EConfig,
	fixture *liveAgentFixture,
) error {
	agent := buildLiveAgent(cfg, fixture)
	if err := k8sClient.Create(ctx, agent); err != nil {
		// As with namespaces, accept an ambiguous create only when the live object
		// carries this execution's labels.
		current := &infrastructurev1.HarnessGitopsAgent{}
		getErr := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: fixture.namespace,
			Name:      fixture.resourceName,
		}, current)
		if getErr != nil ||
			current.Labels[liveOwnerIDLabel] != fixture.ownerID ||
			current.Labels[liveRunIDLabel] != fixture.runID {
			return fmt.Errorf("create %s agent without taking ownership: %w", fixture.scope, err)
		}
		agent = current
	}
	if agent.UID == "" {
		return fmt.Errorf("%s agent has no UID after creation", fixture.scope)
	}

	fixture.agentUID = agent.UID
	fixture.agentOwned = true
	return nil
}

func assertLiveAgentRegistered(
	ctx context.Context,
	g Gomega,
	k8sClient client.Client,
	fixture *liveAgentFixture,
) {
	agent := &infrastructurev1.HarnessGitopsAgent{}
	err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: fixture.namespace,
		Name:      fixture.resourceName,
	}, agent)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(agent.Finalizers).To(ContainElement(liveAgentFinalizer))
	g.Expect(agent.UID).To(Equal(fixture.agentUID))
	g.Expect(agent.Labels[liveOwnerIDLabel]).To(Equal(fixture.ownerID))
	g.Expect(agent.Labels[liveRunIDLabel]).To(Equal(fixture.runID))
	g.Expect(strings.TrimSpace(agent.Status.AgentIdentifier)).NotTo(BeEmpty())
	g.Expect(agent.Spec.ApiKeySecretRef).To(Equal(liveAPIKeySecretName))
	g.Expect(agent.Spec.TokenSecretRef).To(Equal(fixture.tokenSecretName))
	if fixture.scope == "ORG" {
		g.Expect(agent.Spec.ProjectId).To(BeEmpty())
	} else {
		g.Expect(agent.Spec.ProjectId).To(Equal(fixture.projectID))
	}

	tokenSecret := &corev1.Secret{}
	err = k8sClient.Get(ctx, types.NamespacedName{
		Namespace: fixture.namespace,
		Name:      fixture.tokenSecretName,
	}, tokenSecret)
	g.Expect(err).NotTo(HaveOccurred())
	token, found := tokenSecret.Data[liveTokenSecretKey]
	g.Expect(found).To(BeTrue(), "token Secret does not contain the required key")
	g.Expect(token).NotTo(BeEmpty(), "token Secret key is empty")
	g.Expect(metav1.IsControlledBy(tokenSecret, agent)).To(BeTrue(), "token Secret is not owned by its HarnessGitopsAgent")
}

func deleteLiveAgentAndWait(
	ctx context.Context,
	k8sClient client.Client,
	fixture *liveAgentFixture,
) error {
	if fixture == nil || !fixture.namespaceOwned {
		return nil
	}

	namespaceObject, err := getOwnedLiveNamespace(ctx, k8sClient, fixture)
	if err != nil {
		return err
	}
	if namespaceObject == nil {
		return nil
	}

	key := types.NamespacedName{Namespace: fixture.namespace, Name: fixture.resourceName}
	agent := &infrastructurev1.HarnessGitopsAgent{}
	if err = k8sClient.Get(ctx, key, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get %s agent before deletion: %w", fixture.scope, err)
	}
	if !fixture.agentOwned {
		// Recover ownership after an ambiguous create response only when the
		// execution labels prove that this run created the object.
		if agent.UID == "" ||
			agent.Labels[liveOwnerIDLabel] != fixture.ownerID ||
			agent.Labels[liveRunIDLabel] != fixture.runID {
			return fmt.Errorf(
				"refusing deletion of unowned %s agent in the live E2E namespace",
				fixture.scope,
			)
		}
		fixture.agentUID = agent.UID
		fixture.agentOwned = true
	}
	if agent.UID != fixture.agentUID ||
		agent.Labels[liveOwnerIDLabel] != fixture.ownerID ||
		agent.Labels[liveRunIDLabel] != fixture.runID {
		return fmt.Errorf(
			"refusing deletion of %s agent because its execution ownership changed",
			fixture.scope,
		)
	}

	if agent.DeletionTimestamp.IsZero() {
		if err := k8sClient.Delete(ctx, agent, client.Preconditions{UID: &fixture.agentUID}); err != nil &&
			!apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s agent: %w", fixture.scope, err)
		}
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		current := &infrastructurev1.HarnessGitopsAgent{}
		err := k8sClient.Get(ctx, key, current)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wait for %s agent deletion: %w", fixture.scope, err)
		}
		if current.UID != fixture.agentUID ||
			current.Labels[liveOwnerIDLabel] != fixture.ownerID ||
			current.Labels[liveRunIDLabel] != fixture.runID {
			return fmt.Errorf(
				"%s agent was replaced while waiting for deletion; refusing further cleanup",
				fixture.scope,
			)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s agent deletion: %w (finalizers: %v)", fixture.scope, ctx.Err(), current.Finalizers)
		case <-ticker.C:
		}
	}
}

func getOwnedLiveNamespace(
	ctx context.Context,
	k8sClient client.Client,
	fixture *liveAgentFixture,
) (*corev1.Namespace, error) {
	namespaceObject := &corev1.Namespace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: fixture.namespace}, namespaceObject); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get namespace %s before cleanup: %w", fixture.namespace, err)
	}

	if namespaceObject.UID != fixture.namespaceUID ||
		namespaceObject.Labels[liveOwnerIDLabel] != fixture.ownerID ||
		namespaceObject.Labels[liveRunIDLabel] != fixture.runID {
		return nil, fmt.Errorf(
			"refusing cleanup of namespace %s because its execution ownership changed",
			fixture.namespace,
		)
	}
	return namespaceObject, nil
}

func deleteOwnedLiveNamespaceAndWait(
	ctx context.Context,
	k8sClient client.Client,
	fixture *liveAgentFixture,
) error {
	if fixture == nil || !fixture.namespaceOwned {
		return nil
	}

	namespaceObject, err := getOwnedLiveNamespace(ctx, k8sClient, fixture)
	if err != nil || namespaceObject == nil {
		return err
	}

	namespaceUID := fixture.namespaceUID
	if err := k8sClient.Delete(
		ctx,
		namespaceObject,
		client.Preconditions{UID: &namespaceUID},
	); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", fixture.namespace, err)
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		current := &corev1.Namespace{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: fixture.namespace}, current)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wait for namespace %s deletion: %w", fixture.namespace, err)
		}
		if current.UID != fixture.namespaceUID ||
			current.Labels[liveOwnerIDLabel] != fixture.ownerID ||
			current.Labels[liveRunIDLabel] != fixture.runID {
			return fmt.Errorf(
				"namespace %s was replaced while waiting for deletion; refusing further cleanup",
				fixture.namespace,
			)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for namespace %s deletion: %w", fixture.namespace, ctx.Err())
		case <-ticker.C:
		}
	}
}

func writeLiveFixtureDiagnostics(ctx context.Context, k8sClient client.Client, fixture *liveAgentFixture) {
	if fixture == nil || !fixture.namespaceOwned {
		return
	}

	agent := &infrastructurev1.HarnessGitopsAgent{}
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: fixture.namespace,
		Name:      fixture.resourceName,
	}, agent); err == nil {
		_, _ = fmt.Fprintf(
			GinkgoWriter,
			"live %s agent diagnostics: namespace=%s statusAgent=%s finalizers=%v\n",
			fixture.scope,
			fixture.namespace,
			agent.Status.AgentIdentifier,
			agent.Finalizers,
		)
	}

	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: fixture.namespace,
		Name:      fixture.tokenSecretName,
	}, secret); err == nil {
		keys := make([]string, 0, len(secret.Data))
		for key := range secret.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		_, _ = fmt.Fprintf(GinkgoWriter, "live %s token Secret keys: %v\n", fixture.scope, keys)
	}
}
