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
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

type recordingDeleteClient struct {
	client.Client
	deletions []string
}

func (c *recordingDeleteClient) Delete(
	ctx context.Context,
	obj client.Object,
	opts ...client.DeleteOption,
) error {
	switch obj.(type) {
	case *infrastructurev1.HarnessGitopsAgent:
		c.deletions = append(c.deletions, "agent/"+obj.GetName())
	case *corev1.Namespace:
		c.deletions = append(c.deletions, "namespace/"+obj.GetName())
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestLoadLiveE2EConfig(t *testing.T) {
	t.Run("loads an explicit safe configuration", func(t *testing.T) {
		setLiveE2EEnvironment(t, "run-123")
		t.Setenv("HARNESS_API_KEY", "")

		cfg, err := loadLiveE2EConfig()
		if err != nil {
			t.Fatalf("load live E2E config: %v", err)
		}
		if cfg.runID != "run-123" {
			t.Fatalf("run ID was changed: got %q", cfg.runID)
		}
		if cfg.controllerNamespace != "operator-system" || cfg.controllerDeployment != "controller-manager" {
			t.Fatalf(
				"unexpected controller target: %s/%s",
				cfg.controllerNamespace,
				cfg.controllerDeployment,
			)
		}
		if cfg.kubeconfig != "/tmp/e2e-kubeconfig" || cfg.kubeconfigContext != "kind-e2e" {
			t.Fatalf("unexpected kubeconfig target: %s (%s)", cfg.kubeconfig, cfg.kubeconfigContext)
		}
	})

	invalidRunIDs := []string{
		"Uppercase",
		"contains_underscore",
		"-leading",
		"trailing-",
		strings.Repeat("a", 37),
	}
	for _, runID := range invalidRunIDs {
		t.Run("rejects "+runID, func(t *testing.T) {
			setLiveE2EEnvironment(t, runID)
			if _, err := loadLiveE2EConfig(); err == nil {
				t.Fatalf("expected run ID %q to be rejected", runID)
			}
		})
	}
}

func TestLoadLiveKubernetesConfigRequiresExpectedCurrentContext(t *testing.T) {
	const (
		expectedContext = "kind-e2e"
		otherContext    = "other-cluster"
		clusterName     = "test-cluster"
		userName        = "test-user"
	)
	kubeconfigPath := filepath.Join(t.TempDir(), "config")
	rawConfig := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			clusterName: {
				Server:                "https://cluster.example.invalid",
				InsecureSkipTLSVerify: true,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			userName: {Token: "test-token"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			expectedContext: {Cluster: clusterName, AuthInfo: userName},
			otherContext:    {Cluster: clusterName, AuthInfo: userName},
		},
		CurrentContext: otherContext,
	}
	if err := clientcmd.WriteToFile(rawConfig, kubeconfigPath); err != nil {
		t.Fatalf("write test kubeconfig: %v", err)
	}

	cfg := liveE2EConfig{
		kubeconfig:        kubeconfigPath,
		kubeconfigContext: expectedContext,
	}
	if _, err := loadLiveKubernetesConfig(cfg); err == nil || !strings.Contains(err.Error(), "current context") {
		t.Fatalf("expected current-context mismatch, got %v", err)
	}

	rawConfig.CurrentContext = expectedContext
	if err := clientcmd.WriteToFile(rawConfig, kubeconfigPath); err != nil {
		t.Fatalf("rewrite test kubeconfig: %v", err)
	}
	restConfig, err := loadLiveKubernetesConfig(cfg)
	if err != nil {
		t.Fatalf("load explicit kubeconfig: %v", err)
	}
	if restConfig.Host != rawConfig.Clusters[clusterName].Server {
		t.Fatalf("unexpected Kubernetes API server: %s", restConfig.Host)
	}
}

func TestNewLiveAgentFixtureUsesSharedNamespaceAndRandomOwnerSuffix(t *testing.T) {
	runID := strings.Repeat("a", 36)
	first := newLiveAgentFixture("PROJECT", runID, "test-project", strings.Repeat("1", 32))
	second := newLiveAgentFixture("PROJECT", runID, "test-project", strings.Repeat("2", 32))

	if first.namespace != liveTestNamespace || second.namespace != liveTestNamespace {
		t.Fatalf("live fixtures do not use the shared namespace: %s / %s", first.namespace, second.namespace)
	}
	if first.agentIdentifier == second.agentIdentifier {
		t.Fatal("random owner ID did not make the Harness identities unique")
	}
	if len(first.agentIdentifier) > 63 {
		t.Fatalf("generated Harness identity exceeds the conservative limit: %s", first.agentIdentifier)
	}
	if strings.Contains(first.agentIdentifier, "-") {
		t.Fatalf("Harness identifier contains a hyphen: %s", first.agentIdentifier)
	}
}

func TestShareLiveNamespaceOwnership(t *testing.T) {
	ownerID := strings.Repeat("1", 32)
	owner := newLiveAgentFixture("ORG", "test-run", "", ownerID)
	project := newLiveAgentFixture("PROJECT", "test-run", "test-project", ownerID)
	owner.namespaceUID = types.UID("namespace-uid")
	owner.namespaceOwned = true

	if err := shareLiveNamespaceOwnership(owner, project); err != nil {
		t.Fatalf("share namespace ownership: %v", err)
	}
	if !project.namespaceOwned || project.namespaceUID != owner.namespaceUID {
		t.Fatal("shared namespace ownership was not copied to the project fixture")
	}

	t.Run("rejects a different execution owner", func(t *testing.T) {
		otherRun := newLiveAgentFixture("PROJECT", owner.runID, "test-project", strings.Repeat("2", 32))
		if err := shareLiveNamespaceOwnership(owner, otherRun); err == nil {
			t.Fatal("expected a mismatched execution owner to be rejected")
		}
	})

	t.Run("rejects an unowned namespace", func(t *testing.T) {
		unowned := newLiveAgentFixture("ORG", owner.runID, "", owner.ownerID)
		matchingProject := newLiveAgentFixture("PROJECT", owner.runID, "test-project", owner.ownerID)
		if err := shareLiveNamespaceOwnership(unowned, matchingProject); err == nil {
			t.Fatal("expected an unowned namespace to be rejected")
		}
	})
}

func TestSharedNamespaceAgentNamesDoNotCollide(t *testing.T) {
	ownerID := strings.Repeat("1", 32)
	org := newLiveAgentFixture("ORG", "test-run", "", ownerID)
	project := newLiveAgentFixture("PROJECT", "test-run", "test-project", ownerID)

	if org.namespace != project.namespace || org.namespace != liveTestNamespace {
		t.Fatalf("agents do not share %s: %s / %s", liveTestNamespace, org.namespace, project.namespace)
	}
	if org.resourceName == project.resourceName {
		t.Fatalf("agent CR names collide in the shared namespace: %s", org.resourceName)
	}
	if org.tokenSecretName == project.tokenSecretName {
		t.Fatalf("token Secret names collide in the shared namespace: %s", org.tokenSecretName)
	}
	if org.agentIdentifier == project.agentIdentifier {
		t.Fatalf("Harness agent identifiers collide: %s", org.agentIdentifier)
	}
}

func TestCreateLiveNamespaceCopiesExistingLocalCredential(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core API to scheme: %v", err)
	}

	ownerID := strings.Repeat("1", 32)
	fixture := newLiveAgentFixture("ORG", "test-run", "", ownerID)
	namespaceObject := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: liveTestNamespace,
			UID:  types.UID("namespace-uid"),
			Labels: map[string]string{
				liveRunIDLabel:   fixture.runID,
				liveOwnerIDLabel: fixture.ownerID,
			},
		},
	}
	sourceSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      liveAPIKeySecretName,
			Namespace: "operator-system",
			UID:       types.UID("source-secret-uid"),
		},
		Data: map[string][]byte{
			liveAPIKeySecretKey: []byte("source-value"),
			"unrelated":         []byte("must-not-be-copied"),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespaceObject, sourceSecret).Build()

	if err := createLiveNamespaceAndCopyAPISecret(
		context.Background(),
		k8sClient,
		fixture,
		"operator-system",
	); err != nil {
		t.Fatalf("copy the local API credential: %v", err)
	}
	if !fixture.namespaceOwned || fixture.namespaceUID != namespaceObject.UID {
		t.Fatal("the live fixture did not capture ownership of the shared namespace")
	}

	targetSecret := &corev1.Secret{}
	if err := k8sClient.Get(
		context.Background(),
		client.ObjectKey{Namespace: liveTestNamespace, Name: liveAPIKeySecretName},
		targetSecret,
	); err != nil {
		t.Fatalf("get copied API Secret: %v", err)
	}
	if string(targetSecret.Data[liveAPIKeySecretKey]) != "source-value" {
		t.Fatal("the required API key was not copied from the local source Secret")
	}
	if _, found := targetSecret.Data["unrelated"]; found {
		t.Fatal("an unrelated source Secret key was copied")
	}

	if err := deleteOwnedLiveNamespaceAndWait(context.Background(), k8sClient, fixture); err != nil {
		t.Fatalf("delete the disposable namespace: %v", err)
	}
	currentSource := &corev1.Secret{}
	if err := k8sClient.Get(
		context.Background(),
		client.ObjectKey{Namespace: "operator-system", Name: liveAPIKeySecretName},
		currentSource,
	); err != nil {
		t.Fatalf("source API Secret was removed during disposable cleanup: %v", err)
	}
	if currentSource.UID != sourceSecret.UID ||
		string(currentSource.Data[liveAPIKeySecretKey]) != "source-value" ||
		string(currentSource.Data["unrelated"]) != "must-not-be-copied" {
		t.Fatal("source API Secret was modified")
	}
}

