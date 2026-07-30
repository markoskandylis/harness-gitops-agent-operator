# Manual controller test manifests

Bare custom resources for testing the **controller alone** with `kubectl
apply`—registration, token Secret, separate mappings, and finalizer cleanup—
without installing the GitOps agent runtime. To install a complete agent
instance (CRs **and** runtime), use
[`charts/harness-gitops-agent-bootstrap`](../../charts/harness-gitops-agent-bootstrap)
instead.

| File | Scope | Shows |
|---|---|---|
| `project-agent.yaml` | PROJECT | Minimal project-level agent (`projectId` required) |
| `org-agent.yaml` | ORG | Minimal org-level agent (no `projectId`) |
| `org-agent-with-mapping.yaml` | ORG | Agent plus a separate AppProject → Harness project Mapping CR |

## Flow

```sh
# 1. Namespace + API key secret (key must be "api_key")
kubectl create namespace argocd-agent --dry-run=client -o yaml | kubectl apply -f -
kubectl -n argocd-agent create secret generic harness-api-key-secret \
  --from-literal=api_key='<HARNESS_API_KEY>'

# 2. Edit the placeholders in one manifest, then apply it
kubectl apply -f test/manifests/project-agent.yaml

# 3. Verify the controller did its work
kubectl -n argocd-agent get harnessgitopsagent project-agent -o yaml   # .status.agentIdentifier set
kubectl -n argocd-agent get secret project-agent-token                 # GITOPS_AGENT_TOKEN written

# A mapping manifest also requires the named Argo CD AppProject to exist.
kubectl -n argocd-agent get harnessgitopsprojectmapping

# 4. Clean up — the finalizer deregisters the agent from Harness
kubectl delete -f test/manifests/project-agent.yaml
```

Notes:

- Each manifest is standalone with unique names/identifiers, so they can
  coexist while testing the controller. Only install a **runtime** for at most
  one agent per namespace (fixed component names).
- Use a least-privilege Harness service-account API key scoped to the target
  org/project; never commit real account identifiers or key values here.
- `config/samples/` carries the kubebuilder-conventional copy of the project
  example.
