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
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/markoskandylis/harness-gitops-agent-operator/test/utils"
)

const enabledEnvValue = "true"

var (
	// useExistingController skips the scaffolded image build and controller deployment.
	// It lets the live registration specs exercise an exact image that CI installed already.
	useExistingController = os.Getenv("E2E_USE_EXISTING_CONTROLLER") == enabledEnvValue

	// Optional Environment Variables:
	// - CERT_MANAGER_INSTALL_SKIP=true: Skips CertManager installation during test setup.
	// These variables are useful if CertManager is already installed, avoiding
	// re-installation and conflicts.
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == enabledEnvValue
	// isCertManagerAlreadyInstalled will be set true when CertManager CRDs be found on the cluster
	isCertManagerAlreadyInstalled = false
	// certManagerInstalledBySuite prevents teardown from touching an ambient installation
	// when setup was skipped, detected an existing installation, or failed early.
	certManagerInstalledBySuite = false

	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "example.com/harness-gitops-agent-operator:v0.0.1"
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purpose of being used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting harness-gitops-agent-operator integration test suite\n")
	suiteConfig, _ := GinkgoConfiguration()
	suiteConfig.FailOnEmpty = true
	RunSpecs(t, "e2e suite", suiteConfig)
}

var _ = BeforeSuite(func() {
	liveRequested := os.Getenv("RUN_HARNESS_LIVE_E2E") == enabledEnvValue
	if liveRequested || liveSpecsExplicitlySelected() {
		Expect(liveRequested).To(BeTrue(), "RUN_HARNESS_LIVE_E2E=true is required for live-selected E2E specs")
		Expect(useExistingController).To(
			BeTrue(),
			"E2E_USE_EXISTING_CONTROLLER=true is required for live-selected E2E specs",
		)
	}

	if useExistingController {
		return
	}

	By("building the manager(Operator) image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	// TODO(user): If you want to change the e2e test vendor from Kind, ensure the image is
	// built and available before running the tests. Also, remove the following block.
	By("loading the manager(Operator) image on Kind")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")

	// The tests-e2e are intended to run on a temporary cluster that is created and destroyed for testing.
	// To prevent errors when tests run in environments with CertManager already installed,
	// we check for its presence before execution.
	// Setup CertManager before the suite if not skipped and if not already installed
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
			certManagerInstalledBySuite = true
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}
})

var _ = AfterSuite(func() {
	if useExistingController {
		return
	}

	// Teardown only resources whose successful installation this suite recorded.
	if certManagerInstalledBySuite {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		utils.UninstallCertManager()
	}
})
