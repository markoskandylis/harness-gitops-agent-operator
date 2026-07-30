# Harness GitOps Agent Operator

A Kubernetes operator for Harness GitOps Agents and their Argo CD AppProject
mappings.

The API intentionally uses two resources:

- `HarnessGitopsAgent` manages one Harness Agent registration and its runtime
  token Secret.
- `HarnessGitopsProjectMapping` manages or observes one AppProject-to-Harness
  project mapping.

One Agent can therefore serve any number of Mapping resources. Each mapping has
its own readiness, remote identity, ownership, and cleanup lifecycle.

The editable architecture diagram is in
[docs/architecture.drawio](docs/architecture.drawio).

## Architecture

One controller-runtime manager runs two reconcilers:

| Reconciler | Responsibility |
|---|---|
| Agent | Register or reference a Harness Agent, recover ambiguous creates, write its token Secret, and coordinate dependent Mapping deletion. |
| Project Mapping | Resolve the Agent and target scopes, wait for the AppProject and healthy Agent, then create, verify, adopt, observe, or delete one Harness mapping. |

The root `internal/controller/setup.go` is a small registration façade. Agent
logic lives in `internal/controller/agent/`; Mapping logic lives in
`internal/controller/projectmapping/`. Harness SDK session construction,
Secret lookup, error handling, identifier candidates, Agent calls, Mapping
calls, and readiness checks live behind the shared `internal/harness/`
boundary.

```text
HarnessGitopsAgent ──1:N──> HarnessGitopsProjectMapping ──1:1──> AppProject
        │                              │
        │                              └────────────> Harness mapping row
        ├────────────> token Secret ────────────────> GitOps Agent runtime
        └───────────────────────────────────────────> Harness Agent
```

Mapping and Agent events requeue each other through a same-namespace
`agentRef`. There are no owner references between the two CRDs; explicit
finalizers provide safe external-resource deletion ordering.

## Resource model

API group/version: `infrastructure.kandylis.co.uk/v1`

| Kind | Short name | Purpose |
|---|---|---|
| `HarnessGitopsAgent` | `hga` | One Harness Agent and, for controller-created Agents, its Kubernetes token Secret. |
| `HarnessGitopsProjectMapping` | `hgapm` | One Argo CD AppProject mapped to one Harness project through a same-namespace Agent. |

Agent identity fields are immutable. Mapping identity fields are also immutable;
replace the Mapping to change its Agent, AppProject, target project, or
`autoCreateServiceEnv`. `adoptMappingId` remains mutable so an adoption attempt
can be corrected without changing mapping identity.

### Scope resolution

Agent lookup scope and mapping target scope are separate. This is essential at
account level: an `ACCOUNT` Agent has no Agent org/project, while every Mapping
still targets a concrete Harness org/project.

| Agent scope | Agent identity sent to Harness | Mapping target |
|---|---|---|
| `ACCOUNT` | Account only; Agent `orgId` and `projectId` are empty | Each Mapping requires its own `orgId` and `projectId` |
| `ORG` | Account plus Agent `orgId`; Agent `projectId` is empty | Mapping requires `projectId`; `orgId` is omitted or exactly matches the Agent org |
| `PROJECT` | Account plus Agent `orgId` and `projectId` | Mapping values are normally omitted and inherited; explicit values must exactly match the Agent |

The controller preserves these as two different tuples in status and in Harness
API calls. It never substitutes a target org/project into an account-scoped
Agent lookup.

### One Agent, many mappings

This account-scoped example keeps Agent identity at account level and gives each
Mapping an independent target:

