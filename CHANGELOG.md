# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased] - 2026-10-02

### Changed (breaking)

- **API group renamed** from `infrastructure.kandylis.co.uk` to `gitops.harness.io`.
  The CRDs are now `harnessgitopsagents.gitops.harness.io` and
  `harnessgitopsprojectmappings.gitops.harness.io`. Existing custom resources
  must be recreated under the new group.
- **API version renamed** from `v1` to `v1alpha1`. Manifests now use
  `apiVersion: gitops.harness.io/v1alpha1`.
- **Go module path renamed** from
  `github.com/markoskandylis/harness-gitops-agent-operator` to
  `github.com/harness/harness-gitops-agent-operator`. Downstream importers must
  update their import paths.

### Changed

- Bumped Kubernetes libraries (`k8s.io/api`, `apimachinery`, `client-go`,
  `apiextensions-apiserver`) from `v0.34` to `v0.36`, and
  `sigs.k8s.io/controller-runtime` from `v0.22` to `v0.24` in lockstep.
  envtest now runs against Kubernetes 1.36.
- Bumped the Go toolchain to `1.26.6` (`go.mod` and the Dockerfile builder image).
- Switched the runtime container base image from `gcr.io/distroless/static:nonroot`
  to `registry.access.redhat.com/ubi9/ubi-minimal:9.8`, with a `microdnf update`
  hardening step and OCI image labels.

### Added

- `.gitignore` entries for `.DS_Store`, `.omc/`, and build output under `bin/`.
- Added `Apply` method to the `statusFailureWriter` test fake to satisfy the
  controller-runtime v0.24 `client.SubResourceWriter` interface.
