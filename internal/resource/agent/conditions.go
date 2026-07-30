package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/harness/harness-go-sdk/harness/nextgen"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

const (
	harnessAgentReadyCondition           = "Ready"
	harnessAgentHealthyCondition         = "Healthy"
	harnessAgentReasonWaitingForMappings = "WaitingForMappings"
	harnessAgentReasonHealthy            = "AgentHealthy"
	harnessAgentReasonUnhealthy          = "AgentUnhealthy"
	harnessAgentReasonAbsent             = "AgentAbsent"
	harnessAgentReasonHealthUnreadable   = "HealthUnreadable"

	agentHealthFastResync            = 30 * time.Second
	DefaultAgentHealthResyncInterval = 5 * time.Minute
)

func readinessFromAgent(agent nextgen.V1Agent) AgentReadiness {
	readiness := AgentReadiness{Exists: true}
	if agent.Health == nil {
		readiness.Message = "Harness GitOps agent health has not been reported yet"
		return readiness
	}

	connectionStatus := nextgen.CONNECTED_STATUS_UNSET_V1ConnectedStatus
	if agent.Health.ConnectionStatus != nil {
		connectionStatus = *agent.Health.ConnectionStatus
	}
	healthStatus := nextgen.HEALTH_STATUS_UNSET_Servicev1HealthStatus
	healthMessage := ""
	if agent.Health.HarnessGitopsAgent != nil {
		healthMessage = strings.TrimSpace(agent.Health.HarnessGitopsAgent.Message)
		if agent.Health.HarnessGitopsAgent.Status != nil {
			healthStatus = *agent.Health.HarnessGitopsAgent.Status
		}
	}

	readiness.Ready = connectionStatus == nextgen.CONNECTED_V1ConnectedStatus &&
		healthStatus == nextgen.HEALTHY_Servicev1HealthStatus
	if readiness.Ready {
		readiness.Message = "Harness GitOps agent is Connected and Healthy"
		return readiness
	}

	readiness.Message = fmt.Sprintf(
		"Harness GitOps agent is not ready: connection=%s health=%s",
		connectionStatus,
		healthStatus,
	)
	if healthMessage != "" {
		readiness.Message += ": " + healthMessage
	}
	return readiness
}

func (r *Reconciler) refreshAgentHealth(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
) (ctrl.Result, error) {
	session, err := SessionForAgent(
		ctx,
		r.apiReader(),
		r.APIKeySecretNamespace,
		agentCR,
	)
	return r.agentHealthResult(ctx, agentCR, session, agentIdentifier, err)
}

func (r *Reconciler) agentHealthResult(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	session *harnessapi.Session,
	agentIdentifier string,
	sessionErr error,
) (ctrl.Result, error) {
	requeueAfter, err := r.publishAgentHealth(
		ctx,
		agentCR,
		session,
		agentIdentifier,
		sessionErr,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// publishAgentHealth keeps unreadable health distinct from observed unhealthy
// state so callers never mistake a permission or transport failure for absence.
func (r *Reconciler) publishAgentHealth(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	session *harnessapi.Session,
	agentIdentifier string,
	sessionErr error,
) (time.Duration, error) {
	status := metav1.ConditionUnknown
	reason := harnessAgentReasonHealthUnreadable
	message := ""

	readiness := AgentReadiness{}
	err := sessionErr
	if err == nil && strings.TrimSpace(agentIdentifier) != "" {
		readiness, err = r.harnessAgentAPI().Readiness(
			ctx,
			session,
			harnessAgentFor(agentCR, agentIdentifier),
		)
	}
	switch {
	case err != nil:
		message = "Harness GitOps agent health could not be read: " + err.Error()
	case strings.TrimSpace(agentIdentifier) == "":
		message = "Harness GitOps agent has no identifier to read health for"
	case !readiness.Exists:
		status = metav1.ConditionFalse
		reason = harnessAgentReasonAbsent
		message = "Harness GitOps agent does not exist"
	case readiness.Ready:
		status = metav1.ConditionTrue
		reason = harnessAgentReasonHealthy
		message = strings.TrimSpace(readiness.Message)
		if message == "" {
			message = "Harness GitOps agent is Connected and Healthy"
		}
	default:
		status = metav1.ConditionFalse
		reason = harnessAgentReasonUnhealthy
		message = strings.TrimSpace(readiness.Message)
		if message == "" {
			message = "Harness GitOps agent is not Connected and Healthy"
		}
	}

	if err := r.setAgentCondition(
		ctx,
		agentCR,
		harnessAgentHealthyCondition,
		status,
		reason,
		message,
	); err != nil {
		return 0, err
	}
	if status == metav1.ConditionTrue {
		return r.agentHealthResyncInterval(), nil
	}
	return agentHealthFastResync, nil
}

func (r *Reconciler) agentHealthResyncInterval() time.Duration {
	if r.AgentHealthResyncInterval > 0 {
		return r.AgentHealthResyncInterval
	}
	return DefaultAgentHealthResyncInterval
}

func (r *Reconciler) setAgentCondition(
	ctx context.Context,
	agent *infrastructurev1.HarnessGitopsAgent,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	before := agent.DeepCopy().Status
	apiMeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: agent.Generation,
		Reason:             reason,
		Message:            message,
	})
	if reflect.DeepEqual(before, agent.Status) {
		return nil
	}
	return r.Status().Update(ctx, agent)
}

func (r *Reconciler) setAgentWaitingForMappings(
	ctx context.Context,
	agent *infrastructurev1.HarnessGitopsAgent,
	names []string,
) error {
	return r.setAgentCondition(
		ctx,
		agent,
		harnessAgentReadyCondition,
		metav1.ConditionFalse,
		harnessAgentReasonWaitingForMappings,
		fmt.Sprintf(
			"Waiting for HarnessGitopsProjectMapping resources to be deleted: %s",
			strings.Join(names, ", "),
		),
	)
}