```yaml
apiVersion: infrastructure.kandylis.co.uk/v1
kind: HarnessGitopsAgent
metadata:
  name: account-agent
  namespace: gitops-agent
spec:
  name: account-agent
  identifier: account_agent
  accountId: my-account
  operator: ARGO
  scope: ACCOUNT
  type: MANAGED_ARGO_PROVIDER
  apiKeySecretRef: harness-api-key-secret
  tokenSecretRef: account-agent-token
---
apiVersion: infrastructure.kandylis.co.uk/v1
kind: HarnessGitopsProjectMapping
metadata:
  name: platform-payments
  namespace: gitops-agent
spec:
  agentRef:
    name: account-agent
  appProject: payments
  orgId: platform
  projectId: payments
  autoCreateServiceEnv: false
---
apiVersion: infrastructure.kandylis.co.uk/v1
kind: HarnessGitopsProjectMapping
metadata:
  name: commerce-orders
  namespace: gitops-agent
spec:
  agentRef:
    name: account-agent
  appProject: orders
  orgId: commerce
  projectId: orders
  autoCreateServiceEnv: true
```

The `payments` and `orders` AppProjects must exist in the Mapping namespace.
The Mapping controller waits rather than creating a remote mapping until the
AppProject exists and the Harness Agent is connected and healthy.

