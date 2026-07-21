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
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	ginkgotypes "github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	"github.com/markoskandylis/harness-gitops-agent-operator/test/utils"
)

const (
	mappingProbeEnabledEnv     = "RUN_HARNESS_MAPPING_PROBE_E2E"
	mappingProbeFixturePath    = "test/e2e/gitops/plugin-probe"
	mappingProbeExpression     = `<+secrets.getValue("harness-plugin-probe")>`
	mappingProbeSecretName     = "harness-plugin-probe"
	mappingProbeSecretKey      = "value"
	mappingProbeDeadline       = 5 * time.Minute
	mappingProbeRequestTimeout = 30 * time.Second
)

var exactGitRevisionPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

type mappingProbeConfig struct {
	kubeconfig        string
	kubeconfigContext string
	agentNamespace    string
	agentName         string
	tokenSecret       string
	apiKeySecret      string
	projectID         string
	appProject        string
	application       string
	targetNamespace   string
	repoURL           string
	revision          string
}

var _ = Describe("Helm-installed mapping and remote Git plugin", Serial, Label("remote", "mapping-probe"), func() {
	It("maps only the healthy agent and syncs the exact pushed commit", func() {
		if os.Getenv(mappingProbeEnabledEnv) != enabledEnvValue {
			if mappingProbeExplicitlySelected() {
				Fail(mappingProbeEnabledEnv + "=true is required for the selected mapping probe")
			}
			Skip("set " + mappingProbeEnabledEnv + "=true to run the remote mapping probe")
		}

		cfg, err := loadMappingProbeConfig()
		Expect(err).NotTo(HaveOccurred())
		assertMappingProbeFixture()

		restConfig, err := loadMappingProbeKubernetesConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		restConfig.Timeout = mappingProbeRequestTimeout
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(infrastructurev1.AddToScheme(scheme)).To(Succeed())
		k8sClient, err := client.New(restConfig, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the Helm-installed agent and controller-created token")
		Eventually(func(g Gomega) {
			ctx, cancel := context.WithTimeout(context.Background(), mappingProbeRequestTimeout)
			defer cancel()
			agent := &infrastructurev1.HarnessGitopsAgent{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Namespace: cfg.agentNamespace,
				Name:      cfg.agentName,
			}, agent)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.EqualFold(agent.Spec.Scope, "ORG")).To(BeTrue())
			g.Expect(strings.TrimSpace(agent.Spec.ProjectId)).To(BeEmpty())
			g.Expect(agent.Spec.ProjectMapping).NotTo(BeNil())
			if agent.Spec.ProjectMapping != nil {
				g.Expect(agent.Spec.ProjectMapping.ProjectId).To(Equal(cfg.projectID))
				g.Expect(agent.Spec.ProjectMapping.AppProject).To(Equal(cfg.appProject))
			}
			g.Expect(agent.Spec.ApiKeySecretRef).To(Equal(cfg.apiKeySecret))
			g.Expect(agent.Spec.TokenSecretRef).To(Equal(cfg.tokenSecret))
			g.Expect(agent.Finalizers).To(ContainElement("infrastructure.kandylis.co.uk/finalizer"))
			g.Expect(agent.Status.ArgoProjectId).To(Equal(cfg.appProject))
			g.Expect(strings.TrimSpace(agent.Status.ArgoProjectMappingId)).NotTo(BeEmpty())
			condition := apiMeta.FindStatusCondition(agent.Status.Conditions, "MappingReady")
			g.Expect(condition).NotTo(BeNil())
			if condition != nil {
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(condition.ObservedGeneration).To(Equal(agent.Generation))
			}

			token := &corev1.Secret{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Namespace: cfg.agentNamespace,
				Name:      cfg.tokenSecret,
			}, token)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(token.Data["GITOPS_AGENT_TOKEN"]).NotTo(BeEmpty())
			g.Expect(metav1.IsControlledBy(token, agent)).To(BeTrue())

			apiKey := &corev1.Secret{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Namespace: cfg.agentNamespace,
				Name:      cfg.apiKeySecret,
			}, apiKey)
			g.Expect(apierrors.IsNotFound(err)).To(
				BeTrue(),
				"the controller API key must remain outside the agent namespace",
			)
		}, mappingProbeDeadline, 2*time.Second).Should(Succeed())

		appProject := &unstructured.Unstructured{}
		appProject.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "argoproj.io", Version: "v1alpha1", Kind: "AppProject",
		})
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{
			Namespace: cfg.agentNamespace,
			Name:      cfg.appProject,
		}, appProject)).To(Succeed())

		By("verifying Argo synced the remote fixture from the exact commit")
		Eventually(func(g Gomega) {
			ctx, cancel := context.WithTimeout(context.Background(), mappingProbeRequestTimeout)
			defer cancel()
			application := &unstructured.Unstructured{}
			application.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "argoproj.io", Version: "v1alpha1", Kind: "Application",
			})
			err := k8sClient.Get(ctx, types.NamespacedName{
				Namespace: cfg.agentNamespace,
				Name:      cfg.application,
			}, application)
			g.Expect(err).NotTo(HaveOccurred())
			expectNestedString(g, application.Object, cfg.repoURL, "spec", "source", "repoURL")
			expectNestedRevision(g, application.Object, cfg.revision, "spec", "source", "targetRevision")
			expectNestedString(g, application.Object, mappingProbeFixturePath, "spec", "source", "path")
			expectNestedString(g, application.Object, "Synced", "status", "sync", "status")
			expectNestedString(g, application.Object, "Healthy", "status", "health", "status")
			expectNestedRevision(g, application.Object, cfg.revision, "status", "sync", "revision")
			conditions, found, err := unstructured.NestedSlice(application.Object, "status", "conditions")
			g.Expect(err).NotTo(HaveOccurred())
			if found {
				g.Expect(conditions).To(BeEmpty())
			}
		}, mappingProbeDeadline, 2*time.Second).Should(Succeed())

		By("verifying the plugin resolved an ordinary non-cluster Secret")
		Eventually(func(g Gomega) {
			ctx, cancel := context.WithTimeout(context.Background(), mappingProbeRequestTimeout)
			defer cancel()
			secret := &corev1.Secret{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Namespace: cfg.targetNamespace,
				Name:      mappingProbeSecretName,
			}, secret)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
			g.Expect(secret.Labels["argocd.argoproj.io/secret-type"]).NotTo(Equal("cluster"))
			g.Expect(secret.Data).To(HaveLen(1))
			value, found := secret.Data[mappingProbeSecretKey]
			g.Expect(found).To(BeTrue())
			g.Expect(value).NotTo(BeEmpty())
			g.Expect(string(value)).NotTo(Equal(mappingProbeExpression))
		}, mappingProbeDeadline, 2*time.Second).Should(Succeed())
	})
})

