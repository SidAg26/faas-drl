# OpenFaaS Customization & Local Development Changelog

## Overview

This document tracks all recent changes and customizations made to the OpenFaaS stack in this workspace, including `faas-netes`, `faas-provider`, and the gateway (`faas-drl`). It also documents the Go module commands and Docker/Kubernetes steps required to build and deploy these changes.

---

## 1. Codebase Changes

### faas-provider-openfaas

- **Custom logic added to proxy/proxy.go**:
  - Added/modified headers to track backend pod/service IPs (`X-OpenFaaS-Backend-IP`).
  - Improved request/response tracing for debugging and observability.

### faas-netes-openfaas

- **Switched to use local faas-provider code**:
  - Updated `go.mod` with a `replace` directive:
    ```go
    replace github.com/openfaas/faas-provider => /home/ubuntu/memFigLessDIR/faas-provider-openfaas
    ```
  - This ensures all imports of `faas-provider` use your local, modified version.

- **Go version management**:
  - Ensured `go.mod` uses a Go version supported by your toolchain (e.g., `go 1.18`).

- **Vendor update**:
  - Ran `go mod vendor` after changing `go.mod` to sync dependencies.

### faas-drl/gateway

- **Enhanced notifiers and logging**:
  - Added/modified custom notifiers for request tracing and Prometheus metrics.
  - Updated `forwarding_proxy.go` to propagate backend IP and status code in responses and logs.
  - Ensured all status codes and notifier interfaces use consistent types (string/int as needed).

- **Gateway configuration**:
  - Verified and documented the use of `functions_provider_url` for correct provider routing.
  - Noted that `127.0.0.1:8081` only works if provider and gateway are in the same container; otherwise, use the Kubernetes service DNS name.

---

## 2. Go Commands for Local Development

After making changes to any Go code or `go.mod`:

```sh
# Ensure dependencies are up to date and vendor directory is synced
go mod tidy
go mod vendor

# Build the binary (example for faas-netes)
go build
```

---

## 3. Docker & Kubernetes Steps

After building your custom binaries:

```sh
# Build Docker images for each component you changed
docker build -t <your-dockerhub-username>/faas-netes:custom .

# Push the image to your registry
docker push <your-dockerhub-username>/faas-netes:custom

# Update your Kubernetes deployment to use the new image
kubectl -n openfaas set image deployment/faas-netesd faas-netesd=<your-dockerhub-username>/faas-netes:custom

# (Repeat for gateway and faas-provider if changed)
```

If you are using KinD for local development, you can load the image directly:

```sh
kind load docker-image <your-dockerhub-username>/faas-netes:custom --name kind
```

---

## 4. Troubleshooting & Notes

- **Connection refused to 127.0.0.1:8081**:  
  Ensure your `functions_provider_url` points to the correct provider service. In Kubernetes, use the service DNS name (e.g., `http://faas-netesd.openfaas:8081/`).

- **Go version errors**:  
  The Go version in `go.mod` must match your installed Go toolchain.

- **Vendor directory errors**:  
  Always run `go mod vendor` after changing `go.mod` or adding a `replace` directive.

- **Pod IP tracking**:  
  The `X-OpenFaaS-Backend-IP` header is now set in both requests to the function pod and responses to the client/gateway for easier tracing.

---

## 5. Purpose of Changes

- **Local development and rapid iteration**:  
  Using the `replace` directive allows you to test changes in `faas-provider` without publishing to a remote repo.

- **Enhanced observability**:  
  Custom headers and notifiers make it easier to trace requests through the OpenFaaS stack and debug issues.

- **Consistent deployment**:  
  Documented build and deployment steps ensure you can reliably reproduce your environment and changes in the future.

---

## 6. Example: Quick Recap of Steps

```sh
# 1. Edit code in faas-provider-openfaas, faas-netes-openfaas, or faas-drl/gateway

# 2. Update go.mod in faas-netes-openfaas:
#    Add: replace github.com/openfaas/faas-provider => /home/ubuntu/memFigLessDIR/faas-provider-openfaas

# 3. Sync dependencies and vendor:
go mod tidy
go mod vendor

# 4. Build your binaries and Docker images
go build
docker build -t <your-image> .

# 5. Push and deploy to Kubernetes
docker push <your-image>
kubectl -n openfaas set image deployment/faas-netesd faas-netesd=<your-image>
kubectl -n openfaas rollout restart deployment faas-netesd
```

---

**Keep this file updated with each new change for a clear development history!**