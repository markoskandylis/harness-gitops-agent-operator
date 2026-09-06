package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	resourceutil "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource"
)

func ownedTokenSecret(t *testing.T, fixture *agentRegistrationFixture) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: fixture.agent.Spec.TokenSecretRef, Namespace: fixture.agent.Namespace},
		Data:       map[string][]byte{gitopsAgentTokenSecretKey: []byte("existing-test-token")},
	}
	if err := ctrl.SetControllerReference(fixture.agent, secret, fixture.reconciler.Scheme); err != nil {
		t.Fatal(err)
	}
	return secret
}

func assertNoHarnessCalls(t *testing.T, fixture *agentRegistrationFixture) {
	t.Helper()
	api := fixture.agentAPI
	if api.lookupCalls+api.createCalls+api.resolveCalls+api.readinessCalls+api.deleteCalls != 0 {
		t.Fatal("unsafe token destination triggered a Harness API call")
	}
}

func TestTokenSecretRefusesUnsafeDestinationsWithoutCredentialReads(t *testing.T) {
	for _, name := range []string{"input secret", "default collides with input", "cross namespace", "invalid", "unapproved", "missing UID"} {
		t.Run(name, func(t *testing.T) {
			fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
			agent := fixture.agent.DeepCopy()
			switch name {
			case "input secret":
				agent.Spec.TokenSecretRef = agent.Spec.ApiKeySecretRef
			case "default collides with input":
				agent.Spec.TokenSecretRef = ""
				agent.Spec.ApiKeySecretRef = agent.Name + "-agent-token"
			case "cross namespace":
				agent.Spec.TokenSecretRef = "hga-system/token"
			case "invalid":
				agent.Spec.TokenSecretRef = " Token"
			case "unapproved":
				fixture.reconciler.NamespacePolicy = resourceutil.NamespacePolicy{}
			case "missing UID":
				agent.UID = ""
			}
			fixture.reconciler.APIReader = forbiddenAgentReader{}
			if _, err := fixture.reconciler.readAgentTokenSecret(context.Background(), agent); err == nil {
				t.Fatal("unsafe token destination was accepted for reading")
			}
			if err := fixture.reconciler.upsertAgentTokenSecret(context.Background(), agent, "test-token"); err == nil {
				t.Fatal("unsafe token destination was accepted for writing")
			}
		})
	}
}

func TestReconcileRejectsForeignOrUnsafeTokenSecretsBeforeHarnessCalls(t *testing.T) {
	for _, name := range []string{"ownerless", "label only", "different UID", "different kind", "non-token data", "deleting", "empty immutable"} {
		t.Run(name, func(t *testing.T) {
			fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
			secret := ownedTokenSecret(t, fixture)
			switch name {
			case "ownerless", "label only":
				secret.OwnerReferences = nil
				if name == "label only" {
					secret.Labels = map[string]string{ManagedByLabelKey: ManagedByLabelValue}
				}
			case "different UID":
				secret.OwnerReferences[0].UID = "other-agent-uid"
			case "different kind":
				secret.OwnerReferences[0].Kind = "UnrelatedResource"
			case "non-token data":
				secret.Data[resourceutil.APIKeySecretKey] = []byte("must-not-reach-runtime")
			case "deleting":
				secret.Finalizers = []string{"testing.example/hold"}
			case "empty immutable":
				immutable := true
				secret.Immutable = &immutable
				secret.Data = nil
			}
			if err := fixture.client.Create(context.Background(), secret); err != nil {
				t.Fatal(err)
			}
			if name == "deleting" {
				if err := fixture.client.Delete(context.Background(), secret); err != nil {
					t.Fatal(err)
				}
			}
			before := &corev1.Secret{}
			if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(secret), before); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.reconciler.Reconcile(context.Background(), ctrlRequestFor(fixture.agent)); err == nil {
				t.Fatal("unsafe token Secret was silently reused")
			}
			if err := fixture.reconciler.upsertAgentTokenSecret(context.Background(), fixture.agent, "replacement-token"); err == nil {
				t.Fatal("unsafe token Secret was adopted or overwritten")
			}
			after := &corev1.Secret{}
			if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(secret), after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected token Secret was mutated")
			}
			assertNoHarnessCalls(t, fixture)
			healthy := apiMeta.FindStatusCondition(fixture.getAgent(t).Status.Conditions, harnessAgentHealthyCondition)
			if healthy == nil || healthy.Status != metav1.ConditionUnknown || healthy.Reason != harnessAgentReasonTokenSecretUnavailable {
				t.Fatal("unsafe token destination did not publish an actionable condition")
			}
		})
	}
}