func expectNestedString(g Gomega, object map[string]interface{}, want string, fields ...string) {
	value, found, err := unstructured.NestedString(object, fields...)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(value).To(Equal(want), "field %s had an unexpected value", strings.Join(fields, "."))
}

func expectNestedRevision(g Gomega, object map[string]interface{}, want string, fields ...string) {
	value, found, err := unstructured.NestedString(object, fields...)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(strings.EqualFold(value, want)).To(
		BeTrue(),
		"field %s was %q, want commit %q",
		strings.Join(fields, "."),
		value,
		want,
	)
}

func loadMappingProbeConfig() (mappingProbeConfig, error) {
	values := map[string]string{
		"KUBECONFIG":                             strings.TrimSpace(os.Getenv("KUBECONFIG")),
		"E2E_KUBECONFIG_CONTEXT":                 strings.TrimSpace(os.Getenv("E2E_KUBECONFIG_CONTEXT")),
		"HARNESS_MAPPING_AGENT_NAMESPACE":        strings.TrimSpace(os.Getenv("HARNESS_MAPPING_AGENT_NAMESPACE")),
		"HARNESS_MAPPING_AGENT_NAME":             strings.TrimSpace(os.Getenv("HARNESS_MAPPING_AGENT_NAME")),
		"HARNESS_MAPPING_AGENT_TOKEN_SECRET":     strings.TrimSpace(os.Getenv("HARNESS_MAPPING_AGENT_TOKEN_SECRET")),
		"HARNESS_MAPPING_API_KEY_SECRET":         strings.TrimSpace(os.Getenv("HARNESS_MAPPING_API_KEY_SECRET")),
		"HARNESS_MAPPING_PROJECT_ID":             strings.TrimSpace(os.Getenv("HARNESS_MAPPING_PROJECT_ID")),
		"HARNESS_MAPPING_APP_PROJECT":            strings.TrimSpace(os.Getenv("HARNESS_MAPPING_APP_PROJECT")),
		"HARNESS_MAPPING_PROBE_APPLICATION":      strings.TrimSpace(os.Getenv("HARNESS_MAPPING_PROBE_APPLICATION")),
		"HARNESS_MAPPING_PROBE_TARGET_NAMESPACE": strings.TrimSpace(os.Getenv("HARNESS_MAPPING_PROBE_TARGET_NAMESPACE")),
		"HARNESS_MAPPING_PROBE_REPO_URL":         strings.TrimSpace(os.Getenv("HARNESS_MAPPING_PROBE_REPO_URL")),
		"HARNESS_MAPPING_PROBE_REVISION":         strings.TrimSpace(os.Getenv("HARNESS_MAPPING_PROBE_REVISION")),
	}
	var missing []string
	for name, value := range values {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return mappingProbeConfig{}, fmt.Errorf("missing required mapping probe environment variables: %s", strings.Join(missing, ", "))
	}
	repositoryURL, err := url.Parse(values["HARNESS_MAPPING_PROBE_REPO_URL"])
	if err != nil || repositoryURL.Scheme != "https" || repositoryURL.Host == "" || repositoryURL.User != nil {
		return mappingProbeConfig{}, fmt.Errorf("HARNESS_MAPPING_PROBE_REPO_URL must be an HTTPS remote URL without embedded credentials")
	}
	if !exactGitRevisionPattern.MatchString(values["HARNESS_MAPPING_PROBE_REVISION"]) {
		return mappingProbeConfig{}, fmt.Errorf("HARNESS_MAPPING_PROBE_REVISION must be an exact 40- or 64-character commit")
	}
	return mappingProbeConfig{
		kubeconfig:        values["KUBECONFIG"],
		kubeconfigContext: values["E2E_KUBECONFIG_CONTEXT"],
		agentNamespace:    values["HARNESS_MAPPING_AGENT_NAMESPACE"],
		agentName:         values["HARNESS_MAPPING_AGENT_NAME"],
		tokenSecret:       values["HARNESS_MAPPING_AGENT_TOKEN_SECRET"],
		apiKeySecret:      values["HARNESS_MAPPING_API_KEY_SECRET"],
		projectID:         values["HARNESS_MAPPING_PROJECT_ID"],
		appProject:        values["HARNESS_MAPPING_APP_PROJECT"],
		application:       values["HARNESS_MAPPING_PROBE_APPLICATION"],
		targetNamespace:   values["HARNESS_MAPPING_PROBE_TARGET_NAMESPACE"],
		repoURL:           values["HARNESS_MAPPING_PROBE_REPO_URL"],
		revision:          strings.ToLower(values["HARNESS_MAPPING_PROBE_REVISION"]),
	}, nil
}

