/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("sanitizeDNSName", func() {
	It("should replace dots with dashes", func() {
		Expect(sanitizeDNSName("my.model.v1")).To(Equal("my-model-v1"))
	})
	It("should leave names without dots unchanged", func() {
		Expect(sanitizeDNSName("my-service")).To(Equal("my-service"))
	})
})

var _ = Describe("constructService", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("should create ClusterIP service with default port", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
		}
		svc := reconciler.constructService(isvc)

		Expect(svc.Name).To(Equal("test-svc"))
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
	})

	It("should create NodePort service", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Type: "NodePort"},
			},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
	})

	It("should set ServicePort.NodePort only when endpoint.nodePort is set on a NodePort service", func() {
		nodePort := int32(30080)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{
					Type:     "NodePort",
					NodePort: &nodePort,
				},
			},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
		Expect(svc.Spec.Ports[0].NodePort).To(Equal(nodePort))
	})

	It("should not set ServicePort.NodePort when endpoint.nodePort is nil on a NodePort service", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Type: "NodePort"},
			},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
		Expect(svc.Spec.Ports[0].NodePort).To(Equal(int32(0)))
	})

	It("should not set ServicePort.NodePort for ClusterIP service", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{
					Type:     "ClusterIP",
					NodePort: ptrInt32(30080),
				},
			},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		Expect(svc.Spec.Ports[0].NodePort).To(Equal(int32(0)))
	})

	It("should create LoadBalancer service", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Type: "LoadBalancer"},
			},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
	})

	It("should use custom port", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Port: 3000},
			},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(3000)))
	})

	It("should sanitize service name with dots", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my.model.v1", Namespace: "default"},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Name).To(Equal("my-model-v1"))
	})
})

var _ = Describe("constructEndpoint", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("should construct default endpoint URL", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
		}
		endpoint := reconciler.constructEndpoint(isvc, svc)
		Expect(endpoint).To(Equal("http://test-svc.default.svc.cluster.local:8080/v1/chat/completions"))
	})

	It("should use custom port", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Port: 9090},
			},
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
		}
		endpoint := reconciler.constructEndpoint(isvc, svc)
		Expect(endpoint).To(ContainSubstring(":9090"))
	})

	It("should use custom path", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Path: "/api/generate"},
			},
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
		}
		endpoint := reconciler.constructEndpoint(isvc, svc)
		Expect(endpoint).To(HaveSuffix("/api/generate"))
	})
})

var _ = Describe("reconcileService Metal path", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	It("should return minimal Service with correct name and namespace when isMetal is true", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-svc-test", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "some-model"},
		}

		svc, result, err := reconciler.reconcileService(context.Background(), isvc, true, 1, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeNil())
		Expect(svc).NotTo(BeNil())
		Expect(svc.Name).To(Equal("metal-svc-test"))
		Expect(svc.Namespace).To(Equal("default"))
	})

	It("should DNS-sanitize the minimal Service name when isMetal is true", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "llama-3.2-3b", Namespace: "test-ns"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "some-model"},
		}

		svc, result, err := reconciler.reconcileService(context.Background(), isvc, true, 1, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeNil())
		Expect(svc.Name).To(Equal("llama-3-2-3b"))
		Expect(svc.Namespace).To(Equal("test-ns"))
	})

	It("should not have Spec fields populated on the minimal Service", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-minimal", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "some-model",
				Endpoint: &inferencev1alpha1.EndpointSpec{Port: 9090, Type: "LoadBalancer"},
			},
		}

		svc, _, err := reconciler.reconcileService(context.Background(), isvc, true, 1, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Spec.Ports).To(BeEmpty())
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceType("")))
	})

	It("should not create any K8s Service resource when isMetal is true", func() {
		isvcName := "metal-no-k8s-svc"
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "some-model"},
		}

		_, _, err := reconciler.reconcileService(context.Background(), isvc, true, 1, true)
		Expect(err).NotTo(HaveOccurred())

		svc := &corev1.Service{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: isvcName, Namespace: "default"}, svc)
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})
})

