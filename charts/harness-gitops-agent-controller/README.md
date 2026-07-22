# Harness GitOps Agent Controller Helm Chart

Deploys the `harness-gitops-agent-operator` controller manager and its
`HarnessGitopsAgent` CRD.

## Prerequisites

- Kubernetes >= 1.25
- Cluster-admin for the install (the chart installs a cluster-scoped CRD)
- A published controller image (`image.repository` / `image.tag`)

## Installing the chart

```bash
helm upgrade --install hgac charts/harness-gitops-agent-controller \
  --namespace hga-system --create-namespace \
  --values my-values.yaml
```

Installs the CRD and the controller together; a later `helm upgrade` updates both.

## Uninstalling the chart

```bash
helm uninstall hgac -n hga-system
```

The CRD carries `helm.sh/resource-policy: keep`, so it and every `HarnessGitopsAgent`
survive uninstall. To also remove the CRD — **this deletes every `HarnessGitopsAgent`
in the cluster** — delete the CRs first (so finalizers can deregister the agents in
Harness), then `kubectl delete crd harnessgitopsagents.infrastructure.kandylis.co.uk`.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `crds.enabled` | bool | `true` | Install the `HarnessGitopsAgent` CRD with the chart. Set `false` to manage it out-of-band (e.g. by a platform team). |
| `crds.keep` | bool | `true` | Annotate the CRD with `helm.sh/resource-policy: keep` so `helm uninstall` never deletes it. |
| `replicaCount` | int | `1` | Controller replicas. Use `>= 2` with a PDB for HA. |
| `image.repository` | string | `mkandylis/harness-gitops-agent-operator` | Controller image repository. |
| `image.tag` | string | `v0.2.0` | Image tag. Pin to an immutable tag or digest in production. |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy. |
| `rbac.create` | bool | `true` | Create the controller ClusterRole and binding. |
| `serviceAccount.create` | bool | `true` | Create the controller ServiceAccount. |
| `leaderElection.enabled` | bool | `true` | Enable leader election (required for HA). |
| `manager.apiKeySecretNamespace` | string | `""` | Namespace to read each `apiKeySecretRef` from. Empty = the agent CR's own namespace. |
| `manager.appProjectPendingRetryInterval` | string | `20s` | Requeue interval while an AppProject/agent is not yet ready. |
| `manager.harnessMappingResyncInterval` | string | `5m` | Interval to re-verify a ready mapping against Harness. |
| `manager.metrics.bindAddress` | string | `"0"` | Metrics bind address (`"0"` disables the endpoint). |
| `manager.metrics.secure` | bool | `true` | Serve metrics over HTTPS with authn/authz. |
| `manager.enableHTTP2` | bool | `false` | Enable HTTP/2 (kept off for CVE mitigation). |
| `manager.zapDevelopment` | bool | `true` | Development-mode logging. Set `false` for JSON logs in production. |
| `podDisruptionBudget.enabled` | bool | `false` | Create a PodDisruptionBudget. |
| `resources` | object | see `values.yaml` | Controller resource requests/limits. |

See [`values.yaml`](./values.yaml) for the full list (probes, securityContext,
nodeSelector, tolerations, affinity, topology spread).

## Production notes

- Pin `image.tag` to an immutable tag or digest.
- Set `manager.zapDevelopment=false` for JSON logs.
- HA: `replicaCount >= 2`, `podDisruptionBudget.enabled=true`, `leaderElection.enabled=true`.
- Set `manager.apiKeySecretNamespace` per your API-key responsibility model
  (controller namespace to centralize credentials, empty to keep them beside each agent CR).
