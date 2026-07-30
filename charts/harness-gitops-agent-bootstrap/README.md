# harness-gitops-agent-bootstrap

Installs one Harness GitOps Agent runtime and its Kubernetes control-plane
resources:

- one `HarnessGitopsAgent`;
- zero or more `HarnessGitopsProjectMapping` resources, all referring to that
  Agent in the release namespace;
- the official Harness `gitops-helm` runtime;
- optionally, one convenience Argo CD `AppProject`.

The controller chart must be installed first. For a controller-created Agent,
the bootstrap chart then supports a single-release flow: Kubernetes creates the
Agent CR and runtime together, the runtime pods briefly wait for their token
Secret, and the controller registers the Agent and writes that Secret. Mapping
reconcilers wait until the referenced AppProject exists and the Harness Agent is
connected and healthy.

The token never appears in values, rendered manifests, or Helm release state.
Only the controller and the target Secret handle it.

## Prerequisites

- The controller from `charts/harness-gitops-agent-controller` is running. Its
  chart installs both required CRDs.
- A Secret containing the Harness API key exists in the controller's configured
  API-key namespace. The controller Helm chart defaults this to its own release
  namespace; a directly run controller without
  `--api-key-secret-namespace` instead reads from each Agent namespace. Its name
  is `harnessAgent.spec.apiKeySecretRef` and its data key is `api_key`.
- Use a least-privilege Harness service-account key. Never place the key in
  chart values.
- Run one Agent runtime per namespace because the upstream runtime uses fixed
  component names.
- Give every instance on the same cluster a unique
  `gitopsAgent.agent.harnessName` and Harness Agent identifier.

## Install

Copy [values-example.yaml](values-example.yaml), replace its placeholders, and
install:

```sh
helm dependency build .

helm upgrade --install my-agent . \
  --namespace my-agent-ns \
  --create-namespace \
  --values my-values.yaml \
  --wait \
  --timeout 10m
```

`gitopsAgent.enabled=true` and `appProject.enabled=false` are the safe defaults
for a clean cluster. The temporary `CreateContainerConfigError` while the token
Secret is absent is expected; the controller creates the Secret and the pods
then start.

### Optional AppProject convenience

The upstream dependency renders Argo CRDs as normal Helm templates. On a clean
cluster, Helm validates this chart's AppProject before that same release has
installed the CRD, so one render cannot safely create both.

The chart fails with a clear message when `appProject.enabled=true` and the
AppProject API is not already available. After the first install above:

```sh
helm upgrade my-agent . \
  --namespace my-agent-ns \
  --reuse-values \
  --set appProject.enabled=true \
  --wait
```

If the cluster already has the AppProject CRD, `appProject.enabled=true` works
on the first install. Set `gitopsAgent.argo-cd.crds.install=false` when another
Argo CD installation owns those CRDs. The convenience AppProject is always
created in the bootstrap release namespace because Mapping lookup and
`agentRef` are intentionally same-namespace.

## One Agent, many mappings

Mappings are top-level list entries, not fields embedded in the Agent:

```yaml
projectMappings:
  - name: payments
    appProject: payments
    orgId: platform
    projectId: payments
    autoCreateServiceEnv: false

  - name: orders
    appProject: orders
    orgId: commerce
    projectId: orders
    autoCreateServiceEnv: true
```

Each entry becomes an independent `HarnessGitopsProjectMapping`. A failure or
ownership decision for one mapping does not block status reporting for the
others.

Harness allows one Argo CD AppProject to map to only one Harness project. Do
not repeat an `appProject` for different Harness targets; create distinct Argo
AppProjects instead.

Agent identity and Mapping target fields are immutable Kubernetes API identity.
Replace the affected CR (or use a new Mapping name) to change them;
`adoptMappingId` remains mutable so a failed adoption can be corrected in
place.

The chart rejects blank or duplicate mapping names, blank AppProject names, and
targets that do not fit the Agent scope:

