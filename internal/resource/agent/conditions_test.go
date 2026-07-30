package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPublishAgentHealthPreservesTriStateAndSchedulesRefresh(t *testing.T) {
	readErr := errors.New("health endpoint unavailable")
	tests := []struct {
		name          string
		readiness     AgentReadiness
		readinessErr  error
		sessionErr    error
		wantStatus    metav1.ConditionStatus
		wantReason    string
		wantRequeue   time.Duration
		wantReadCalls int
	}{
		{
			name: "connected and healthy",
			readiness: AgentReadiness{
				Exists:  true,
				Ready:   true,
				Message: "Connected and Healthy",
			},
			wantStatus:    metav1.ConditionTrue,
			wantReason:    harnessAgentReasonHealthy,
			wantRequeue:   DefaultAgentHealthResyncInterval,
			wantReadCalls: 1,
		},
		{
			name: "observed unhealthy",
			readiness: AgentReadiness{
				Exists:  true,
				Message: "Disconnected",
			},
			wantStatus:    metav1.ConditionFalse,
			wantReason:    harnessAgentReasonUnhealthy,
			wantRequeue:   agentHealthFastResync,
			wantReadCalls: 1,
		},
		{
			name:          "observed absent",
			wantStatus:    metav1.ConditionFalse,
			wantReason:    harnessAgentReasonAbsent,
			wantRequeue:   agentHealthFastResync,
			wantReadCalls: 1,
		},
		{
			name:          "read failure remains unknown",
			readinessErr:  readErr,
			wantStatus:    metav1.ConditionUnknown,
			wantReason:    harnessAgentReasonHealthUnreadable,
			wantRequeue:   agentHealthFastResync,
			wantReadCalls: 1,
		},
		{
			name:          "session failure remains unknown",
			sessionErr:    errors.New("API key Secret unavailable"),
			wantStatus:    metav1.ConditionUnknown,
			wantReason:    harnessAgentReasonHealthUnreadable,
			wantRequeue:   agentHealthFastResync,
			wantReadCalls: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAgentRegistrationFixture(t, "PROJECT", nil)
			fixture.agentAPI.readiness = test.readiness
			fixture.agentAPI.readinessErr = test.readinessErr

			requeueAfter, err := fixture.reconciler.publishAgentHealth(
				context.Background(),
				fixture.agent,
				nil,
				fixture.agent.Spec.Identifier,
				test.sessionErr,
			)
			if err != nil {
				t.Fatalf("publish health: %v", err)
			}
			if requeueAfter != test.wantRequeue {
				t.Fatalf("requeueAfter = %s, want %s", requeueAfter, test.wantRequeue)
			}
			if fixture.agentAPI.readinessCalls != test.wantReadCalls {
				t.Fatalf(
					"readiness calls = %d, want %d",
					fixture.agentAPI.readinessCalls,
					test.wantReadCalls,
				)
			}

			current := fixture.getAgent(t)
			healthy := apiMeta.FindStatusCondition(
				current.Status.Conditions,
				harnessAgentHealthyCondition,
			)
			if healthy == nil {
				t.Fatal("Healthy condition was not published")
			}
			if healthy.Status != test.wantStatus || healthy.Reason != test.wantReason {
				t.Fatalf(
					"Healthy = %s/%s, want %s/%s",
					healthy.Status,
					healthy.Reason,
					test.wantStatus,
					test.wantReason,
				)
			}
			if healthy.ObservedGeneration != current.Generation {
				t.Fatalf(
					"observedGeneration = %d, want %d",
					healthy.ObservedGeneration,
					current.Generation,
				)
			}
		})
	}
}

func TestPublishAgentHealthSkipsUnchangedStatusWrite(t *testing.T) {
	fixture := newAgentRegistrationFixture(t, "ACCOUNT", nil)
	fixture.agentAPI.readiness = AgentReadiness{
		Exists:  true,
		Ready:   true,
		Message: "Connected and Healthy",
	}

	if _, err := fixture.reconciler.publishAgentHealth(
		context.Background(),
		fixture.agent,
		nil,
		fixture.agent.Spec.Identifier,
		nil,
	); err != nil {
		t.Fatalf("initial health publish: %v", err)
	}
	before := fixture.client.updateCalls
	current := fixture.getAgent(t)
	if _, err := fixture.reconciler.publishAgentHealth(
		context.Background(),
		current,
		nil,
		current.Spec.Identifier,
		nil,
	); err != nil {
		t.Fatalf("repeat health publish: %v", err)
	}
	if fixture.client.updateCalls != before {
		t.Fatalf(
			"unchanged health caused a status update: got %d calls, want %d",
			fixture.client.updateCalls,
			before,
		)
	}
}
