package resource

import (
	"context"
	"strings"
	"testing"

	"github.com/harness/harness-go-sdk/harness/nextgen"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

func TestNamespacePolicyIsExplicitAndFailClosed(t *testing.T) {
	if (NamespacePolicy{}).Allows("team-argo") {
		t.Fatal("zero policy must deny access")
	}
	for _, names := range [][]string{nil, {}, {"team-argo", "platform-argo"}, {" team-argo ", "team-argo"}} {
		policy, err := NewNamespacePolicy(names)
		if err != nil {
			t.Fatal(err)
		}
		if policy.Allows("team-dev") || policy.Allows("") || policy.Allows("team-argo/other") {
			t.Fatal("policy broadened namespace approval")
		}
		if len(names) != 0 && !policy.Allows("team-argo") {
			t.Fatal("approved namespace was denied")
		}
	}
	for _, bad := range []string{"", "*", "team-*", "../hga-system", "TEAM", strings.Repeat("n", 64)} {
		policy, err := NewNamespacePolicy([]string{"team-argo", bad})
		if err == nil || policy.Allows("team-argo") {
			t.Fatalf("invalid configuration %q must return a deny-all policy and an error", bad)
		}
	}
}

type recordingCredentialReader struct {
	client.Reader
	keys []client.ObjectKey
}

func (r *recordingCredentialReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.keys = append(r.keys, key)
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestCredentialResolutionUsesOnlyApprovedLocalSecretAndReloadsRotation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	local := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-key", Namespace: "team-argo"},
		Data: map[string][]byte{"api_key": []byte("local-test-key")}}
	global := local.DeepCopy()
	global.Namespace = "hga-system"
	global.Data["api_key"] = []byte("global-test-key")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(local, global).Build()
	reader := &recordingCredentialReader{Reader: kube}
	policy, err := NewNamespacePolicy([]string{"team-argo"})
	if err != nil {
		t.Fatal(err)
	}
	agent := &infrastructurev1.HarnessGitopsAgent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-argo"},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{ApiKeySecretRef: "api-key"}}
	for _, want := range []string{"local-test-key", "rotated-test-key"} {
		current := &corev1.Secret{}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(local), current); err != nil {
			t.Fatal(err)
		}
		current.Data["api_key"] = []byte(want)
		if err := kube.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		session, err := SessionForAgent(context.Background(), reader, policy, agent)
		if err != nil {
			t.Fatal(err)
		}
		key := session.AuthContext(context.Background()).Value(nextgen.ContextAPIKey).(nextgen.APIKey)
		if key.Key != want {
			t.Fatal("session selected a foreign or stale credential")
		}
	}
	if err := kube.Delete(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	if _, err := SessionForAgent(context.Background(), reader, policy, agent); !apierrors.IsNotFound(err) {
		t.Fatalf("missing local key must not fall back: %v", err)
	}
	for _, key := range reader.keys {
		if key != client.ObjectKeyFromObject(local) {
			t.Fatalf("cross-namespace or unexpected read: %v", key)
		}
	}
	before := len(reader.keys)
	if _, err := SessionForAgent(context.Background(), reader, NamespacePolicy{}, agent); err == nil {
		t.Fatal("missing policy must deny credential resolution")
	}
	for _, name := range []string{"", "hga-system/api-key", "../api-key", " api-key"} {
		agent.Spec.ApiKeySecretRef = name
		if _, err := SessionForAgent(context.Background(), reader, policy, agent); err == nil {
			t.Fatal("invalid Secret reference was accepted")
		}
	}
	if _, err := SessionForAgent(context.Background(), reader, policy, nil); err == nil {
		t.Fatal("nil Agent was accepted")
	}
	if len(reader.keys) != before {
		t.Fatal("rejected requests must not read credentials")
	}
}

func TestCredentialResolutionRejectsUnusableSecrets(t *testing.T) {
	policy, err := NewNamespacePolicy([]string{"team-argo"})
	if err != nil {
		t.Fatal(err)
	}
	agent := &infrastructurev1.HarnessGitopsAgent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-argo"},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{ApiKeySecretRef: "api-key"}}
	if _, err := SessionForAgent(context.Background(), nil, policy, agent); err == nil {
		t.Fatal("nil reader must fail safely")
	}
	for _, data := range []map[string][]byte{nil, {APIKeySecretKey: {}}, {APIKeySecretKey: []byte(" \n\t")}, {"wrong_key": []byte("key")}} {
		scheme := runtime.NewScheme()
		if err := corev1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-key", Namespace: agent.Namespace}, Data: data}
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		if _, err := SessionForAgent(context.Background(), reader, policy, agent); !apierrors.IsBadRequest(err) {
			t.Fatalf("unusable credential was not rejected: %v", err)
		}
	}
}