var _ = Describe("constructEndpoint with Metal minimal Service", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("should construct correct URL from minimal Metal Service with default settings", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-test", Namespace: "default"},
		}
		minimalSvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sanitizeDNSName(isvc.Name),
				Namespace: isvc.Namespace,
			},
		}
		endpoint := reconciler.constructEndpoint(isvc, minimalSvc)
		Expect(endpoint).To(Equal("http://metal-test.default.svc.cluster.local:8080/v1/chat/completions"))
	})

	It("should construct correct URL from minimal Metal Service with custom port and path", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-custom", Namespace: "production"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{
					Port: 3000,
					Path: "/api/v2/infer",
				},
			},
		}
		minimalSvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sanitizeDNSName(isvc.Name),
				Namespace: isvc.Namespace,
			},
		}
		endpoint := reconciler.constructEndpoint(isvc, minimalSvc)
		Expect(endpoint).To(Equal("http://metal-custom.production.svc.cluster.local:3000/api/v2/infer"))
	})

	It("should construct correct URL when Metal Service name is DNS-sanitized", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "model.v2.1", Namespace: "ml"},
		}
		minimalSvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sanitizeDNSName(isvc.Name),
				Namespace: isvc.Namespace,
			},
		}
		endpoint := reconciler.constructEndpoint(isvc, minimalSvc)
		Expect(endpoint).To(Equal("http://model-v2-1.ml.svc.cluster.local:8080/v1/chat/completions"))
	})
})

var _ = Describe("reconcileService update path", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	It("should update Service type from ClusterIP to NodePort when endpoint.type changes", func() {
		ctx := context.Background()

		// Create a ready Model
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-update-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:       "https://huggingface.co/test/model.gguf",
				Format:       "gguf",
				Quantization: "Q4_K_M",
				Hardware:     &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				Resources:    &inferencev1alpha1.ResourceRequirements{CPU: "1", Memory: "1Gi"},
			},
			Status: inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())

		// Create InferenceService with ClusterIP (default)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-update-test", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "svc-update-model",
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

		// Reconcile to create the Service
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "svc-update-test", Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify Service was created with ClusterIP
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "svc-update-test", Namespace: "default"}, svc)).To(Succeed())
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))

		// Patch InferenceService to NodePort
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "svc-update-test", Namespace: "default"}, isvc)).To(Succeed())
		isvc.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Type: "NodePort"}
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		// Reconcile again
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "svc-update-test", Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify Service type changed to NodePort
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "svc-update-test", Namespace: "default"}, svc)).To(Succeed())
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
	})

	It("should update Service type from ClusterIP to LoadBalancer when endpoint.type changes", func() {
		ctx := context.Background()

		// Create a ready Model
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-update-lb-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:       "https://huggingface.co/test/model.gguf",
				Format:       "gguf",
				Quantization: "Q4_K_M",
				Hardware:     &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				Resources:    &inferencev1alpha1.ResourceRequirements{CPU: "1", Memory: "1Gi"},
			},
			Status: inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())

		// Create InferenceService with ClusterIP (default)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-update-lb-test", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "svc-update-lb-model",
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

		// Reconcile to create the Service
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "svc-update-lb-test", Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify Service was created with ClusterIP
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "svc-update-lb-test", Namespace: "default"}, svc)).To(Succeed())
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))

		// Patch InferenceService to LoadBalancer
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "svc-update-lb-test", Namespace: "default"}, isvc)).To(Succeed())
		isvc.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Type: "LoadBalancer"}
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		// Reconcile again
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "svc-update-lb-test", Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify Service type changed to LoadBalancer
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "svc-update-lb-test", Namespace: "default"}, svc)).To(Succeed())
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
	})

	It("should update Service port when endpoint.port changes", func() {
		ctx := context.Background()

		// Create a ready Model
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-update-port-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:       "https://huggingface.co/test/model.gguf",
				Format:       "gguf",
				Quantization: "Q4_K_M",
				Hardware:     &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				Resources:    &inferencev1alpha1.ResourceRequirements{CPU: "1", Memory: "1Gi"},
			},
			Status: inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())

		// Create InferenceService with default port 8080
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-update-port-test", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "svc-update-port-model",
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

		// Reconcile to create the Service
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "svc-update-port-test", Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify Service was created with port 8080
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "svc-update-port-test", Namespace: "default"}, svc)).To(Succeed())
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))

		// Patch InferenceService to use port 9090
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "svc-update-port-test", Namespace: "default"}, isvc)).To(Succeed())
		isvc.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: 9090}
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		// Reconcile again
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "svc-update-port-test", Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify Service port changed to 9090
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "svc-update-port-test", Namespace: "default"}, svc)).To(Succeed())
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(9090)))
	})
})
