# Manual controller test manifests

Bare custom resources for testing the **controller alone** with `kubectl
apply`: registration, token Secret, separate mappings, finalizer cleanup, and
the namespace security model. To install a complete agent instance (CRs **and**
runtime), use
[`charts/harness-gitops-agent-bootstrap`](../../charts/harness-gitops-agent-bootstrap)
instead.

| File | Scope | Shows |
|---|---|---|
| `project-agent.yaml` | PROJECT | Minimal project-level agent (`projectId` required) |
| `org-agent.yaml` | ORG | Minimal org-level agent (no `projectId`) |
| `org-agent-with-mapping.yaml` | ORG | Agent plus a separate AppProject to Harness project Mapping CR |
| `tenant-mapping-editor-rbac.yaml` | any | Role for developers: create Mappings beside a platform-owned Agent, never Agents, Secrets, or status |

## How namespaces work

- The controller reconciles Agent and Mapping CRs **only** in the namespaces
  listed in `manager.managedNamespaces` (flag `--managed-namespaces`). A CR
  anywhere else gets `NamespaceNotAllowed`, no finalizer, and no Harness call.
- The API key is read from `spec.apiKeySecretRef` **in the Agent CR's own
  namespace**. There is no central key namespace and no fallback. A listed
  namespace without a key gets `CredentialsUnavailable` and no finalizer.
- A Mapping must live in the **same namespace as its Agent**. A Mapping that
  names an Agent in another namespace gets `AgentRefNotFound`.
- The Mapping target is bounded by the Agent scope: PROJECT agents map only to
  their own project, ORG agents only to projects of their org, ACCOUNT agents
  anywhere. Violations are `ResolutionInvalid` before any Harness call.
- Inside an approved namespace the controller acts with **that namespace's
  key**, not with the identity of whoever created the CR. Who may create
  Agents versus Mappings there is therefore a Kubernetes RBAC decision; see
  `tenant-mapping-editor-rbac.yaml`.

## Flow

```sh
# 0. The controller must list the test namespace. Example install:
helm upgrade --install hga-controller charts/harness-gitops-agent-controller \
  --namespace hga-system --create-namespace \
  --set 'manager.managedNamespaces={argocd-agent}' --wait

# 1. Namespace + API key Secret IN THAT NAMESPACE (key must be "api_key")
kubectl create namespace argocd-agent --dry-run=client -o yaml | kubectl apply -f -
kubectl -n argocd-agent create secret generic harness-api-key-secret \
  --from-literal=api_key='<HARNESS_API_KEY>'

# 2. Edit the placeholders in one manifest, then apply it
kubectl apply -f test/manifests/project-agent.yaml

# 3. Verify the controller did its work
kubectl -n argocd-agent get harnessgitopsagent project-agent -o yaml   # .status.agentIdentifier set, Healthy condition
kubectl -n argocd-agent get secret project-agent-token                 # GITOPS_AGENT_TOKEN written, owned by the Agent CR

# A mapping manifest also requires the named Argo CD AppProject to exist.
kubectl -n argocd-agent get harnessgitopsprojectmapping

# 4. Clean up. The finalizer deregisters the agent from Harness using the
#    namespace key, so delete the CRs BEFORE the namespace or the key.
kubectl delete -f test/manifests/project-agent.yaml
```

## Persona checks

Each of these must end without a Harness call. Watch the controller log while
you run them; no `Registering new Harness GitOps Agent` line may appear.

```sh
# Unlisted namespace: expect Healthy=Unknown, reason NamespaceNotAllowed, no finalizer
kubectl create namespace unlisted
sed 's/namespace: argocd-agent/namespace: unlisted/' test/manifests/project-agent.yaml | kubectl apply -f -
kubectl -n unlisted get harnessgitopsagent project-agent -o jsonpath='{.metadata.finalizers} {.status.conditions[0].reason}{"\n"}'

# Listed namespace, missing key: expect CredentialsUnavailable, no finalizer
sed -e 's/name: project-agent$/name: no-key-agent/' -e 's/apiKeySecretRef: .*/apiKeySecretRef: does-not-exist/' \
  test/manifests/project-agent.yaml | kubectl apply -f -

# Wrong target project through a PROJECT agent: expect Ready=False, ResolutionInvalid
kubectl -n argocd-agent apply -f - <<'EOF'
apiVersion: infrastructure.kandylis.co.uk/v1
kind: HarnessGitopsProjectMapping
metadata: {name: wrong-project}
spec:
  agentRef: {name: project-agent}
  appProject: default
  projectId: some_other_project
EOF

# Developer RBAC: expect no / yes / no
kubectl auth can-i create harnessgitopsagents -n argocd-agent --as-group=<TEAM_GROUP> --as=anyone
kubectl auth can-i create harnessgitopsprojectmappings -n argocd-agent --as-group=<TEAM_GROUP> --as=anyone
kubectl auth can-i patch harnessgitopsprojectmappings --subresource=status -n argocd-agent --as-group=<TEAM_GROUP> --as=anyone
```

Notes:

- Each manifest is standalone with unique names/identifiers, so they can
  coexist while testing the controller. Only install a **runtime** for at most
  one agent per namespace (fixed component names).
- Use a least-privilege Harness service-account API key scoped to the target
  org/project. The key's Harness permissions are the ceiling for everything the
  controller does in that namespace; never commit real account identifiers or
  key values here.
- A Mapping stuck in `CleanupBlocked` with `creationState: OutcomeUnknown` means
  a create attempt had an uncertain outcome. Check Harness for the row and set
  `spec.adoptMappingId` to its exact ID; only when you have verified that no
  row exists should the finalizer be removed by hand.
- `config/samples/` carries the kubebuilder-conventional copy of the project
  example.
