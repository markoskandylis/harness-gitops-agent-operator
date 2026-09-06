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

package integration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	resourceutil "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource"
	agentcontroller "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource/agent"
)

var _ = Describe("HarnessGitopsAgent Controller", func() {
	It("keeps remote agent identity immutable", func() {
		resource := &infrastructurev1.HarnessGitopsAgent{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "immutable-agent",
				Namespace: "default",
			},
			Spec: infrastructurev1.HarnessGitopsAgentSpec{
				Name:            "immutable-agent",
				Identifier:      "immutable-agent",
				Operator:        "ARGO",
				AccountId:       "account",
				OrgId:           "org",
				ProjectId:       "project",
				Scope:           "PROJECT",
				Type:            "MANAGED_ARGO_PROVIDER",
				ApiKeySecretRef: "api-key",
				TokenSecretRef:  "agent-token",
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		DeferCleanup(func() {
			current := &infrastructurev1.HarnessGitopsAgent{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(resource), current); err == nil {
				Expect(k8sClient.Delete(ctx, current)).To(Succeed())
			}
		})

		resource.Spec.ApiKeySecretRef = "rotated-api-key"
		Expect(k8sClient.Update(ctx, resource)).To(Succeed())

		resource.Spec.ExistingAgentIdentifier = "shared-agent"
		err := k8sClient.Update(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Harness agent identity is immutable"))
	})

	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		harnessgitopsagent := &infrastructurev1.HarnessGitopsAgent{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind HarnessGitopsAgent")
			err := k8sClient.Get(ctx, typeNamespacedName, harnessgitopsagent)
			if err != nil && errors.IsNotFound(err) {
				resource := &infrastructurev1.HarnessGitopsAgent{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: infrastructurev1.HarnessGitopsAgentSpec{
						Name:            "test-agent",
						Identifier:      "test-agent",
						Operator:        "ARGO",
						AccountId:       "account",
						OrgId:           "org",
						ProjectId:       "project",
						Scope:           "PROJECT",
						Type:            "MANAGED_ARGO_PROVIDER",
						ApiKeySecretRef: "missing-secret",
						TokenSecretRef:  "test-agent-token",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &infrastructurev1.HarnessGitopsAgent{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance HarnessGitopsAgent")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("reports a missing local key without adding a finalizer", func() {
			By("Reconciling the created resource")
			policy, policyErr := resourceutil.NewNamespacePolicy([]string{typeNamespacedName.Namespace})
			Expect(policyErr).NotTo(HaveOccurred())
			controllerReconciler := &agentcontroller.Reconciler{
				NamespacePolicy: policy,
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			current := &infrastructurev1.HarnessGitopsAgent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, current)).To(Succeed())
			Expect(current.Finalizers).To(BeEmpty())
			Expect(current.Status.Conditions).To(ContainElement(HaveField("Reason", "CredentialsUnavailable")))
		})
	})
})
