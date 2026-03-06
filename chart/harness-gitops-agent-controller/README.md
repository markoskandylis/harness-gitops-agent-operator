# Harness GitOps Agent Controller Helm Chart

This chart deploys the `harness-gitops-agent-operator` controller manager.

## Installation (step-by-step)

1. Build and publish/push the controller image you want to run.

2. Install the CRD manually:

```bash
kubectl apply -f chart/harness-gitops-agent-controller/crds/harnessgitopsagents.infrastructure.kandylis.co.uk.yaml
```

3. Review and customize values:

```bash
cp chart/harness-gitops-agent-controller/values.yaml /tmp/hgac-values.yaml
```

4. Install the controller chart while skipping CRD install:

```bash
helm upgrade --install hgac chart/harness-gitops-agent-controller \
  --namespace harness-system \
  --create-namespace \
  --skip-crds \
  --values /tmp/hgac-values.yaml
```

5. Validate rollout:

```bash
kubectl get deploy,pod -n harness-system
kubectl logs -n harness-system deploy/hgac-harness-gitops-agent-controller -f
```

## Recommended production overrides

- Set `image.repository` to your registry image.
- Set `image.tag` to an immutable tag (or digest-pinned image).
- Set `manager.zapDevelopment=false`.
- Keep `leaderElection.enabled=true`.
- For HA, use `replicaCount >= 2` and `podDisruptionBudget.enabled=true`.
