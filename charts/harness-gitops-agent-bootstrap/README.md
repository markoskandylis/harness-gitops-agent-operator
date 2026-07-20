# harness-gitops-agent-bootstrap

Bootstrap one Harness GitOps Agent instance managed by the
[harness-gitops-agent-operator](../../README.md). One release:

1. Creates a `HarnessGitopsAgent` custom resource (phase one).
2. The controller registers the agent with Harness, writes the agent token into
   the Secret named by `tokenSecretRef`, and waits if an optional AppProject
   mapping is configured but the AppProject does not exist or the Harness agent
   is not yet Connected and Healthy.
3. With `gitopsAgent.enabled=true`, installs the
   official [Harness gitops-helm chart](https://harness.github.io/gitops-helm/)
   (Argo CD + GitOps agent). The runtime consumes the controller-written token
   via `agent.existingSecrets.agentToken`.
4. The bundled Argo CRDs are installed with the runtime. The AppProject runs as
   a post-install/post-upgrade hook after those CRDs are established, so a clean
   cluster can install the runtime and AppProject in one Helm operation.
5. Once the AppProject exists and the Harness agent is Connected and Healthy,
   the controller creates and verifies the exact mapping and periodically checks
   both readiness and mapping state for external drift.

The agent token never appears in values, manifests, or Helm release state —
only the controller and the target-namespace Secret hold it.

## Prerequisites

- The operator (controller) is installed in the cluster and healthy
  (`charts/harness-gitops-agent-controller`).
- A Secret with a Harness API key exists in the namespace configured by the
  controller's `manager.apiKeySecretNamespace`, or in this release namespace
  when that setting is empty. Its name comes from
  `harnessAgent.spec.apiKeySecretRef` (default `harness-api-key-secret`) and its
  key is `api_key`. Use a least-privilege Harness service-account key scoped to
  the target org/project. Never commit or print its value.
- One agent instance per namespace: the runtime chart uses fixed component
  names (`gitops-agent`, `argocd-*`), and Harness expects one agent runtime per
  namespace.
- On clusters that already have the Argo CRDs (an existing Argo CD or another
  agent instance), set `gitopsAgent.argo-cd.crds.install=false`.
- When installing more than one instance on the same cluster, give each release
  a unique `gitopsAgent.agent.harnessName` (it names the agent
  ClusterRole/ClusterRoleBinding) and a unique
  `gitopsAgent.harness.identity.agentIdentifier`.

## Install

See [values-example.yaml](values-example.yaml) for a commented starting point.

```sh
helm dependency build .

# Phase one: CR only — the controller registers the agent and writes the token
helm upgrade --install my-agent . \
  --namespace my-agent-ns --create-namespace \
  --values my-values.yaml

# Wait for the controller-written token Secret
kubectl get secret <tokenSecretRef> -n my-agent-ns

# Phase two: install the agent runtime
helm upgrade my-agent . --namespace my-agent-ns \
  --reuse-values --set gitopsAgent.enabled=true --wait
```

The AppProject uses Helm hook weight `0` with
`before-hook-creation`. This ensures Argo CRDs exist before Helm asks
Kubernetes to build the AppProject. On upgrade, Helm recreates the AppProject
hook instead of patching it in place.

Uninstalling the release deletes the CR; the controller finalizer then
deregisters the agent from Harness.

## Key values

| Value | Meaning |
|---|---|
| `harnessAgent.spec.scope` | `PROJECT`, `ORG`, or `ACCOUNT`; drives which identity fields are required |
| `gitopsAgent.harness.identity.*` | Account/org/project/agent identifiers, used by both the CR and the runtime |
| `harnessAgent.spec.tokenSecretRef` | Secret the controller writes; **must equal** `gitopsAgent.agent.existingSecrets.agentToken` (render fails on mismatch) |
| `harnessAgent.spec.projectMapping` | Optional Argo `AppProject` → Harness project mapping |
| `gitopsAgent.enabled` | Phase switch: `false` = CR only, `true` = install the runtime |
| `gitopsAgent.agent.harnessName` | Names the agent ClusterRole/Binding; unique per install |
| `gitopsAgent.upgrader.enabled` | Off by default: the upgrader breaks with `existingSecrets` and pinned installs should not self-upgrade |

## CI/CD usage

The repository's CD pipeline uses this chart as its functional end-to-end
test: it installs one PROJECT-scoped and one ORG-scoped instance in separate
namespaces, verifies the agents report CONNECTED/HEALTHY in Harness, then
uninstalls and verifies deregistration. Identity values are injected by the
pipeline at runtime; only this chart and the example values live in the
repository.