| Agent scope | Mapping target |
|---|---|
| `ACCOUNT` | `orgId` and `projectId` are required on every mapping |
| `ORG` | `projectId` is required; `orgId` is omitted or exactly matches the Agent org |
| `PROJECT` | both values are normally omitted and inherited; explicit values must exactly match the Agent |

The former `harnessAgent.spec.projectMapping` key is intentionally unsupported
and causes rendering to fail. Use `projectMappings` instead.

When `autoCreateServiceEnv` is enabled, Harness inspects Applications for
`harness.io/serviceRef` and `harness.io/envRef` labels. Their values become
Harness identifiers and invalid values can fail silently. After removing a
mapping, wait at least 30 seconds before re-adding it if the auto-create flow
must run; this is a Harness Redis-cache requirement, separate from the
controller's finalizer visibility grace. See the
[Harness BYOA mapping documentation](https://developer.harness.io/docs/continuous-delivery/gitops/connect-and-manage/multiple-argo-to-single-harness/).

## Ownership and adoption

Ownership is fail-closed:

- A mapping created and verified by the controller becomes `Managed`.
- A matching mapping that already exists is observed as `External` by default.
- Setting `adoptMappingId` asks the controller to adopt one exact existing
  Harness mapping.

Example:

```yaml
projectMappings:
  - name: adopted-payments
    appProject: payments
    orgId: platform
    projectId: payments
    adoptMappingId: existing-harness-mapping-id
```

Adoption succeeds only if that ID exists and its complete Agent, org, project,
AppProject, and option tuple matches the requested resource. A wrong or missing
ID fails closed and does not create a replacement. `adoptMappingId` is
correctable recovery input, so it can be fixed without replacing the Mapping
CR.

Deleting a `Managed` or `Adopted` Mapping CR authorizes deletion of that exact
remote mapping. Deleting an `External` Mapping CR does not. Similarly, an Agent
created by the controller is managed, while
`harnessAgent.spec.existingAgentIdentifier` refers to an external Agent that is
not deregistered by finalization.

`External` describes deletion ownership, not an observe-only policy. If an
external mapping later disappears, the controller restores the desired mapping
and owns the verified replacement as `Managed`.

### Existing Agent mode

When `harnessAgent.spec.existingAgentIdentifier` is set, the controller observes
that Agent but does not register it and does not write a token Secret.

- If the existing Agent runtime already runs elsewhere, set
  `gitopsAgent.enabled=false`; this chart then creates only the Agent and Mapping
  control-plane resources.
- If this chart should run the runtime for that existing Agent, create a valid
  token Secret in the bootstrap release namespace before installing. Set both
  `harnessAgent.spec.tokenSecretRef` and
  `gitopsAgent.agent.existingSecrets.agentToken` to that Secret name.

In either case, deleting the Agent CR leaves the external Harness Agent intact.

## Uninstall

```sh
helm uninstall my-agent --namespace my-agent-ns
```

Helm marks the Mapping and Agent resources for deletion. Their finalizers keep
the controller involved until managed or adopted Harness resources are cleaned
up; external resources remain. Keep the controller and API-key Secret available
until finalization finishes.

## Key values

| Value | Meaning |
|---|---|
| `harnessAgent.spec.scope` | `PROJECT`, `ORG`, or `ACCOUNT` |
| `gitopsAgent.harness.identity.*` | Shared account/org/project/Agent identity |
| `harnessAgent.spec.tokenSecretRef` | Secret written by the controller; must equal `gitopsAgent.agent.existingSecrets.agentToken` |
| `projectMappings` | Independent AppProject-to-Harness-project mappings |
| `projectMappings[].adoptMappingId` | Optional request to claim one exact existing mapping |
| `projectMappings[].autoCreateServiceEnv` | Enable Harness label-driven service/environment creation |
| `gitopsAgent.enabled` | Install the upstream runtime; defaults to `true` |
| `appProject.enabled` | Create one convenience AppProject, only when its CRD already exists |
| `gitopsAgent.agent.harnessName` | Names cluster-scoped runtime RBAC; keep unique per install |
