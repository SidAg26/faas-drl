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

- **Request Routing & Round-Robin Selection**:
  - Refactored request routing logic in `pkg/k8s/proxy.go` to support round-robin selection of backend pods for each function.
  - Introduced a new `round_robin.go` module to encapsulate round-robin state and logic, ensuring thread-safe and modular selection of pods.
  - Updated `FunctionLookup` to use the new `RoundRobinSelector` for backend pod selection.
  - Ensured round-robin logic is robust to autoscaling events (pod count changes), always selecting a valid pod index.
  - Added comments and improved logging for request routing decisions (with recommendations for production log levels).

- **Request Routing Suggestions**:
  - Documented and implemented strategies for request routing, including round-robin, with placeholders for future strategies (least connections, weighted, etc.).
  - Modularized routing logic for easier extension and testing.

- **Pod Status Cache & Pod Status Marking**
  - Added a **pod status cache** to track the status (busy/idle) of function pods.
  - Implemented a `PodStatusCache` type and integrated it into `FunctionLookup` in `pkg/k8s/`.
  - Updated `proxy.go` to update the pod status cache whenever a pod is selected for a request.
  - Added new methods to `FunctionLookup` for querying and updating pod statuses.
  - Registered a new HTTP handler for `/system/podstatus/{status}` in the provider's router (see `main.go`), using `handlers.MakePodIdleHandler`.
  - Ensured the provider exposes this endpoint so the gateway can mark pods as busy or idle.

### faas-drl/gateway

- **Enhanced notifiers and logging**:
  - Added/modified custom notifiers for request tracing and Prometheus metrics.
  - Updated `forwarding_proxy.go` to propagate backend IP and status code in responses and logs.
  - Ensured all status codes and notifier interfaces use consistent types (string/int as needed).

- **Gateway configuration**:
  - Verified and documented the use of `functions_provider_url` for correct provider routing.
  - Noted that `127.0.0.1:8081` only works if provider and gateway are in the same container; otherwise, use the Kubernetes service DNS name.

- **Pod Status Cache & Pod Status Marking**
  - Added a **PodStatusPlugin** in `plugin/pod_status.go` to send pod status updates (busy/idle) to the provider.
  - The plugin sends POST requests to `/system/podstatus/{status}` with pod name and IP in the JSON body.
  - Integrated the plugin into the request forwarding logic (`handlers/forwarding_proxy.go`), so pods are marked as busy before a request and idle after completion.
  - Updated notifier and logging logic to include pod status transitions for observability.
  - Ensured the gateway is configurable to point to the correct provider URL for pod status updates.

### faas-provider-openfaas

- **Custom logic in proxy/proxy.go**:
  - Added/modified headers to track backend pod/service IPs (`X-OpenFaaS-Backend-IP`).
  - Improved request/response tracing for debugging and observability.

- **Pod Status Cache & Pod Status Marking**
  - Updated the `types` compatible with the PodStatusCahcing and PodStatusUpdater interface and methods.
  - Added a `PodStatusCache` type to track pod status (busy/idle).
  - Implemented methods for querying/updating pod statuses.
  - (If used as a base for the provider) Ensured the `serve.go` and router setup allow for custom handler registration, including `/system/podstatus/{status}`.
  - Registered a new HTTP handler for `/system/podstatus/{status}` in the provider's router (see `main.go`), using `handlers.MakePodIdleHandler`.
  - Ensured the provider exposes this endpoint so the gateway can mark pods as busy or idle.

- **Custom Deployment & Pod Version Checking**:
  - Implemented logic in `plugin/external.go` and `scaling/function_scaler.go` to support dynamic function versioning and custom deployments:
    - If a requested function version (e.g., `func-256-1`) is not found, the gateway will:
      - Search for alternative versions with available replicas (e.g., `func-128-1`, `func-512-2`).
      - If no suitable version is found, it will attempt to deploy a new function with the requested resources (memory/CPU) by cloning the base function definition and adjusting resource limits.
      - The deployment is initiated via the provider's `/system/functions` endpoint.
      - The scaling logic and cache are aware of version matches and annotate responses to indicate if a version was matched or a deployment was triggered.
    - This enables on-demand, resource-specific function deployments and fallback to available versions, improving flexibility and resource utilization.

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
For the Pod Status Caching and Pod Status Marking:

- Build and push Docker images for any changed components, and update your Kubernetes deployments.
- The provider and gateway must both be rebuilt and redeployed for pod status tracking to work end-to-end.
- The provider must expose `/system/podstatus/{status}` and update its internal pod status cache accordingly.


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

- **Round-robin pod selection**:  
  The round-robin selector is now modular and robust to autoscaling events. If the number of pods changes, the selector resets or advances as needed to always select a valid backend.

- **Request routing strategies**:  
  The codebase is now structured to allow easy addition of new routing strategies (e.g., least connections, weighted, etc.) in the future.

- **Custom deployment/version fallback**:  
  If a function version is not found, the gateway will attempt to find an alternative version or deploy a new one with the requested resources.

---

## 5. Purpose of Changes

- **Local development and rapid iteration**:  
  Using the `replace` directive allows you to test changes in `faas-provider` without publishing to a remote repo.

- **Enhanced observability**:  
  Custom headers and notifiers make it easier to trace requests through the OpenFaaS stack and debug issues.

- **Consistent deployment**:  
  Documented build and deployment steps ensure you can reliably reproduce your environment and changes in the future.

- **Improved request routing**:  
  Round-robin and modular routing logic provide fairer load distribution and a foundation for advanced routing features.

- **Pod Status Cache and Pod Status Marking**
  These changes enable the OpenFaaS gateway to track and update the status of function pods (busy/idle) in real time, improving scheduling, observability, and future scaling strategies.

- **Custom deployment and version checking**:  
  The gateway and provider now support dynamic function versioning, fallback to available versions, and on-demand deployment of new function variants with specific resource requirements.

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