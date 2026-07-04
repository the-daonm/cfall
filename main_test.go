package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMutatePod(t *testing.T) {
	gpuQuantity := resource.MustParse("1")
	cpuQuantity := resource.MustParse("2")

	tests := []struct {
		name                 string
		pod                  *corev1.Pod
		availableGPUs        int64
		expectedMutated      bool
		expectedGPUCleared   bool
		expectedCUDACleared  bool
		expectedTriggeredAnn bool
	}{
		{
			name: "No fallback labels or annotations - skip mutation",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cuda-container",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									"nvidia.com/gpu": gpuQuantity,
								},
							},
						},
					},
				},
			},
			availableGPUs:        0,
			expectedMutated:      false,
			expectedGPUCleared:   false,
			expectedCUDACleared:  false,
			expectedTriggeredAnn: false,
		},
		{
			name: "Fallback enabled, no GPU requests - skip mutation",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
					Labels: map[string]string{
						"gpu-fallback": "true",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cpu-container",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU: cpuQuantity,
								},
							},
						},
					},
				},
			},
			availableGPUs:        0,
			expectedMutated:      false,
			expectedGPUCleared:   false,
			expectedCUDACleared:  false,
			expectedTriggeredAnn: false,
		},
		{
			name: "Fallback enabled, GPU requested, available GPUs = 0 - mutate",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
					Labels: map[string]string{
						"gpu-fallback": "true",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cuda-container",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									"nvidia.com/gpu": gpuQuantity,
								},
							},
							Env: []corev1.EnvVar{
								{Name: "EXISTING_VAR", Value: "val"},
							},
						},
					},
				},
			},
			availableGPUs:        0,
			expectedMutated:      true,
			expectedGPUCleared:   true,
			expectedCUDACleared:  true,
			expectedTriggeredAnn: true,
		},
		{
			name: "Fallback enabled, GPU requested, available GPUs = 2 - skip mutation (adequate capacity)",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
					Labels: map[string]string{
						"gpu-fallback": "true",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cuda-container",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									"nvidia.com/gpu": gpuQuantity,
								},
							},
						},
					},
				},
			},
			availableGPUs:        2,
			expectedMutated:      false,
			expectedGPUCleared:   false,
			expectedCUDACleared:  false,
			expectedTriggeredAnn: false,
		},
		{
			name: "Fallback enabled, GPU requested, available GPUs = 2 but force fallback - mutate",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
					Labels: map[string]string{
						"gpu-fallback": "true",
					},
					Annotations: map[string]string{
						"gpu-fallback.example.com/force": "true",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cuda-container",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									"nvidia.com/gpu": gpuQuantity,
								},
							},
						},
					},
				},
			},
			availableGPUs:        2,
			expectedMutated:      true,
			expectedGPUCleared:   true,
			expectedCUDACleared:  true,
			expectedTriggeredAnn: true,
		},
		{
			name: "Fallback enabled, nodeSelector containing gpu keys - strip nodeSelector",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
					Labels: map[string]string{
						"gpu-fallback": "true",
					},
				},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"nvidia.com/gpu":      "true",
						"kubernetes.io/arch": "amd64",
					},
					Containers: []corev1.Container{
						{
							Name: "cuda-container",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									"nvidia.com/gpu": gpuQuantity,
								},
							},
						},
					},
				},
			},
			availableGPUs:        0,
			expectedMutated:      true,
			expectedGPUCleared:   true,
			expectedCUDACleared:  true,
			expectedTriggeredAnn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &WebhookServer{
				checkClusterCapacity: true,
			}
			patches, mutated := s.mutatePod(tt.pod, tt.availableGPUs)
			if mutated != tt.expectedMutated {
				t.Errorf("expected mutated = %t, got %t", tt.expectedMutated, mutated)
			}

			if !mutated {
				return
			}

			// Verify patches
			hasReplaceContainers := false
			hasReplaceNodeSelector := false
			hasAddAnnotations := false

			for _, patch := range patches {
				switch patch.Path {
				case "/spec/containers":
					hasReplaceContainers = true
					containers, ok := patch.Value.([]corev1.Container)
					if !ok {
						t.Errorf("failed to convert replacement value to container array")
						continue
					}
					for _, c := range containers {
						if tt.expectedGPUCleared {
							if _, ok := c.Resources.Limits["nvidia.com/gpu"]; ok {
								t.Errorf("nvidia.com/gpu limit was not cleared")
							}
							if _, ok := c.Resources.Requests["nvidia.com/gpu"]; ok {
								t.Errorf("nvidia.com/gpu request was not cleared")
							}
						}
						if tt.expectedCUDACleared {
							foundCUDA := false
							foundActive := false
							for _, env := range c.Env {
								if env.Name == "CUDA_VISIBLE_DEVICES" {
									foundCUDA = true
									if env.Value != "" {
										t.Errorf("expected CUDA_VISIBLE_DEVICES to be empty, got %s", env.Value)
									}
								}
								if env.Name == "GPU_FALLBACK_ACTIVE" {
									foundActive = true
									if env.Value != "true" {
										t.Errorf("expected GPU_FALLBACK_ACTIVE to be true, got %s", env.Value)
									}
								}
							}
							if !foundCUDA {
								t.Errorf("CUDA_VISIBLE_DEVICES env var not injected")
							}
							if !foundActive {
								t.Errorf("GPU_FALLBACK_ACTIVE env var not injected")
							}
						}
					}
				case "/spec/nodeSelector":
					hasReplaceNodeSelector = true
					nodeSelector, ok := patch.Value.(map[string]string)
					if !ok {
						t.Errorf("failed to convert nodeSelector replacement value to map[string]string")
						continue
					}
					if _, ok := nodeSelector["nvidia.com/gpu"]; ok {
						t.Errorf("nvidia.com/gpu node selector was not stripped")
					}
					if nodeSelector["kubernetes.io/arch"] != "amd64" {
						t.Errorf("non-gpu node selector was stripped or altered")
					}
				case "/metadata/annotations":
					hasAddAnnotations = true
					annotations, ok := patch.Value.(map[string]string)
					if !ok {
						t.Errorf("failed to convert annotations value to map[string]string")
						continue
					}
					if annotations["gpu-fallback.example.com/fallback-triggered"] != "true" {
						t.Errorf("expected fallback annotation, got %+v", annotations)
					}
				case "/metadata/annotations/gpu-fallback.example.com~1fallback-triggered":
					hasAddAnnotations = true
					val, ok := patch.Value.(string)
					if !ok {
						t.Errorf("failed to convert annotation value to string")
						continue
					}
					if val != "true" {
						t.Errorf("expected annotation value 'true', got %s", val)
					}
				}
			}

			if tt.expectedGPUCleared && !hasReplaceContainers {
				t.Errorf("expected /spec/containers patch, but got none")
			}
			if tt.pod.Spec.NodeSelector != nil && !hasReplaceNodeSelector {
				t.Errorf("expected /spec/nodeSelector patch, but got none")
			}
			if tt.expectedTriggeredAnn && !hasAddAnnotations {
				t.Errorf("expected annotation patch, but got none")
			}
		})
	}
}
