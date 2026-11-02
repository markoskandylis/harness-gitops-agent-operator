# Build the manager binary
FROM golang:1.26.6 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

FROM registry.access.redhat.com/ubi9/ubi-minimal:9.8

USER root

# Upgrade dependencies
RUN microdnf update --nodocs --setopt=install_weak_deps=0 -y && \
    microdnf clean all && \
    rm -rf /tmp/* /var/tmp/*

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ARG VERSION

LABEL name="harness/harness-gitops-agent-operator"
LABEL vendor="Harness"
LABEL maintainer="Harness"
LABEL version=${VERSION}
LABEL release=${VERSION}
LABEL summary="Harness GitOps Agent Operator manages Harness GitOps agents and Project Mapping"
LABEL description="The Harness GitOps Agent Operator automates the registration, deployment, and lifecycle management of Harness GitOps agents and the project mappings in Kubernetes environments."

ENTRYPOINT ["/manager"]