func loadMappingProbeKubernetesConfig(cfg mappingProbeConfig) (*rest.Config, error) {
	paths := filepath.SplitList(cfg.kubeconfig)
	if len(paths) == 0 {
		return nil, fmt.Errorf("KUBECONFIG does not contain a path")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.Precedence = paths
	rawConfig, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("load KUBECONFIG: %w", err)
	}
	if rawConfig.CurrentContext != cfg.kubeconfigContext {
		return nil, fmt.Errorf("KUBECONFIG current context is %q, expected %q", rawConfig.CurrentContext, cfg.kubeconfigContext)
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: cfg.kubeconfigContext}
	return clientcmd.NewNonInteractiveClientConfig(*rawConfig, cfg.kubeconfigContext, overrides, rules).ClientConfig()
}

func assertMappingProbeFixture() {
	projectDir, err := utils.GetProjectDir()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	fixture, err := os.ReadFile(filepath.Join(projectDir, mappingProbeFixturePath, "secret.yaml"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	content := string(fixture)
	ExpectWithOffset(1, content).To(ContainSubstring(mappingProbeExpression))
	ExpectWithOffset(1, content).NotTo(ContainSubstring("argocd.argoproj.io/secret-type"))
}

func mappingProbeExplicitlySelected() bool {
	suiteConfig, _ := GinkgoConfiguration()
	if strings.TrimSpace(suiteConfig.LabelFilter) == "" {
		return false
	}
	filter, err := ginkgotypes.ParseLabelFilter(suiteConfig.LabelFilter)
	return err == nil && filter([]string{"remote", "mapping-probe"})
}