func TestCreateLiveNamespaceRequiresExistingLocalCredential(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core API to scheme: %v", err)
	}

	testCases := []struct {
		name        string
		objects     []client.Object
		wantMessage string
	}{
		{
			name:        "missing Secret",
			wantMessage: "get source API key Secret",
		},
		{
			name: "empty required key",
			objects: []client.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      liveAPIKeySecretName,
					Namespace: "operator-system",
				},
				Data: map[string][]byte{liveAPIKeySecretKey: {}},
			}},
			wantMessage: "must contain non-empty key",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(testCase.objects...).Build()
			fixture := newLiveAgentFixture("ORG", "test-run", "", strings.Repeat("1", 32))
			err := createLiveNamespaceAndCopyAPISecret(
				context.Background(),
				k8sClient,
				fixture,
				"operator-system",
			)
			if err == nil || !strings.Contains(err.Error(), testCase.wantMessage) {
				t.Fatalf("expected invalid local credentials to fail setup, got %v", err)
			}

			currentNamespace := &corev1.Namespace{}
			err = k8sClient.Get(context.Background(), client.ObjectKey{Name: liveTestNamespace}, currentNamespace)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("namespace was created before validating the local credential: %v", err)
			}
		})
	}
}

func TestCreateLiveNamespaceRejectsDisposableCredentialSource(t *testing.T) {
	fixture := newLiveAgentFixture("ORG", "test-run", "", strings.Repeat("1", 32))
	err := createLiveNamespaceAndCopyAPISecret(
		context.Background(),
		nil,
		fixture,
		liveTestNamespace,
	)
	if err == nil || !strings.Contains(err.Error(), "must differ from the disposable E2E namespace") {
		t.Fatalf("expected the disposable namespace to be rejected as a credential source, got %v", err)
	}
}

