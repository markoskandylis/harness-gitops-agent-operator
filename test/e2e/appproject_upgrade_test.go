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
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/markoskandylis/harness-gitops-agent-operator/test/utils"
)

const bootstrapChartPath = "charts/harness-gitops-agent-bootstrap"

var _ = Describe("Bootstrap AppProject lifecycle", Serial, Label("chart"), func() {
	It("retains the AppProject UID across a Helm upgrade", func() {
		_, err := exec.LookPath("helm")
		Expect(err).NotTo(HaveOccurred(), "helm is required for the chart lifecycle test")

		suffix := strings.TrimPrefix(strconv.FormatInt(GinkgoRandomSeed(), 10), "-")
		namespace := "hga-appproject-" + suffix
		release := "appproject-" + suffix

		By("building the locked bootstrap chart dependency")
		_, err = utils.Run(exec.Command("helm", "dependency", "build", bootstrapChartPath))
		Expect(err).NotTo(HaveOccurred())

		By("creating an isolated namespace")
		_, err = utils.Run(exec.Command("kubectl", "create", "namespace", namespace))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("helm", "uninstall", release, "--namespace", namespace))
			_, _ = utils.Run(exec.Command(
				"kubectl", "delete", "namespace", namespace,
				"--ignore-not-found", "--wait=true", "--timeout=2m",
			))
		})

		By("installing the bootstrap chart with its AppProject as a normal resource")
		_, err = utils.Run(exec.Command(
			"helm",
			bootstrapChartArgs(release, namespace, "before-upgrade")...,
		))
		Expect(err).NotTo(HaveOccurred())
		uidBefore := appProjectUID(namespace)

		By("upgrading the AppProject in place")
		_, err = utils.Run(exec.Command(
			"helm",
			bootstrapChartArgs(release, namespace, "after-upgrade")...,
		))
		Expect(err).NotTo(HaveOccurred())
		uidAfter := appProjectUID(namespace)

		Expect(uidAfter).To(Equal(uidBefore), "Helm upgrade replaced AppProject/default")
	})
})

func bootstrapChartArgs(release string, namespace string, description string) []string {
	argoName := release + "-argocd"
	return []string{
		"upgrade", "--install", release, bootstrapChartPath,
		"--namespace", namespace,
		"--set", "harnessAgent.enabled=false",
		"--set", "appProject.enabled=true",
		"--set-string", "appProject.description=" + description,
		"--set", "gitopsAgent.enabled=true",
		"--set-string", "gitopsAgent.harness.identity.accountIdentifier=test-account",
		"--set-string", "gitopsAgent.harness.identity.orgIdentifier=test-org",
		"--set-string", "gitopsAgent.harness.identity.projectIdentifier=test-project",
		"--set-string", "gitopsAgent.harness.identity.agentIdentifier=test-agent",
		"--set-string", "gitopsAgent.agent.harnessName=" + release,
		"--set-string", "gitopsAgent.agent.existingSecrets.agentToken=unused-token-secret",
		"--set", "gitopsAgent.agent.replicas=0",
		"--set-string", "gitopsAgent.argo-cd.nameOverride=" + argoName,
		"--set-string", "gitopsAgent.argo-cd.fullnameOverride=" + argoName,
		"--set", "gitopsAgent.argo-cd.controller.replicas=0",
		"--set", "gitopsAgent.argo-cd.applicationSet.replicas=0",
		"--set", "gitopsAgent.argo-cd.repoServer.replicas=0",
	}
}

func appProjectUID(namespace string) string {
	output, err := utils.Run(exec.Command(
		"kubectl", "get", "appproject", "default",
		"--namespace", namespace,
		"--output", "jsonpath={.metadata.uid}",
	))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	uid := strings.TrimSpace(output)
	ExpectWithOffset(1, uid).NotTo(BeEmpty(), fmt.Sprintf("AppProject/default in %s has no UID", namespace))
	return uid
}
