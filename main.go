package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionv1.AddToScheme(runtimeScheme)
}

type PatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

type WebhookServer struct {
	clientset            *kubernetes.Clientset
	checkClusterCapacity bool
	port                 string
	certFile             string
	keyFile              string
}

func main() {
	var (
		port                 = flag.String("port", "8443", "Webhook server port.")
		certFile             = flag.String("tls-cert-file", "/etc/webhook/certs/tls.crt", "File containing the TLS client certificate.")
		keyFile              = flag.String("tls-key-file", "/etc/webhook/certs/tls.key", "File containing the TLS private key.")
		checkCapacityEnv     = os.Getenv("CHECK_CLUSTER_CAPACITY")
		checkClusterCapacity = true
	)
	flag.Parse()

	if checkCapacityEnv != "" {
		if val, err := strconv.ParseBool(checkCapacityEnv); err == nil {
			checkClusterCapacity = val
		}
	}

	log.Printf("Starting GPU Fallback Mutating Webhook...")
	log.Printf("Configuration - Port: %s, Check Capacity: %t", *port, checkClusterCapacity)

	// Create Kubernetes client
	var config *rest.Config
	var err error

	// Try in-cluster first, fallback to KUBECONFIG for local testing
	config, err = rest.InClusterConfig()
	if err != nil {
		log.Printf("Not running in-cluster, trying KUBECONFIG: %v", err)
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.Getenv("HOME") + "/.kube/config"
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Fatalf("Failed to build kubeconfig: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	server := &WebhookServer{
		clientset:            clientset,
		checkClusterCapacity: checkClusterCapacity,
		port:                 *port,
		certFile:             *certFile,
		keyFile:              *keyFile,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", server.handleMutate)
	mux.HandleFunc("/healthz", server.handleHealthz)

	srv := &http.Server{
		Addr:         ":" + *port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Printf("Listening on :%s...", *port)
	if err := srv.ListenAndServeTLS(*certFile, *keyFile); err != nil {
		log.Fatalf("Failed to start webhook server: %v", err)
	}
}

func (s *WebhookServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *WebhookServer) handleMutate(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	if len(body) == 0 {
		log.Printf("empty body received")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// Verify the content type
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Printf("Content-Type=%s, expect application/json", contentType)
		http.Error(w, "invalid content type, expect application/json", http.StatusUnsupportedMediaType)
		return
	}

	var admissionReviewReq admissionv1.AdmissionReview
	if _, _, err := deserializer.Decode(body, nil, &admissionReviewReq); err != nil {
		log.Printf("Could not decode body: %v", err)
		http.Error(w, fmt.Sprintf("could not decode body: %v", err), http.StatusBadRequest)
		return
	}

	if admissionReviewReq.Request == nil {
		log.Printf("admission review request is nil")
		http.Error(w, "admission review request is nil", http.StatusBadRequest)
		return
	}

	// Prepare response
	admissionReviewResp := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:     admissionReviewReq.Request.UID,
			Allowed: true,
		},
	}

	// Decode Pod object
	var pod corev1.Pod
	if err := json.Unmarshal(admissionReviewReq.Request.Object.Raw, &pod); err != nil {
		log.Printf("Could not unmarshal raw object to Pod: %v", err)
		s.writeResponse(w, s.errResponse(admissionReviewReq.Request.UID, fmt.Sprintf("could not unmarshal to Pod: %v", err)))
		return
	}

	log.Printf("Processing Pod %s/%s", pod.Namespace, pod.Name)

	// Check if webhook is enabled for this Pod
	// Enabled if label gpu-fallback == "true" or annotation gpu-fallback.example.com/enabled == "true"
	enabled := pod.Labels["gpu-fallback"] == "true" || pod.Annotations["gpu-fallback.example.com/enabled"] == "true"
	if !enabled {
		log.Printf("Pod %s/%s does not have gpu-fallback label or annotation enabled. Skipping.", pod.Namespace, pod.Name)
		s.writeResponse(w, &admissionReviewResp)
		return
	}

	// Calculate cluster GPU capacity if configured
	var availableGPUs int64 = 0
	if s.checkClusterCapacity {
		var err error
		availableGPUs, err = s.getAvailableGPUs()
		if err != nil {
			log.Printf("Error checking cluster GPU capacity: %v. Defaulting to 0 (forcing fallback if requested).", err)
			availableGPUs = 0
		} else {
			log.Printf("Cluster capacity calculation - Available GPUs: %d", availableGPUs)
		}
	}

	// Determine if fallback is triggered and generate patch operations
	patches, mutated := s.mutatePod(&pod, availableGPUs)
	if mutated && len(patches) > 0 {
		patchBytes, err := json.Marshal(patches)
		if err != nil {
			log.Printf("Failed to marshal patch operations: %v", err)
			s.writeResponse(w, s.errResponse(admissionReviewReq.Request.UID, fmt.Sprintf("failed to marshal patches: %v", err)))
			return
		}

		admissionReviewResp.Response.Patch = patchBytes
		patchType := admissionv1.PatchTypeJSONPatch
		admissionReviewResp.Response.PatchType = &patchType
		log.Printf("Successfully generated mutation patch for Pod %s/%s (fallback triggered)", pod.Namespace, pod.Name)
	} else {
		log.Printf("No mutation required for Pod %s/%s (adequate GPU capacity or no GPU requested)", pod.Namespace, pod.Name)
	}

	s.writeResponse(w, &admissionReviewResp)
}

func (s *WebhookServer) getAvailableGPUs() (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. List all nodes
	nodes, err := s.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to list nodes: %w", err)
	}

	var totalGPUs int64 = 0
	for _, node := range nodes.Items {
		// Skip cordoned nodes
		if node.Spec.Unschedulable {
			continue
		}

		// Sum allocatable GPUs
		if gpus, ok := node.Status.Allocatable["nvidia.com/gpu"]; ok {
			totalGPUs += gpus.Value()
		}
	}

	// 2. List all pods across all namespaces
	pods, err := s.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to list pods: %w", err)
	}

	var requestedGPUs int64 = 0
	for _, pod := range pods.Items {
		// Skip terminated pods
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		// Sum GPU requests for all containers in the pod
		for _, container := range pod.Spec.Containers {
			if gpus, ok := container.Resources.Requests["nvidia.com/gpu"]; ok {
				requestedGPUs += gpus.Value()
			} else if gpus, ok := container.Resources.Limits["nvidia.com/gpu"]; ok {
				requestedGPUs += gpus.Value()
			}
		}

		// Also check init containers
		for _, container := range pod.Spec.InitContainers {
			if gpus, ok := container.Resources.Requests["nvidia.com/gpu"]; ok {
				requestedGPUs += gpus.Value()
			} else if gpus, ok := container.Resources.Limits["nvidia.com/gpu"]; ok {
				requestedGPUs += gpus.Value()
			}
		}
	}

	availableGPUs := totalGPUs - requestedGPUs
	if availableGPUs < 0 {
		availableGPUs = 0
	}
	return availableGPUs, nil
}

