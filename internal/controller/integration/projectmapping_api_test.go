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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

var _ = Describe("HarnessGitopsProjectMapping API", func() {
	It("accepts a valid mapping and its complete remote status", func() {
		mapping := newValidProjectMapping("valid-project-mapping")
		Expect(k8sClient.Create(ctx, mapping)).To(Succeed())
		defer deleteProjectMapping(mapping.Name)

		Expect(mapping.Spec.AutoCreateServiceEnv).To(BeFalse())

		mapping.Status = infrastructurev1.HarnessGitopsProjectMappingStatus{
			ObservedGeneration: mapping.Generation,
			Remote: &infrastructurev1.HarnessGitopsProjectMappingRemoteStatus{
				MappingID: "mapping-id",
				Ownership: infrastructurev1.OwnershipManaged,
				Agent: infrastructurev1.HarnessGitopsProjectMappingAgentStatus{
					Identifier: "agent-id",
					AccountID:  "account-id",
					Scope:      "ACCOUNT",
					OrgID:      "",
					ProjectID:  "",
				},
				Target: infrastructurev1.HarnessGitopsProjectMappingTargetStatus{
					OrgID:                "org-id",
					ProjectID:            "project-id",
					AppProject:           "payments",
					AutoCreateServiceEnv: false,
				},
			},
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: mapping.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             "Verified",
				Message:            "The mapping is verified",
			}},
		}
		Expect(k8sClient.Status().Update(ctx, mapping)).To(Succeed())

		current := &infrastructurev1.HarnessGitopsProjectMapping{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(mapping), current)).To(Succeed())
		Expect(current.Status.Remote).NotTo(BeNil())
		Expect(current.Status.Remote.MappingID).To(Equal("mapping-id"))
		Expect(current.Status.Remote.Ownership).To(Equal(infrastructurev1.OwnershipManaged))

		current.Status.Remote.Ownership = infrastructurev1.OwnershipAdopted
		Expect(k8sClient.Status().Update(ctx, current)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(mapping), current)).To(Succeed())
		Expect(current.Status.Remote.Ownership).To(Equal(infrastructurev1.OwnershipAdopted))
	})

	It("persists a resolved create intent before Harness returns an ID", func() {
		mapping := newValidProjectMapping("pending-project-mapping")
		Expect(k8sClient.Create(ctx, mapping)).To(Succeed())
		defer deleteProjectMapping(mapping.Name)

		mapping.Status = infrastructurev1.HarnessGitopsProjectMappingStatus{
			ObservedGeneration: mapping.Generation,
			CreationState:      infrastructurev1.MappingCreationPending,
			Remote: &infrastructurev1.HarnessGitopsProjectMappingRemoteStatus{
				Agent: infrastructurev1.HarnessGitopsProjectMappingAgentStatus{
					Identifier: "account.agent-id",
					AccountID:  "account-id",
					Scope:      "ACCOUNT",
				},
				Target: infrastructurev1.HarnessGitopsProjectMappingTargetStatus{
					OrgID:      "target-org",
					ProjectID:  "target-project",
					AppProject: "payments",
				},
			},
		}
		Expect(k8sClient.Status().Update(ctx, mapping)).To(Succeed())

		current := &infrastructurev1.HarnessGitopsProjectMapping{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(mapping), current)).To(Succeed())
		Expect(current.Status.CreationState).To(Equal(infrastructurev1.MappingCreationPending))
		Expect(current.Status.Remote).NotTo(BeNil())
		Expect(current.Status.Remote.MappingID).To(BeEmpty())
		Expect(current.Status.Remote.Ownership).To(BeEmpty())
		Expect(current.Status.Remote.Target.OrgID).To(Equal("target-org"))
		Expect(current.Status.Remote.Target.ProjectID).To(Equal("target-project"))
	})

	It("rejects a create intent without its resolved remote tuple", func() {
		mapping := newValidProjectMapping("incomplete-create-intent")
		Expect(k8sClient.Create(ctx, mapping)).To(Succeed())
		defer deleteProjectMapping(mapping.Name)

		mapping.Status.CreationState = infrastructurev1.MappingCreationPending
		err := k8sClient.Status().Update(ctx, mapping)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("creation state requires the resolved remote tuple"))
	})

	DescribeTable("rejects missing required fields",
		func(name string, spec map[string]any, expectedField string) {
			mapping := newUnstructuredProjectMapping(name, spec)
			err := k8sClient.Create(ctx, mapping)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(expectedField))
		},
		Entry(
			"without agentRef",
			"missing-agent-ref",
			map[string]any{"appProject": "payments"},
			"agentRef",
		),
		Entry(
			"without agentRef.name",
			"missing-agent-name",
			map[string]any{
				"agentRef":   map[string]any{},
				"appProject": "payments",
			},
			"name",
		),
		Entry(
			"without appProject",
			"missing-app-project",
			map[string]any{"agentRef": map[string]any{"name": "shared-agent"}},
			"appProject",
		),
	)

	It("rejects changes to mapping identity", func() {
		mapping := newValidProjectMapping("immutable-project-mapping")
		mapping.Spec.OrgID = "org-a"
		mapping.Spec.ProjectID = "project-a"
		Expect(k8sClient.Create(ctx, mapping)).To(Succeed())
		defer deleteProjectMapping(mapping.Name)

		mutations := []struct {
			name   string
			mutate func(*infrastructurev1.HarnessGitopsProjectMapping)
		}{
			{
				name: "agent reference",
				mutate: func(current *infrastructurev1.HarnessGitopsProjectMapping) {
					current.Spec.AgentRef.Name = "other-agent"
				},
			},
			{
				name: "AppProject",
				mutate: func(current *infrastructurev1.HarnessGitopsProjectMapping) {
					current.Spec.AppProject = "orders"
				},
			},
			{
				name: "organization",
				mutate: func(current *infrastructurev1.HarnessGitopsProjectMapping) {
					current.Spec.OrgID = "org-b"
				},
			},
			{
				name: "project",
				mutate: func(current *infrastructurev1.HarnessGitopsProjectMapping) {
					current.Spec.ProjectID = "project-b"
				},
			},
			{
				name: "automatic service and environment creation",
				mutate: func(current *infrastructurev1.HarnessGitopsProjectMapping) {
					current.Spec.AutoCreateServiceEnv = true
				},
			},
		}

		for _, mutation := range mutations {
			By("rejecting a change to " + mutation.name)
			current := &infrastructurev1.HarnessGitopsProjectMapping{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(mapping), current)).To(Succeed())
			mutation.mutate(current)
			err := k8sClient.Update(ctx, current)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Harness project mapping identity is immutable"))
		}
	})

	It("treats omitted and empty optional identifiers as equivalent", func() {
		const name = "empty-optional-identifiers"
		mapping := newUnstructuredProjectMapping(name, map[string]any{
			"agentRef":   map[string]any{"name": "shared-agent"},
			"appProject": "payments",
		})
		Expect(k8sClient.Create(ctx, mapping)).To(Succeed())
		defer deleteProjectMapping(name)

		current := newUnstructuredProjectMapping(name, nil)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(mapping), current)).To(Succeed())
		Expect(unstructured.SetNestedField(current.Object, "", "spec", "orgId")).To(Succeed())
		Expect(unstructured.SetNestedField(current.Object, "", "spec", "projectId")).To(Succeed())
		Expect(unstructured.SetNestedField(current.Object, "", "spec", "adoptMappingId")).To(Succeed())
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(mapping), current)).To(Succeed())
		unstructured.RemoveNestedField(current.Object, "spec", "orgId")
		unstructured.RemoveNestedField(current.Object, "spec", "projectId")
		unstructured.RemoveNestedField(current.Object, "spec", "adoptMappingId")
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
	})

	It("allows adoptMappingId to be corrected after an ambiguous create", func() {
		mapping := newValidProjectMapping("correctable-adoption")
		Expect(k8sClient.Create(ctx, mapping)).To(Succeed())
		defer deleteProjectMapping(mapping.Name)

		mapping.Spec.AdoptMappingID = "mapping-id"
		Expect(k8sClient.Update(ctx, mapping)).To(Succeed())

		mapping.Spec.AdoptMappingID = "different-mapping-id"
		Expect(k8sClient.Update(ctx, mapping)).To(Succeed())
	})
})

func newValidProjectMapping(name string) *infrastructurev1.HarnessGitopsProjectMapping {
	return &infrastructurev1.HarnessGitopsProjectMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: infrastructurev1.HarnessGitopsProjectMappingSpec{
			AgentRef: infrastructurev1.HarnessGitopsAgentReference{
				Name: "shared-agent",
			},
			AppProject: "payments",
		},
	}
}

func newUnstructuredProjectMapping(name string, spec map[string]any) *unstructured.Unstructured {
	mapping := &unstructured.Unstructured{
		Object: map[string]any{
			"metadata": map[string]any{
				"name":      name,
				"namespace": "default",
			},
		},
	}
	mapping.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   infrastructurev1.GroupVersion.Group,
		Version: infrastructurev1.GroupVersion.Version,
		Kind:    "HarnessGitopsProjectMapping",
	})
	if spec != nil {
		mapping.Object["spec"] = spec
	}
	return mapping
}

func deleteProjectMapping(name string) {
	current := &infrastructurev1.HarnessGitopsProjectMapping{}
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, current)
	if err == nil {
		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
	}
}