func TestCreateLiveNamespaceRefusesStaleOwnedCredentialCopy(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core API to scheme: %v", err)
	}

	ownerID := strings.Repeat("1", 32)
	fixture := newLiveAgentFixture("ORG", "test-run", "", ownerID)
	namespaceObject := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: liveTestNamespace,
			UID:  types.UID("namespace-uid"),
			Labels: map[string]string{
				liveRunIDLabel:   fixture.runID,
				liveOwnerIDLabel: fixture.ownerID,
			},
		},
	}
	sourceSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      liveAPIKeySecretName,
			Namespace: "operator-system",
		},
		Data: map[string][]byte{liveAPIKeySecretKey: []byte("current-value")},
	}
	targetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      liveAPIKeySecretName,
			Namespace: liveTestNamespace,
			Labels: map[string]string{
				liveRunIDLabel:   fixture.runID,
				liveOwnerIDLabel: fixture.ownerID,
			},
		},
		Data: map[string][]byte{liveAPIKeySecretKey: []byte("stale-value")},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		namespaceObject,
		sourceSecret,
		targetSecret,
	).Build()

	err := createLiveNamespaceAndCopyAPISecret(
		context.Background(),
		k8sClient,
		fixture,
		"operator-system",
	)
	if err == nil || !strings.Contains(err.Error(), "create API key Secret") {
		t.Fatalf("expected a stale credential copy to be rejected, got %v", err)
	}

	currentTarget := &corev1.Secret{}
	if err := k8sClient.Get(
		context.Background(),
		client.ObjectKey{Namespace: liveTestNamespace, Name: liveAPIKeySecretName},
		currentTarget,
	); err != nil {
		t.Fatalf("get the stale target Secret: %v", err)
	}
	if string(currentTarget.Data[liveAPIKeySecretKey]) != "stale-value" {
		t.Fatal("the stale target Secret was overwritten")
	}
}