func (s *WebhookServer) mutatePod(pod *corev1.Pod, availableGPUs int64) ([]PatchOperation, bool) {
	// Check if fallback is enabled for this Pod
	enabled := pod.Labels["gpu-fallback"] == "true" || pod.Annotations["gpu-fallback.example.com/enabled"] == "true"
	if !enabled {
		return nil, false
	}

	var patches []PatchOperation
	mutated := false

	// Check if fallback is forced via annotation
	forceFallback := pod.Annotations["gpu-fallback.example.com/force"] == "true"

	// Verify if Pod requests GPUs and count how many
	var podGPURequestRequirement int64 = 0
	var hasGPURequest = false

	// Check regular containers
	var sumContainersGPUs int64 = 0
	for _, c := range pod.Spec.Containers {
		var containerGPUs int64 = 0
		if val, ok := c.Resources.Requests["nvidia.com/gpu"]; ok {
			containerGPUs = val.Value()
			hasGPURequest = true
		} else if val, ok := c.Resources.Limits["nvidia.com/gpu"]; ok {
			containerGPUs = val.Value()
			hasGPURequest = true
		}
		
		// Check HAMi resource requests as well
		if _, ok := c.Resources.Requests["nvidia.com/gpumem"]; ok {
			hasGPURequest = true
		}
		if _, ok := c.Resources.Limits["nvidia.com/gpumem"]; ok {
			hasGPURequest = true
		}
		if _, ok := c.Resources.Requests["nvidia.com/gpucores"]; ok {
			hasGPURequest = true
		}
		if _, ok := c.Resources.Limits["nvidia.com/gpucores"]; ok {
			hasGPURequest = true
		}
		sumContainersGPUs += containerGPUs
	}

	// Check init containers
	var maxInitGPUs int64 = 0
	for _, c := range pod.Spec.InitContainers {
		var initGPUs int64 = 0
		if val, ok := c.Resources.Requests["nvidia.com/gpu"]; ok {
			initGPUs = val.Value()
			hasGPURequest = true
		} else if val, ok := c.Resources.Limits["nvidia.com/gpu"]; ok {
			initGPUs = val.Value()
			hasGPURequest = true
		}
		
		// Check HAMi resource requests as well
		if _, ok := c.Resources.Requests["nvidia.com/gpumem"]; ok {
			hasGPURequest = true
		}
		if _, ok := c.Resources.Limits["nvidia.com/gpumem"]; ok {
			hasGPURequest = true
		}
		if _, ok := c.Resources.Requests["nvidia.com/gpucores"]; ok {
			hasGPURequest = true
		}
		if _, ok := c.Resources.Limits["nvidia.com/gpucores"]; ok {
			hasGPURequest = true
		}
		if initGPUs > maxInitGPUs {
			maxInitGPUs = initGPUs
		}
	}

	// K8s scheduler uses max(max(init_containers), sum(containers)) for Pod scheduling requests
	podGPURequestRequirement = sumContainersGPUs
	if maxInitGPUs > podGPURequestRequirement {
		podGPURequestRequirement = maxInitGPUs
	}

	if !hasGPURequest {
		return nil, false
	}

	// Trigger fallback if forced, or if remaining GPUs in cluster are insufficient for the Pod's request
	triggerFallback := forceFallback || (availableGPUs < podGPURequestRequirement)

	if !triggerFallback {
		return nil, false
	}

	log.Printf("Fallback triggered for Pod %s/%s. Reason: Force=%t, AvailableGPUs=%d, PodRequestedGPUs=%d",
		pod.Namespace, pod.Name, forceFallback, availableGPUs, podGPURequestRequirement)

	// 1. Mutate regular containers
	mutatedContainers := make([]corev1.Container, len(pod.Spec.Containers))
	for i, c := range pod.Spec.Containers {
		mutatedContainers[i] = *c.DeepCopy()

		// Remove nvidia.com/gpu, nvidia.com/gpumem, and nvidia.com/gpucores limits/requests
		if mutatedContainers[i].Resources.Limits != nil {
			delete(mutatedContainers[i].Resources.Limits, "nvidia.com/gpu")
			delete(mutatedContainers[i].Resources.Limits, "nvidia.com/gpumem")
			delete(mutatedContainers[i].Resources.Limits, "nvidia.com/gpucores")
		}
		if mutatedContainers[i].Resources.Requests != nil {
			delete(mutatedContainers[i].Resources.Requests, "nvidia.com/gpu")
			delete(mutatedContainers[i].Resources.Requests, "nvidia.com/gpumem")
			delete(mutatedContainers[i].Resources.Requests, "nvidia.com/gpucores")
		}

		// Inject environment variables
		env := mutatedContainers[i].Env
		cudaIdx := -1
		activeIdx := -1
		for idx, e := range env {
			if e.Name == "CUDA_VISIBLE_DEVICES" {
				cudaIdx = idx
			}
			if e.Name == "GPU_FALLBACK_ACTIVE" {
				activeIdx = idx
			}
		}

		if cudaIdx >= 0 {
			env[cudaIdx].Value = ""
		} else {
			env = append(env, corev1.EnvVar{Name: "CUDA_VISIBLE_DEVICES", Value: ""})
		}

		if activeIdx >= 0 {
			env[activeIdx].Value = "true"
		} else {
			env = append(env, corev1.EnvVar{Name: "GPU_FALLBACK_ACTIVE", Value: "true"})
		}

		mutatedContainers[i].Env = env
	}

	patches = append(patches, PatchOperation{
		Op:    "replace",
		Path:  "/spec/containers",
		Value: mutatedContainers,
	})
	mutated = true

	// 2. Mutate init containers if they have GPU requests
	if len(pod.Spec.InitContainers) > 0 {
		mutatedInitContainers := make([]corev1.Container, len(pod.Spec.InitContainers))
		initMutated := false
		for i, c := range pod.Spec.InitContainers {
			mutatedInitContainers[i] = *c.DeepCopy()
			containerMutated := false

			if mutatedInitContainers[i].Resources.Limits != nil {
				for _, res := range []corev1.ResourceName{"nvidia.com/gpu", "nvidia.com/gpumem", "nvidia.com/gpucores"} {
					if _, ok := mutatedInitContainers[i].Resources.Limits[res]; ok {
						delete(mutatedInitContainers[i].Resources.Limits, res)
						containerMutated = true
						initMutated = true
					}
				}
			}
			if mutatedInitContainers[i].Resources.Requests != nil {
				for _, res := range []corev1.ResourceName{"nvidia.com/gpu", "nvidia.com/gpumem", "nvidia.com/gpucores"} {
					if _, ok := mutatedInitContainers[i].Resources.Requests[res]; ok {
						delete(mutatedInitContainers[i].Resources.Requests, res)
						containerMutated = true
						initMutated = true
					}
				}
			}

			if containerMutated {
				env := mutatedInitContainers[i].Env
				cudaIdx := -1
				activeIdx := -1
				for idx, e := range env {
					if e.Name == "CUDA_VISIBLE_DEVICES" {
						cudaIdx = idx
					}
					if e.Name == "GPU_FALLBACK_ACTIVE" {
						activeIdx = idx
					}
				}

				if cudaIdx >= 0 {
					env[cudaIdx].Value = ""
				} else {
					env = append(env, corev1.EnvVar{Name: "CUDA_VISIBLE_DEVICES", Value: ""})
				}

				if activeIdx >= 0 {
					env[activeIdx].Value = "true"
				} else {
					env = append(env, corev1.EnvVar{Name: "GPU_FALLBACK_ACTIVE", Value: "true"})
				}
				mutatedInitContainers[i].Env = env
			}
		}
		if initMutated {
			patches = append(patches, PatchOperation{
				Op:    "replace",
				Path:  "/spec/initContainers",
				Value: mutatedInitContainers,
			})
		}
	}

	// 3. Strip GPU node selectors
	gpuKeys := []string{"nvidia.com/gpu", "gpu", "accelerator", "cloud.google.com/gke-accelerator", "k8s.amazonaws.com/accelerator"}
	if pod.Spec.NodeSelector != nil {
		mutatedNodeSelector := make(map[string]string)
		for k, v := range pod.Spec.NodeSelector {
			mutatedNodeSelector[k] = v
		}

		removedKey := false
		for _, key := range gpuKeys {
			if _, ok := mutatedNodeSelector[key]; ok {
				delete(mutatedNodeSelector, key)
				removedKey = true
			}
		}

		if removedKey {
			patches = append(patches, PatchOperation{
				Op:    "replace",
				Path:  "/spec/nodeSelector",
				Value: mutatedNodeSelector,
			})
		}
	}

	// 4. Strip GPU node affinity terms
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
		nodeAffinity := pod.Spec.Affinity.NodeAffinity.DeepCopy()
		mutatedAffinity := false

		// Strip RequiredDuringSchedulingIgnoredDuringExecution terms
		if nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			var keptNodeSelectorTerms []corev1.NodeSelectorTerm
			for _, term := range nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
				var keptMatchExpressions []corev1.NodeSelectorRequirement
				for _, req := range term.MatchExpressions {
					isGPUKey := false
					for _, key := range gpuKeys {
						if req.Key == key {
							isGPUKey = true
							break
						}
					}
					if !isGPUKey {
						keptMatchExpressions = append(keptMatchExpressions, req)
					} else {
						mutatedAffinity = true
					}
				}

				var keptMatchFields []corev1.NodeSelectorRequirement
				for _, req := range term.MatchFields {
					isGPUKey := false
					for _, key := range gpuKeys {
						if req.Key == key {
							isGPUKey = true
							break
						}
					}
					if !isGPUKey {
						keptMatchFields = append(keptMatchFields, req)
					} else {
						mutatedAffinity = true
					}
				}

				// Only keep the term if it still has match expressions/fields
				if len(keptMatchExpressions) > 0 || len(keptMatchFields) > 0 {
					term.MatchExpressions = keptMatchExpressions
					term.MatchFields = keptMatchFields
					keptNodeSelectorTerms = append(keptNodeSelectorTerms, term)
				}
			}

			if mutatedAffinity {
				nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = keptNodeSelectorTerms
			}
		}

		// Strip PreferredDuringSchedulingIgnoredDuringExecution terms
		if len(nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0 {
			var keptPreferredTerms []corev1.PreferredSchedulingTerm
			for _, prefTerm := range nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
				var keptMatchExpressions []corev1.NodeSelectorRequirement
				for _, req := range prefTerm.Preference.MatchExpressions {
					isGPUKey := false
					for _, key := range gpuKeys {
						if req.Key == key {
							isGPUKey = true
							break
						}
					}
					if !isGPUKey {
						keptMatchExpressions = append(keptMatchExpressions, req)
					} else {
						mutatedAffinity = true
					}
				}

				var keptMatchFields []corev1.NodeSelectorRequirement
				for _, req := range prefTerm.Preference.MatchFields {
					isGPUKey := false
					for _, key := range gpuKeys {
						if req.Key == key {
							isGPUKey = true
							break
						}
					}
					if !isGPUKey {
						keptMatchFields = append(keptMatchFields, req)
					} else {
						mutatedAffinity = true
					}
				}

				prefTerm.Preference.MatchExpressions = keptMatchExpressions
				prefTerm.Preference.MatchFields = keptMatchFields
				keptPreferredTerms = append(keptPreferredTerms, prefTerm)
			}
			nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = keptPreferredTerms
		}

		if mutatedAffinity {
			patches = append(patches, PatchOperation{
				Op:    "replace",
				Path:  "/spec/affinity/nodeAffinity",
				Value: nodeAffinity,
			})
		}
	}

	// Remove runtimeClassName: nvidia if present to run on default CPU runtime
	if pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName == "nvidia" {
		patches = append(patches, PatchOperation{
			Op:   "remove",
			Path: "/spec/runtimeClassName",
		})
	}

	// 5. Add fallback-triggered annotation
	if pod.Annotations == nil {
		patches = append(patches, PatchOperation{
			Op:    "add",
			Path:  "/metadata/annotations",
			Value: map[string]string{"gpu-fallback.example.com/fallback-triggered": "true"},
		})
	} else {
		patches = append(patches, PatchOperation{
			Op:    "add",
			Path:  "/metadata/annotations/gpu-fallback.example.com~1fallback-triggered",
			Value: "true",
		})
	}

	return patches, mutated
}

func (s *WebhookServer) errResponse(uid types.UID, message string) *admissionv1.AdmissionReview {
	return &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:     uid,
			Allowed: false,
			Result: &metav1.Status{
				Message: message,
			},
		},
	}
}

func (s *WebhookServer) writeResponse(w http.ResponseWriter, ar *admissionv1.AdmissionReview) {
	resp, err := json.Marshal(ar)
	if err != nil {
		log.Printf("Failed to marshal admission review response: %v", err)
		http.Error(w, fmt.Sprintf("failed to marshal admission review response: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(resp); err != nil {
		log.Printf("Failed to write admission review response: %v", err)
	}
}