Harness permits one Argo CD AppProject to map to only one Harness project. Use
different AppProjects for different targets. When
`autoCreateServiceEnv: true`, Applications need valid
`harness.io/serviceRef` and `harness.io/envRef` labels; those values become
Harness identifiers and invalid values can fail silently. Harness also
recommends waiting at least 30 seconds before re-adding a removed mapping when
the auto-create flow must run. See the
[Harness BYOA mapping documentation](https://developer.harness.io/docs/continuous-delivery/gitops/connect-and-manage/multiple-argo-to-single-harness/).

More examples:

- [PROJECT-scoped Agent](config/samples/infrastructure_v1_harnessgitopsagent.yaml)
- [Mapping resource](config/samples/infrastructure_v1_harnessgitopsprojectmapping.yaml)
- [Bootstrap values](charts/harness-gitops-agent-bootstrap/values-example.yaml)
- [ACCOUNT scope with many mappings](charts/harness-gitops-agent-bootstrap/values-account-scope-example.yaml)

## Ownership and adoption

Remote deletion is fail-closed:

| Ownership | Meaning | Remote deletion on CR deletion |
|---|---|---|
| `Managed` | The controller created the resource and verified its identity. | Yes |
| `Adopted` | A Mapping explicitly claimed an existing exact ID and the complete tuple matched. | Yes |
| `External` | The resource already existed and was not adopted. | No |

For Agents, `spec.existingAgentIdentifier` selects an existing Agent and records
it as `External`; the controller does not register it, write a token Secret for
it, or deregister it. Disable the bootstrap runtime when that Agent already runs
elsewhere. If this chart should run the existing Agent, create its valid token
Secret before installation; the controller will not create one in external
mode.

For Mappings, set `spec.adoptMappingId` only when this operator should own an
existing Harness mapping:

```yaml
spec:
  agentRef:
    name: account-agent
  appProject: payments
  orgId: platform
  projectId: payments
  adoptMappingId: existing-harness-mapping-id
```

Adoption succeeds only when that exact ID and the full remote tuple match:
account, Agent identifier, target org/project, AppProject, and
`autoCreateServiceEnv`. A wrong or missing ID fails closed, creates no
replacement, and can be corrected in place. Once `Managed` or `Adopted`
ownership is established, changing `adoptMappingId` cannot transfer ownership
to a different row.

One Harness Mapping ID can be bound to only one Mapping CR, including an
`External` observer. Before ownership is acquired or a remote row is deleted,
the controller uses an uncached, cluster-wide claim check. Conflicts are
resolved deterministically by ownership state, creation time, namespace, name,
and UID; losing CRs never gain deletion rights.

`External` is a deletion-ownership state, not an observe-only reconciliation
policy. If an external mapping disappears while the Mapping CR remains, the
controller restores the desired row and owns the verified replacement as
`Managed`.

## Create recovery

Both controllers persist create intent before calling Harness.

- Agent creation records `status.creationState: Pending` and tags the remote
  Agent with `hga_cr_uid=<Kubernetes resource UID>`. A timeout or conflict
  becomes `OutcomeUnknown`; a lost status update safely remains `Pending`.
  Either state makes the next reconcile perform an exact identifier, tuple,
  and UID-tag lookup before retrying or accepting ownership.
- Mapping creation records the complete intended tuple as `Pending`. A returned
  Mapping ID is re-listed and verified before ownership becomes `Managed`.
  Ambiguous creates do not repeat the POST. After checking Harness, set
  `adoptMappingId` to the exact observed ID to recover it as `Adopted`.

This avoids silently adopting unrelated resources and avoids deleting anything
whose provenance was not proven.

## Deletion order

Finalizers coordinate Kubernetes and Harness cleanup:

1. Deleting an Agent triggers a fresh API read for all same-namespace Mappings
   that reference it.
2. The Agent controller requests deletion of those Mappings and reports
   `Ready=False`, reason `WaitingForMappings`.
3. An `External` or never-owned Mapping removes its finalizer without Harness
   credentials or a remote read. A `Managed`, `Adopted`, or uncertain create
   performs a fresh claim check and verifies its stored remote ID and complete
   tuple before any delete. A reused ID with a different tuple is never
   deleted.
4. Only after all Mapping CRs are gone may the Agent finalizer delete a verified
   managed Agent. An external Agent is left intact.

A `Pending` Mapping with a returned ID gets a 30-second visibility grace during
finalization. If the exact row appears, ownership is recovered as `Managed`
before deletion; if it remains absent after the grace, cleanup is complete.
`Pending` without an ID and `OutcomeUnknown` remain blocked until
`adoptMappingId` identifies an exact full-tuple match. Keep the controller and
API-key Secret available until finalizers finish. This finalizer grace is
separate from Harness's 30-second Redis-cache wait before re-adding a mapping
for service/environment auto-creation.

## Secrets

The API key Secret named by `HarnessGitopsAgent.spec.apiKeySecretRef` must
contain:

```text
api_key
```

For a controller-created Agent, the controller writes the Secret named by
`spec.tokenSecretRef` in the Agent namespace with:

```text
GITOPS_AGENT_TOKEN
```

For most installations, keep API credentials in the controller namespace:

```sh
kubectl create namespace hga-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n hga-system create secret generic harness-api-key-secret \
  --from-literal=api_key="$HARNESS_API_KEY"
```

When the controller Helm chart is installed in `hga-system`, leaving
`manager.apiKeySecretNamespace` empty resolves it to `hga-system`. Every Agent
still selects a Secret by name, but the controller reads that name from the
controller release namespace. Set an explicit value only to use a different
central namespace. Token Secrets remain beside their Agent runtimes.
Centralizing the API key also keeps cleanup credentials available while a
workload namespace is being removed.

When running the controller binary directly, omitting
`--api-key-secret-namespace` retains the binary's namespaced behavior and reads
each API key Secret from its Agent namespace. Never put API keys or generated
Agent tokens in Helm values or committed manifests.

## Installation

Prerequisites:

- Kubernetes 1.25 or newer
- Helm
- a Harness API key in a Kubernetes Secret
- an accessible controller image

### 1. Install the controller

The controller chart installs the manager, RBAC, and both CRDs:

```sh
helm upgrade --install hga-controller \
  charts/harness-gitops-agent-controller \
  --namespace hga-system \
  --create-namespace \
  --set manager.apiKeySecretNamespace=hga-system \
  --wait
```

### 2. Install one bootstrap release

For a controller-created Agent, the bootstrap chart creates one Agent CR, zero
or more Mapping CRs, and the official Harness GitOps runtime. The controller
must already be running, but the Agent CR and runtime do not require two
bootstrap phases. Runtime pods may briefly wait for the controller-created
token Secret; `--wait` converges after the controller registers the Agent and
writes it.

Copy and edit the example without adding credentials:

```sh
cp charts/harness-gitops-agent-bootstrap/values-account-scope-example.yaml \
  my-agent-values.yaml

helm dependency build charts/harness-gitops-agent-bootstrap

helm upgrade --install account-agent \
  charts/harness-gitops-agent-bootstrap \
  --namespace gitops-agent \
  --create-namespace \
  --values my-agent-values.yaml \
  --wait \
  --timeout 10m
```

The API key named by `harnessAgent.spec.apiKeySecretRef` must already exist in
the controller-configured Secret namespace. Keep
`harnessAgent.spec.tokenSecretRef` equal to
`gitopsAgent.agent.existingSecrets.agentToken`.

### AppProject behavior on a clean cluster

Keep `appProject.enabled=false` on the first bootstrap install; this is the
default. Helm validates an `AppProject` before a CRD rendered by that same
release is registered, so a clean cluster cannot safely install the upstream
Argo CRD and a chart-templated AppProject in one render.

After the Argo CRD exists, either create the required AppProjects normally or
enable the chart's single convenience AppProject. Set `appProject.name` to one
of the names used by `projectMappings`; create any additional AppProjects
separately. The convenience AppProject is always created in the bootstrap
release namespace:

```sh
helm upgrade account-agent \
  charts/harness-gitops-agent-bootstrap \
  --namespace gitops-agent \
  --reuse-values \
  --set appProject.enabled=true \
  --set appProject.name=payments \
  --wait
```

If the AppProject CRD already exists before the first render,
`appProject.enabled=true` can be used on the initial install. When another Argo
CD installation owns the CRDs, set
`gitopsAgent.argo-cd.crds.install=false`.

## Observe and troubleshoot

Start with the compact resource views:

```sh
kubectl get hga,hgapm -A
kubectl -n gitops-agent describe hgapm platform-payments
kubectl -n gitops-agent get hga account-agent -o yaml
kubectl -n hga-system logs deploy/hga-controller-harness-gitops-agent-controller \
  --tail=200
```

Useful Mapping condition reasons:

| Reason | Meaning |
|---|---|
| `AgentRefNotFound`, `AgentDeleting` | The referenced same-namespace Agent is absent or terminating. |
| `AppProjectNotFound` | The named AppProject does not exist in the Mapping namespace. |
| `AgentNotFound`, `AgentNotHealthy` | Harness has not observed the Agent as connected and healthy. |
| `MappingMismatch`, `DuplicateMapping` | Existing remote rows are not an unambiguous complete-tuple match. |
| `OwnershipConflict` | Another Mapping CR is the deterministic binding for this Harness Mapping ID. |
| `AdoptionFailed` | `adoptMappingId` is absent remotely or does not match the complete tuple. |
| `CreateOutcomeUnknown` | Harness may have accepted the create; verify the remote row and adopt its exact ID. |
| `CleanupBlocked`, `CleanupFailed` | Finalization cannot yet prove or complete safe remote cleanup. |
| `VerificationFailed` | A Kubernetes or Harness read/write failed; inspect the condition message and controller log. |

Mapping recovery after an ambiguous create:

```sh
kubectl -n gitops-agent patch hgapm platform-payments \
  --type merge \
  -p '{"spec":{"adoptMappingId":"verified-harness-mapping-id"}}'
```

If an Agent identifier is occupied by a different tuple or UID tag, create a
replacement `HarnessGitopsAgent` with `spec.existingAgentIdentifier` to observe
that external Agent, or choose a new identifier for a managed Agent.

Avoid force-removing finalizers. If it is unavoidable, first verify the exact
Harness Agent/mapping state; forcing deletion explicitly accepts the risk of a
remote leak.

## Development

The main test target regenerates manifests/code, checks formatting, runs
`go vet`, starts envtest, and runs the Go and chart tests:

```sh
make test
make test-e2e-compile
make lint
```

Common development commands:

```sh
make manifests
make generate
make run
make docker-build IMG=harness-gitops-agent-operator:dev
```

Cluster E2E targets mutate the selected cluster and require explicit test
configuration:

```sh
make test-e2e
make test-e2e-remote
```

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0.
