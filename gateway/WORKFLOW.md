# OpenFaaS Gateway Request Handling & Scaling Workflow

1. **Request Arrival**
   - A client sends a request to the OpenFaaS Gateway (e.g., `/function/myfunc`).

2. **Function Extraction**
   - The gateway parses the request to determine the target function and namespace.

3. **Function Cache Check**
   - The gateway checks the in-memory function cache for the function's status (replica count, metadata).
   - **If cache hit and `AvailableReplicas > 0`:**
     - The request can be immediately forwarded to the backend (faas-netes).
   - **If cache miss or `AvailableReplicas == 0`:**
     - Proceed to live query and potential scaling.

4. **Live Query & SingleFlight**
   - The gateway uses a `SingleFlight` group to ensure only one concurrent query/scaling operation per function.
   - It queries the provider (via `ServiceQuery.GetReplicas`) for the latest replica count and status.

5. **Live Replica Check**
   - **If `AvailableReplicas > 0`:**
     - Update the cache and forward the request to the backend.
   - **If `Replicas == 0`:**
     - Proceed to scale up the function.

6. **Scale Up (if needed)**
   - Determine the minimum desired replicas (default 1, or from function metadata).
   - Use `ServiceQuery.SetReplicas` to request scaling up to the minimum.
   - Retry as needed (with backoff and polling).

7. **Wait for Readiness**
   - Poll the provider (with `GetReplicas`) until at least one replica is available or a timeout occurs.
   - Update the cache after each poll.

8. **Forward Request**
   - Once a replica is available, forward the request to the backend (faas-netes), which resolves the pod IP and serves the request.

9. **Metrics & Autoscaling**
   - Prometheus scrapes metrics from the gateway and functions.
   - If an alert rule (e.g., high RPS) triggers, Prometheus sends an alert to Alertmanager.
   - Alertmanager sends the alert to the gateway's `/system/alert` endpoint.
   - The gateway uses the scaling logic to scale the function up or down as needed.

---

## **Key Components Involved**

- **Function Cache (`function_cache.go`):**  
  Stores recent function status to avoid redundant queries.

- **Function Meta (`function_meta.go`):**  
  Holds metadata and last refresh time for each cached function.

- **Function Scaler (`function_scaler.go`):**  
  Orchestrates cache checks, live queries, scaling, and readiness polling.

- **Service Query (`service_query.go`):**  
  Abstracts querying and setting replica counts (provider-agnostic).

- **SingleFlight (`single.go`):**  
  Ensures only one scaling/query operation per function at a time.

---

## **Example Pseudocode**

```go
// On function request:
if cached, hit := functionCache.Get(fn, ns); hit && cached.AvailableReplicas > 0 {
    // Forward request
} else {
    // Use SingleFlight to avoid duplicate scaling
    res, err := scaler.Scale(fn, ns)
    if res.Available {
        // Forward request
    } else {
        // Return error or timeout
    }
}
```

---

## **Summary Table**

| Step                | What Happens                                              |
|---------------------|----------------------------------------------------------|
| Request arrives     | Gateway receives function invocation                     |
| Cache check         | Fast path if replicas available                          |
| Live query          | Query provider if cache miss or zero replicas            |
| Scale up            | If needed, scale to min replicas                         |
| Wait for readiness  | Poll until at least one replica is ready                 |
| Forward request     | Send to backend (faas-netes)                             |
| Metrics/alerts      | Prometheus/Alertmanager may trigger further scaling      |