func TestCreateLiveNamespaceRefusesDifferentOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core API to scheme: %v", err)
	}

	namespaceObject := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: liveTestNamespace,
			Labels: map[string]string{
				liveRunIDLabel:   "other-run",
				liveOwnerIDLabel: "other-owner",
			},
		},
	}
	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      liveAPIKeySecretName,
			Namespace: liveTestNamespace,
		},
		Data: map[string][]byte{liveAPIKeySecretKey: []byte("existing-value")},
	}
	sourceSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      liveAPIKeySecretName,
			Namespace: "operator-system",
		},
		Data: map[string][]byte{liveAPIKeySecretKey: []byte("source-value")},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		namespaceObject,
		existingSecret,
		sourceSecret,
	).Build()
	fixture := newLiveAgentFixture("ORG", "test-run", "", strings.Repeat("1", 32))

	err := createLiveNamespaceAndCopyAPISecret(context.Background(), k8sClient, fixture, "operator-system")
	if err == nil || !strings.Contains(err.Error(), "without taking ownership") {
		t.Fatalf("expected the existing namespace to be rejected, got %v", err)
	}

	currentSecret := &corev1.Secret{}
	if err := k8sClient.Get(
		context.Background(),
		client.ObjectKey{Namespace: liveTestNamespace, Name: liveAPIKeySecretName},
		currentSecret,
	); err != nil {
		t.Fatalf("get the existing API Secret: %v", err)
	}
	if string(currentSecret.Data[liveAPIKeySecretKey]) != "existing-value" {
		t.Fatal("the existing API Secret was modified")
	}
}

func TestCleanupLiveFixturesDeletesAgentsBeforeNamespace(t *testing.T) {
	k8sClient, recorder, projectFixture, orgFixture := newCleanupTestClient(t)

	cleanupFailures := cleanupLiveFixtures(recorder, projectFixture, orgFixture)
	if len(cleanupFailures) != 0 {
		t.Fatalf("cleanup failed: %s", strings.Join(cleanupFailures, "; "))
	}

	want := "agent/project-agent,agent/org-agent,namespace/" + liveTestNamespace
	if got := strings.Join(recorder.deletions, ","); got != want {
		t.Fatalf("unexpected cleanup order: got %q, want %q", got, want)
	}

	currentNamespace := &corev1.Namespace{}
	err := k8sClient.Get(context.Background(), client.ObjectKey{Name: liveTestNamespace}, currentNamespace)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("shared namespace was not removed: %v", err)
	}
}

