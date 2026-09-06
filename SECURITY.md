# Security

## Reporting a vulnerability

Please do not open a public issue for a suspected vulnerability. Use GitHub's
private vulnerability reporting on this repository ("Report a vulnerability"
under the Security tab). Include the controller version, chart version, and a
minimal reproduction. You will get an acknowledgement within a few days.

## Supported versions

Only the latest released controller image and chart receive security fixes.

## Security model in brief

- The controller manages Agent and Mapping resources only in the namespaces
  listed in `manager.managedNamespaces`. Resources anywhere else are refused
  before any credential is read or any Harness API is called.
- Each Harness API key is read from a Secret in the Agent resource's own
  namespace. There is no central key namespace and no fallback between
  namespaces. The key's Harness permissions are the ceiling for everything
  the controller does in that namespace.
- The Harness API endpoint is fixed by configuration and cannot be changed
  through the pod environment.
- Agent token Secrets are written only into the Agent's namespace, must be
  owned by that exact Agent resource, and are never adopted from other owners.
- Harness API response bodies and transport errors are never written to logs
  or resource status.
- Inside an approved namespace the controller acts with that namespace's key,
  not with the identity of whoever created a resource. Use Kubernetes RBAC to
  decide who may create Agents versus Mappings; see
  `test/manifests/tenant-mapping-editor-rbac.yaml`.