type tokenReadFailure struct {
	client.Reader
	key client.ObjectKey
	err error
}

func (r tokenReadFailure) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key == r.key {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestTokenSecretReadFailureDoesNotTriggerRegistrationOrRegeneration(t *testing.T) {
	for _, cause := range []error{
		apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "token", errors.New("denied")),
		context.DeadlineExceeded,
	} {
		fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
		key, err := fixture.reconciler.tokenSecretKey(fixture.agent)
		if err != nil {
			t.Fatal(err)
		}
		fixture.reconciler.APIReader = tokenReadFailure{Reader: fixture.client, key: key, err: cause}
		if _, err := fixture.reconciler.Reconcile(context.Background(), ctrlRequestFor(fixture.agent)); !errors.Is(err, cause) {
			t.Fatalf("read failure was mistaken for absence: %v", err)
		}
		assertNoHarnessCalls(t, fixture)
	}
}

func TestTokenSecretWriteIsOwnedNamespaceLocalAndIdempotent(t *testing.T) {
	fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
	fixture.agent.Spec.TokenSecretRef = ""
	ctx := context.Background()
	if err := fixture.reconciler.upsertAgentTokenSecret(ctx, fixture.agent, "test-token"); err != nil {
		t.Fatal(err)
	}
	first, err := fixture.reconciler.readAgentTokenSecret(ctx, fixture.agent)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.Name != fixture.agent.Name+"-agent-token" || first.Namespace != fixture.agent.Namespace ||
		first.Labels[ManagedByLabelKey] != ManagedByLabelValue || len(first.Data) != 1 {
		t.Fatal("token Secret has incorrect namespace, default name, labels or data")
	}
	if err := fixture.reconciler.upsertAgentTokenSecret(ctx, fixture.agent, "test-token"); err != nil {
		t.Fatal(err)
	}
	second, err := fixture.reconciler.readAgentTokenSecret(ctx, fixture.agent)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("idempotent token write changed the Secret")
	}
	second.Data = nil
	if err := fixture.client.Update(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := fixture.reconciler.upsertAgentTokenSecret(ctx, fixture.agent, "recovered-test-token"); err != nil {
		t.Fatal(err)
	}
	third, err := fixture.reconciler.readAgentTokenSecret(ctx, fixture.agent)
	if err != nil || string(third.Data[gitopsAgentTokenSecretKey]) != "recovered-test-token" {
		t.Fatal("owned empty token Secret was not repaired")
	}
	if err := fixture.reconciler.upsertAgentTokenSecret(ctx, fixture.agent, " "); err == nil {
		t.Fatal("empty token overwrote an existing credential")
	}
}

type tokenRaceWriter struct {
	client.Client
	beforeWrite func(context.Context) error
}

func (w tokenRaceWriter) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := w.beforeWrite(ctx); err != nil {
		return err
	}
	return w.Client.Create(ctx, obj, opts...)
}

func (w tokenRaceWriter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if err := w.beforeWrite(ctx); err != nil {
		return err
	}
	return w.Client.Update(ctx, obj, opts...)
}

func TestTokenSecretWriteLosesRacesWithoutOverwritingForeignData(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent create", true: "concurrent ownership change"}[existing], func(t *testing.T) {
			fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
			secret := ownedTokenSecret(t, fixture)
			secret.Data = nil
			if existing {
				if err := fixture.client.Create(context.Background(), secret); err != nil {
					t.Fatal(err)
				}
			}
			var raced *corev1.Secret
			fixture.reconciler.Client = tokenRaceWriter{
				Client: fixture.client,
				beforeWrite: func(ctx context.Context) error {
					raced = secret.DeepCopy()
					raced.OwnerReferences = nil
					raced.Data = map[string][]byte{"foreign": []byte("must-survive")}
					if existing {
						return fixture.client.Update(ctx, raced)
					}
					return fixture.client.Create(ctx, raced)
				},
			}
			err := fixture.reconciler.upsertAgentTokenSecret(context.Background(), fixture.agent, "new-test-token")
			if existing && !apierrors.IsConflict(err) || !existing && !apierrors.IsAlreadyExists(err) {
				t.Fatalf("write race was not surfaced: %v", err)
			}
			after := &corev1.Secret{}
			if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(secret), after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(raced.Data, after.Data) || len(after.OwnerReferences) != 0 {
				t.Fatal("write race overwrote or adopted the competing Secret")
			}
		})
	}
}