func TestCleanupLiveFixturesRetainsNamespaceAfterAgentFailure(t *testing.T) {
	k8sClient, recorder, projectFixture, orgFixture := newCleanupTestClient(t)

	projectAgent := &infrastructurev1.HarnessGitopsAgent{}
	projectKey := client.ObjectKey{Namespace: liveTestNamespace, Name: projectFixture.resourceName}
	if err := k8sClient.Get(context.Background(), projectKey, projectAgent); err != nil {
		t.Fatalf("get project agent: %v", err)
	}
	projectAgent.Labels[liveOwnerIDLabel] = "replacement-owner"
	if err := k8sClient.Update(context.Background(), projectAgent); err != nil {
		t.Fatalf("replace project agent ownership: %v", err)
	}

	apiSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      liveAPIKeySecretName,
			Namespace: liveTestNamespace,
		},
		Data: map[string][]byte{liveAPIKeySecretKey: []byte("test-value")},
	}
	if err := k8sClient.Create(context.Background(), apiSecret); err != nil {
		t.Fatalf("create API Secret: %v", err)
	}

	cleanupFailures := cleanupLiveFixtures(recorder, projectFixture, orgFixture)
	if len(cleanupFailures) != 1 {
		t.Fatalf("expected one cleanup failure, got %v", cleanupFailures)
	}
	for _, deletion := range recorder.deletions {
		if strings.HasPrefix(deletion, "namespace/") {
			t.Fatalf("shared namespace was deleted after an agent cleanup failure: %v", recorder.deletions)
		}
	}

	for _, objectKey := range []client.ObjectKey{
		{Name: liveTestNamespace},
		{Namespace: liveTestNamespace, Name: liveAPIKeySecretName},
	} {
		var object client.Object = &corev1.Namespace{}
		if objectKey.Namespace != "" {
			object = &corev1.Secret{}
		}
		if err := k8sClient.Get(context.Background(), objectKey, object); err != nil {
			t.Fatalf("retained object %s is missing: %v", objectKey.String(), err)
		}
	}
}

func newCleanupTestClient(
	t *testing.T,
) (client.Client, *recordingDeleteClient, *liveAgentFixture, *liveAgentFixture) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core API to scheme: %v", err)
	}
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add HarnessGitopsAgent API to scheme: %v", err)
	}

	const runID = "test-run"
	ownerID := strings.Repeat("1", 32)
	namespaceUID := types.UID("namespace-uid")
	namespaceObject := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: liveTestNamespace,
			UID:  namespaceUID,
			Labels: map[string]string{
				liveRunIDLabel:   runID,
				liveOwnerIDLabel: ownerID,
			},
		},
	}

	orgFixture := newLiveAgentFixture("ORG", runID, "", ownerID)
	projectFixture := newLiveAgentFixture("PROJECT", runID, "test-project", ownerID)
	for _, fixture := range []*liveAgentFixture{orgFixture, projectFixture} {
		fixture.namespaceUID = namespaceUID
		fixture.namespaceOwned = true
		fixture.agentUID = types.UID(fixture.scope + "-agent-uid")
		fixture.agentOwned = true
	}

	orgAgent := buildLiveAgent(liveE2EConfig{}, orgFixture)
	orgAgent.UID = orgFixture.agentUID
	projectAgent := buildLiveAgent(liveE2EConfig{}, projectFixture)
	projectAgent.UID = projectFixture.agentUID

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		namespaceObject,
		orgAgent,
		projectAgent,
	).Build()
	return k8sClient, &recordingDeleteClient{Client: k8sClient}, projectFixture, orgFixture
}

func TestAssertLiveControllerReady(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps API to scheme: %v", err)
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coordination API to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core API to scheme: %v", err)
	}

	replicas := int32(1)
	controllerLabels := map[string]string{"app": "controller-manager"}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "controller-manager",
			Namespace:  "operator-system",
			Generation: 2,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: controllerLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: controllerLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  liveManagerContainer,
							Image: "example.com/controller:test-sha",
							Args:  []string{"--leader-elect"},
						},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			ReadyReplicas:      1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			Conditions: []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentAvailable,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "controller-manager-abc123",
			Namespace: deployment.Namespace,
			Labels:    controllerLabels,
		},
		Spec: deployment.Spec.Template.Spec,
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	holderIdentity := pod.Name + "_leader-id"
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "controller-leader",
			Namespace: deployment.Namespace,
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holderIdentity},
	}
	crd := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata": map[string]interface{}{
			"name": liveAgentCRDName,
		},
		"spec": map[string]interface{}{
			"versions": []interface{}{
				map[string]interface{}{
					"name":    liveCRDVersion,
					"served":  true,
					"storage": true,
				},
			},
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Established",
					"status": "True",
				},
			},
		},
	}}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	})

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, pod, lease, crd).Build()
	assertLiveControllerReady(context.Background(), gomega.NewWithT(t), k8sClient, liveE2EConfig{
		controllerNamespace:  deployment.Namespace,
		controllerDeployment: deployment.Name,
		controllerImage:      "example.com/controller:test-sha",
		controllerLeaseName:  lease.Name,
	})
}

