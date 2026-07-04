# Kubernetes GPU-to-CPU Fallback Mutating Webhook

This repository contains a Kubernetes **Mutating Admission Webhook** designed to automatically fall back GPU workloads to CPU execution when GPU resources in the cluster are fully exhausted, or when fallback is explicitly requested.

---

## The Challenge

In Kubernetes, GPUs are extended resources. If a Pod requests `nvidia.com/gpu` and no GPU-enabled node has sufficient capacity, the Kubernetes scheduler has no native way to fall back to CPU nodes. Instead, the Pod stays in a `Pending` state indefinitely.

## The Solution

This mutating admission webhook intercepts Pod creation requests. For Pods that opt-in, it checks the current cluster-wide GPU capacity:
1. **Total Allocatable GPUs**: Sums `nvidia.com/gpu` resource capacity from all schedulable (non-cordoned) nodes.
2. **Total Requested GPUs**: Sums `nvidia.com/gpu` resource requests/limits from all running/scheduled Pods across all namespaces.
3. **Trigger**: If `Available GPUs < Pod Requested GPUs` (or if forced by annotation), fallback is triggered.

### What Mutation Does

When fallback is triggered, the webhook applies the following mutations to the Pod spec:
* **Strips Resource Limits & Requests**: Removes `nvidia.com/gpu` from `resources.limits` and `resources.requests` in both regular and `init` containers.
* **Injects Environment Variables**: 
  - `CUDA_VISIBLE_DEVICES=""`: Masks GPUs from CUDA-aware applications (e.g., PyTorch, TensorFlow) forcing them to use CPU.
  - `GPU_FALLBACK_ACTIVE="true"`: A signal flag so the application or container entrypoint script knows it is running in fallback mode and can adjust behaviors (such as changing batch sizes or using CPU-optimized runtimes like OpenVINO/ONNX).
* **Cleans Node Selectors**: Scans and removes GPU-specific node selector keys (e.g., `nvidia.com/gpu`, `gpu`, `accelerator`, `cloud.google.com/gke-accelerator`) so the Pod can land on standard CPU nodes.
* **Prunes Node Affinities**: Strips GPU-matching requirements in `nodeAffinity` rules.
* **Adds Annotation**: Injects `gpu-fallback.example.com/fallback-triggered: "true"` for transparency and visibility.

---

## Setup & Deployment

### Prerequisites

* A Kubernetes cluster
* `kubectl` configured with administrative permissions
* `openssl` (for certificate generation)
* `go` (version 1.26 or later, if building from source)
* A container registry to push the built image

### 1. Build and Push the Webhook Image

Build the container image using the multi-stage `Dockerfile`:

```bash
# Build the image
make docker-build IMAGE_REGISTRY=your-registry

# Push the image to your registry
docker push your-registry/gpu-fallback-webhook:latest
```

> [!NOTE]
> Update the image field in `deploy/deployment.yaml` to point to your container registry image path.

### 2. Generate Certificates and Deploy

Mutating webhooks require HTTPS with valid TLS certificates. We provide a helper script that generates self-signed certificates, creates the Kubernetes secret, patches the mutating webhook configuration with the CA bundle, and applies it to the cluster:

```bash
make deploy
```

This command:
1. Creates the `gpu-fallback` namespace.
2. Generates CA certs and server certs (with SANs for `gpu-fallback-webhook.gpu-fallback.svc`).
3. Creates a secret containing the TLS credentials in the `gpu-fallback` namespace.
4. Generates `deploy/webhook-configuration-active.yaml` with the CA bundle embedded.
5. Deploys the service account, cluster RBAC roles, service, deployment, and mutating webhook configuration to your cluster.

---

## Configuration & Usage

### Opting In

By default, the webhook will **ignore** Pods unless they explicitly opt-in. To enable CPU fallback, add the label `gpu-fallback: "true"` or annotation `gpu-fallback.example.com/enabled: "true"` to your Pod:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: ml-inference
  labels:
    gpu-fallback: "true" # Opt-in label
  annotations:
    gpu-fallback.example.com/enabled: "true" # Alternately, opt-in annotation
spec:
  containers:
    - name: model-server
      image: tensorflow/serving:latest
      resources:
        limits:
          nvidia.com/gpu: "1"
```

### Forcing Fallback (Testing/Debugging)

To force CPU execution regardless of the cluster's GPU availability (useful for testing or benchmarking), add the force annotation:

```yaml
annotations:
  gpu-fallback.example.com/enabled: "true"
  gpu-fallback.example.com/force: "true" # Forces fallback to CPU
```

### Disabling Cluster Capacity Check

If you want the webhook to fallback **only** when the `gpu-fallback.example.com/force` annotation is present, or if you prefer to manage fallback policies purely via labels/annotations without querying the Kubernetes API, set `CHECK_CLUSTER_CAPACITY` environment variable in the deployment to `false` in `deploy/deployment.yaml`.

---

## Verification & Testing

Verify that the admission webhook is working using the test pod:

1. Deploy the test pod:
   ```bash
   kubectl apply -f deploy/test-pod.yaml
   ```

2. Inspect the Pod:
   ```bash
   kubectl get pod gpu-test-pod -o yaml
   ```

   If the cluster has no available GPUs, or if you set the force annotation, you will notice:
   * The `nvidia.com/gpu` request has been removed.
   * `GPU_FALLBACK_ACTIVE="true"` and `CUDA_VISIBLE_DEVICES=""` are in the container's environment variables.
   * The annotation `gpu-fallback.example.com/fallback-triggered: "true"` is present.

3. Read the logs of the test container:
   ```bash
   kubectl logs gpu-test-pod
   ```
   If fallback succeeded, it will print:
   ```
   --- GPU Fallback Verification ---
   GPU_FALLBACK_ACTIVE: 'true'
   CUDA_VISIBLE_DEVICES: ''
   Result: CPU Fallback is ACTIVE (Successful webhook mutation)
   ```

---

## Cleanup

To remove the webhook and all associated resources:

```bash
make undeploy
make clean
```
