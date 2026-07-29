# Harness GitOps Agent Operator

Kubernetes controller that manages Harness GitOps Agent lifecycle using the `HarnessGitopsAgent` custom resource.

## Overview

The controller reconciles `HarnessGitopsAgent` resources and performs:

1. Agent registration in Harness using Harness Go SDK.
2. Status update with the resolved Harness agent identifier.
3. Token secret creation/update in Kubernetes.
4. Controller-created agent deletion in Harness during CR deletion
   (finalizer-driven). Agents referenced with `existingAgentIdentifier` are
   never deleted.

## Features

1. Idempotent reconcile for create/update/delete.
2. Finalizer-based external cleanup.
3. Token secret written for Harness `gitops-helm` consumption.
4. Safe delete behavior:
   If Harness credentials are missing, finalizer is retained so resources are not orphaned.
5. "Agent not found" during delete is treated as already deleted.
6. Harness agent identity is immutable after CR creation. Replace the CR to
   change account, scope, identifier, or adoption mode.
7. Existing Harness agents must be adopted explicitly with
   `spec.existingAgentIdentifier`; identifier conflicts are never adopted
   automatically.

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


## Controller Helm Chart

Chart path:

- `charts/harness-gitops-agent-controller`

Resources installed:

1. ServiceAccount
2. RBAC (ClusterRole/ClusterRoleBinding)
3. Leader election Role/RoleBinding
4. Deployment
5. CRD (installed with the chart by default; `crds.enabled=true`, kept on uninstall via `helm.sh/resource-policy: keep`)

## Bootstrap Helm Chart (CR + GitOps Agent)

Chart path:

- `charts/harness-gitops-agent-bootstrap`

Purpose:

1. Creates `HarnessGitopsAgent` CR (controller registers agent in Harness and writes token secret).
2. Installs Harness `gitops-helm` runtime in the same namespace.

Install the controller first. The bootstrap then starts with
`gitopsAgent.enabled=false`, so its first phase creates only the CR. Enable the
runtime after the controller has written the token secret.

Install example (phase one - CR only):

```sh
helm dependency build charts/harness-gitops-agent-bootstrap

helm upgrade --install hub-bootstrap charts/harness-gitops-agent-bootstrap \
  -n argocd-agent \
  --create-namespace \
  --set gitopsAgent.harness.identity.accountIdentifier="<ACCOUNT_ID>" \
  --set gitopsAgent.harness.identity.orgIdentifier="<ORG_ID>" \
  --set gitopsAgent.harness.identity.projectIdentifier="<PROJECT_ID>" \
  --set gitopsAgent.harness.identity.agentIdentifier="hubagent" \
  --set gitopsAgent.agent.harnessName="hub-bootstrap" \
  --set harnessAgent.spec.apiKeySecretRef="harness-api-key-secret" \
  --set harnessAgent.spec.tokenSecretRef="my-agent-token" \
  --set gitopsAgent.agent.existingSecrets.agentToken="my-agent-token"
```

Phase two - install the GitOps runtime and Argo CRDs once the token secret exists
(`kubectl get secret my-agent-token -n argocd-agent`):

```sh
helm upgrade hub-bootstrap charts/harness-gitops-agent-bootstrap \
  -n argocd-agent \
  --reuse-values \
  --set gitopsAgent.enabled=true \
  --set appProject.enabled=false \
  --wait
```

When an AppProject is configured, enable it after its CRD exists:

```sh
helm upgrade hub-bootstrap charts/harness-gitops-agent-bootstrap \
  -n argocd-agent \
  --reuse-values \
  --set appProject.enabled=true \
  --wait
```

Notes:

1. Controller must already be installed.
2. Secret `harness-api-key-secret` with key `api_key` must exist in `argocd-agent`.
3. Keep `harnessAgent.spec.tokenSecretRef` and `gitopsAgent.agent.existingSecrets.agentToken` identical.
4. The gitops-helm dependency archive is gitignored; run `helm dependency build` after a clean checkout.
5. If the Argo CRDs already exist, runtime and AppProject enablement can be combined.
6. See `charts/harness-gitops-agent-bootstrap/README.md` and `values-example.yaml` for the full contract.

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
helm upgrade --install hgac charts/harness-gitops-agent-controller \
  --namespace harness-system \
  --create-namespace
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

Edit the `<...>` placeholders in the manifest first (account, org, project
identifiers). See `test/manifests/README.md` for all scope variants.

```sh
kubectl apply -f test/manifests/project-agent.yaml
```

### 3. Verify reconcile

```sh
kubectl get harnessgitopsagent -n argocd-agent project-agent -o yaml
kubectl get secret -n argocd-agent project-agent-token -o yaml
```

Expected:

1. `.status.agentIdentifier` is populated.
2. Finalizer is present while resource exists.
3. Token secret contains `GITOPS_AGENT_TOKEN`.

## Deletion

```sh
kubectl delete -f test/manifests/project-agent.yaml
```

For an agent created by this CR, the controller removes the agent from Harness
and then removes the finalizer. An agent referenced by
`spec.existingAgentIdentifier` is external: deleting the CR leaves that agent
running. Mappings found before this controller created them are also treated as
external and left intact.

If Harness accepted a create request but Kubernetes status could not be written,
the controller fails closed instead of guessing ownership. Remove the incomplete
remote agent or mapping in Harness, then retry/recreate the CR. This recovery is
manual because deleting an unproven external resource automatically would be
unsafe.

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