func TestDeleteOwnedLiveNamespaceRefusesChangedOwnership(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core API to scheme: %v", err)
	}

	const (
		namespaceName = "hga-e2e-project-run-123"
		runID         = "run-123"
	)
	namespaceObject := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespaceName,
			UID:  types.UID("replacement-uid"),
			Labels: map[string]string{
				liveRunIDLabel:   "replacement-run",
				liveOwnerIDLabel: "replacement-owner",
			},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespaceObject).Build()
	fixture := &liveAgentFixture{
		runID:          runID,
		namespace:      namespaceName,
		ownerID:        "original-owner",
		namespaceUID:   types.UID("original-uid"),
		namespaceOwned: true,
	}

	err := deleteOwnedLiveNamespaceAndWait(context.Background(), k8sClient, fixture)
	if err == nil || !strings.Contains(err.Error(), "refusing cleanup") {
		t.Fatalf("expected changed ownership to block cleanup, got %v", err)
	}

	current := &corev1.Namespace{}
	if err := k8sClient.Get(
		context.Background(),
		client.ObjectKey{Name: namespaceName},
		current,
	); err != nil {
		t.Fatalf("replacement namespace should not be deleted: %v", err)
	}
}

func TestDeleteLiveAgentRefusesChangedOwnership(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core API to scheme: %v", err)
	}
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add HarnessGitopsAgent API to scheme: %v", err)
	}

	const (
		namespaceName = "hga-e2e-project-run-123-owner"
		runID         = "run-123"
	)
	namespaceObject := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespaceName,
			UID:  types.UID("namespace-uid"),
			Labels: map[string]string{
				liveRunIDLabel:   runID,
				liveOwnerIDLabel: "original-owner",
			},
		},
	}
	replacementAgent := &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "project-agent",
			Namespace: namespaceName,
			UID:       types.UID("replacement-agent-uid"),
			Labels: map[string]string{
				liveRunIDLabel:   "replacement-run",
				liveOwnerIDLabel: "replacement-owner",
			},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		namespaceObject,
		replacementAgent,
	).Build()
	fixture := &liveAgentFixture{
		scope:          "PROJECT",
		runID:          runID,
		namespace:      namespaceName,
		resourceName:   replacementAgent.Name,
		ownerID:        "original-owner",
		namespaceUID:   namespaceObject.UID,
		namespaceOwned: true,
		agentUID:       types.UID("original-agent-uid"),
		agentOwned:     true,
	}

	err := deleteLiveAgentAndWait(context.Background(), k8sClient, fixture)
	if err == nil || !strings.Contains(err.Error(), "refusing deletion") {
		t.Fatalf("expected changed agent ownership to block deletion, got %v", err)
	}

	current := &infrastructurev1.HarnessGitopsAgent{}
	if err := k8sClient.Get(
		context.Background(),
		client.ObjectKey{Namespace: namespaceName, Name: replacementAgent.Name},
		current,
	); err != nil {
		t.Fatalf("replacement agent should not be deleted: %v", err)
	}
}

func setLiveE2EEnvironment(t *testing.T, runID string) {
	t.Helper()
	values := map[string]string{
		"HARNESS_ACCOUNT_ID":        "test-account",
		"HARNESS_ORG_ID":            "test-org",
		"HARNESS_PROJECT_ID":        "test-project",
		"HARNESS_E2E_RUN_ID":        runID,
		"KUBECONFIG":                "/tmp/e2e-kubeconfig",
		"E2E_KUBECONFIG_CONTEXT":    "kind-e2e",
		"E2E_CONTROLLER_NAMESPACE":  "operator-system",
		"E2E_CONTROLLER_DEPLOYMENT": "controller-manager",
		"E2E_CONTROLLER_IMAGE":      "example.com/controller:test-sha",
		"E2E_CONTROLLER_LEASE_NAME": "controller-leader",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
}
