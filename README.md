# Harness GitOps Agent Operator

Kubernetes controller that manages Harness GitOps Agent lifecycle using the `HarnessGitopsAgent` custom resource.

## Overview

The controller reconciles `HarnessGitopsAgent` resources and performs:

1. Agent registration in Harness using Harness Go SDK.
2. Status update with the resolved Harness agent identifier.
3. Token secret creation/update in Kubernetes.
4. Agent deletion in Harness during CR deletion (finalizer-driven).

## Features

1. Idempotent reconcile for create/update/delete.
2. Finalizer-based external cleanup.
3. Token secret written for Harness `gitops-helm` consumption.
4. Safe delete behavior:
   If Harness credentials are missing, finalizer is retained so resources are not orphaned.
5. "Agent not found" during delete is treated as already deleted.

## API

- Group/Version: `infrastructure.kandylis.co.uk/v1`
- Kind: `HarnessGitopsAgent`

Required spec fields:

1. `name`
2. `identifier`
3. `accountId`
4. `orgId`
5. `projectId`
6. `operator`
7. `apiKeySecretRef`
8. `tokenSecretRef`

Common optional/defaulted fields:

1. `scope` (default `PROJECT`)
2. `type` (default `KUBERNETES` in CRD)

## Secret Contract

Input secret (referenced by `spec.apiKeySecretRef`) must contain:

- key: `api_key`

Output token secret (`spec.tokenSecretRef`) contains:

- key: `GITOPS_AGENT_TOKEN`

Note: legacy token key support was removed. Only `GITOPS_AGENT_TOKEN` is written.

## Controller Helm Chart

Chart path:

- `chart/harness-gitops-agent-controller`

Resources installed:

1. ServiceAccount
2. RBAC (ClusterRole/ClusterRoleBinding)
3. Leader election Role/RoleBinding
4. Deployment
5. CRD from `chart/harness-gitops-agent-controller/crds/`

## Bootstrap Helm Chart (CR + GitOps Agent)

Chart path:

- `chart/harness-gitops-agent-bootstrap`

Purpose:

1. Creates `HarnessGitopsAgent` CR (controller registers agent in Harness and writes token secret).
2. Installs Harness `gitops-helm` runtime in the same namespace.

Install example:

```sh
helm upgrade --install hub-bootstrap chart/harness-gitops-agent-bootstrap \
  -n argocd-agent \
  --create-namespace \
  --set gitopsAgent.harness.identity.accountIdentifier="<ACCOUNT_ID>" \
  --set gitopsAgent.harness.identity.orgIdentifier="<ORG_ID>" \
  --set gitopsAgent.harness.identity.projectIdentifier="<PROJECT_ID>" \
  --set gitopsAgent.harness.identity.agentIdentifier="hubagent" \
  --set harnessAgent.spec.apiKeySecretRef="harness-api-key-secret" \
  --set harnessAgent.spec.tokenSecretRef="my-agent-token" \
  --set gitopsAgent.agent.existingSecrets.agentToken="my-agent-token"
```

Notes:

1. Controller must already be installed.
2. Secret `harness-api-key-secret` with key `api_key` must exist in `argocd-agent`.
3. Keep `harnessAgent.spec.tokenSecretRef` and `gitopsAgent.agent.existingSecrets.agentToken` identical.

## Quickstart (Local k3d)

### Prerequisites

1. Docker
2. kubectl
3. Helm 3
4. k3d

### 1. Build local controller image

```sh
docker build -t harness-gitops-agent-operator:dev .
```

### 2. Import image into k3d cluster

```sh
k3d image import -c hub harness-gitops-agent-operator:dev
```

### 3. Install controller

```sh
kubectl apply -f chart/harness-gitops-agent-controller/crds/harnessgitopsagents.infrastructure.kandylis.co.uk.yaml

helm upgrade --install hgac chart/harness-gitops-agent-controller \
  --namespace harness-system \
  --create-namespace \
  --skip-crds
```

### 4. Verify controller

```sh
kubectl get deploy,pod -n harness-system
kubectl logs -n harness-system deploy/hgac-harness-gitops-agent-controller -f
```

## Usage

### 1. Create Harness API key secret

```sh
kubectl create namespace argocd-agent --dry-run=client -o yaml | kubectl apply -f -
kubectl -n argocd-agent create secret generic harness-api-key-secret \
  --from-literal=api_key='<HARNESS_PAT>'
```

### 2. Apply custom resource

```sh
kubectl apply -f test/manifests/my-agent.yaml
```

### 3. Verify reconcile

```sh
kubectl get harnessgitopsagent -n argocd-agent hub-agent -o yaml
kubectl get secret -n argocd-agent my-agent-token -o yaml
```

Expected:

1. `.status.agentIdentifier` is populated.
2. Finalizer is present while resource exists.
3. Token secret contains `GITOPS_AGENT_TOKEN`.

## Deletion

```sh
kubectl delete -f test/manifests/my-agent.yaml
```

The controller removes the agent from Harness and then removes the finalizer.

## Troubleshooting

If CR is stuck in `Terminating`:

1. Ensure controller is running in `harness-system`.
2. Ensure `spec.apiKeySecretRef` exists in CR namespace and contains `api_key`.
3. Check controller logs:

```sh
kubectl logs -n harness-system deploy/hgac-harness-gitops-agent-controller --tail=200
```

## Development

```sh
go test ./...
make manifests
make generate
```

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0.
