package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// multiNodeFixture is the two-Spark DeepSeek-V4-Flash-Vision pair: rank 0 on
// dgx3 uses the service claim, rank 1 on dgx1 brings its own claim because
// the cluster has no ReadWriteMany class.
func multiNodeFixture() (*inferencev1alpha1.InferenceService, *inferencev1alpha1.Model) {
	tp := int32(2)
	gid := int32(3)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "ring", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:    "vllm",
			ModelRef:   "dsv4",
			Resources:  &inferencev1alpha1.InferenceResourceRequirements{GPU: 1, Memory: "110Gi", CPU: "8"},
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "shared-claim"},
			VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: &tp},
			Env:        []corev1.EnvVar{{Name: "USER_VAR", Value: "1"}},
			MultiNode: &inferencev1alpha1.MultiNodeSpec{
				RDMAResource: "rdma/rdma_shared_device_a",
				IBGIDIndex:   &gid,
				Members: []inferencev1alpha1.MultiNodeMember{
					{Node: "dgx3", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.10.4.1", SocketInterface: "enp1s0f0np0", IBHCA: "rocep1s0f0"}},
					{Node: "dgx1", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.10.4.2", SocketInterface: "enp1s0f1np1", IBHCA: "rocep1s0f1"},
						ModelCache: &inferencev1alpha1.MultiNodeMemberCache{ClaimName: "dgx1-claim"}},
				},
			},
		},
	}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "dsv4", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "s3://models/deepseek-ai/DeepSeek-V4-Flash-Vision-Exp",
			Format: "safetensors",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "cuda",
				GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Vendor: "nvidia"},
			},
		},
		// A Ready Model carries the cache key the Model controller computed;
		// without it the storage builder uses no cache volume at all.
		Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady, CacheKey: "deepseek-v4-flash-vision-exp"},
	}
	return isvc, model
}

func pvcClaim(p *corev1.Pod) string {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// buildMemberPods runs the builder on the fixture and returns head, worker
// and the group hash, failing the test on any error.
func buildMemberPods(t *testing.T) (*corev1.Pod, *corev1.Pod, string) {
	t.Helper()
	isvc, model := multiNodeFixture()
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	pods, hash, err := r.constructMemberPods(isvc, model, nil)
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if len(pods) != 2 || hash == "" {
		t.Fatalf("got %d pods, hash %q", len(pods), hash)
	}
	return pods[0], pods[1], hash
}

func TestConstructMemberPodsPlacementAndLabels(t *testing.T) {
	head, worker, hash := buildMemberPods(t)
	if head.Name != "ring-mn-0" || worker.Name != "ring-mn-1" {
		t.Errorf("names = %s, %s", head.Name, worker.Name)
	}
	if head.Spec.NodeName != "dgx3" || worker.Spec.NodeName != "dgx1" {
		t.Errorf("nodeName = %s, %s", head.Spec.NodeName, worker.Spec.NodeName)
	}
	for _, p := range []*corev1.Pod{head, worker} {
		if !p.Spec.HostNetwork || !p.Spec.HostIPC || p.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
			t.Errorf("%s: hostNetwork=%v hostIPC=%v dns=%s", p.Name, p.Spec.HostNetwork, p.Spec.HostIPC, p.Spec.DNSPolicy)
		}
		if p.Labels[LabelMultiNodeGroup] != "ring" {
			t.Errorf("%s: missing group label", p.Name)
		}
		if p.Annotations[AnnotationMultiNodeGroupHash] != hash {
			t.Errorf("%s: hash annotation %q != %q", p.Name, p.Annotations[AnnotationMultiNodeGroupHash], hash)
		}
		if p.Namespace != "default" {
			t.Errorf("%s: namespace %q", p.Name, p.Namespace)
		}
	}
	if head.Labels["inference.llmkube.dev/service"] != "ring" || head.Labels["app"] != "ring" {
		t.Errorf("head must keep the Service labels: %v", head.Labels)
	}
	if _, ok := worker.Labels["inference.llmkube.dev/service"]; ok {
		t.Errorf("worker must not carry the service label: %v", worker.Labels)
	}
	if _, ok := worker.Labels["app"]; ok {
		t.Errorf("worker must not carry the app label: %v", worker.Labels)
	}
	if head.Labels[LabelMultiNodeRank] != "0" || worker.Labels[LabelMultiNodeRank] != "1" {
		t.Errorf("rank labels: %v / %v", head.Labels, worker.Labels)
	}
}

func TestConstructMemberPodsProbesArgsEnv(t *testing.T) {
	head, worker, _ := buildMemberPods(t)
	hc, wc := head.Spec.Containers[0], worker.Spec.Containers[0]
	if hc.ReadinessProbe == nil || hc.StartupProbe == nil {
		t.Errorf("head keeps its probes")
	}
	if wc.ReadinessProbe != nil || wc.StartupProbe != nil || wc.LivenessProbe != nil || len(wc.Ports) != 0 {
		t.Errorf("worker must have no probes or ports: %+v", wc)
	}
	if !containsArg(hc.Args, "--node-rank", "0") || !containsArg(wc.Args, "--node-rank", "1") || !containsArg(wc.Args, "--master-addr", "10.10.4.1") {
		t.Errorf("rank args missing: head %v worker %v", hc.Args, wc.Args)
	}
	if !containsArg(hc.Args, "--tensor-parallel-size", "2") || !containsArg(wc.Args, "--tensor-parallel-size", "2") {
		t.Errorf("both ranks carry --tensor-parallel-size 2: head %v worker %v", hc.Args, wc.Args)
	}
	env := envMap(wc.Env)
	if env["VLLM_HOST_IP"] != "10.10.4.2" || env["NCCL_IB_HCA"] != "rocep1s0f1" || env["NCCL_IB_GID_INDEX"] != "3" || env["USER_VAR"] != "1" {
		t.Errorf("worker env = %v", env)
	}
}

func TestConstructMemberPodsResourcesAndClaims(t *testing.T) {
	head, worker, _ := buildMemberPods(t)
	wc := worker.Spec.Containers[0]
	rdma := corev1.ResourceName("rdma/rdma_shared_device_a")
	req, lim := wc.Resources.Requests[rdma], wc.Resources.Limits[rdma]
	if req.Value() != 1 || lim.Value() != 1 {
		t.Errorf("rdma resource missing: %v", wc.Resources)
	}
	capOK := false
	if wc.SecurityContext != nil && wc.SecurityContext.Capabilities != nil {
		for _, c := range wc.SecurityContext.Capabilities.Add {
			if c == "IPC_LOCK" {
				capOK = true
			}
		}
	}
	if !capOK {
		t.Errorf("IPC_LOCK missing: %+v", wc.SecurityContext)
	}
	if pvcClaim(head) != "shared-claim" || pvcClaim(worker) != "dgx1-claim" {
		t.Errorf("claims: head %q worker %q", pvcClaim(head), pvcClaim(worker))
	}
}

// TestConstructMemberPodsAutoTP: with tensorParallelSize unset the builder pins
// TP = members x GPUs into every rank's argv, because BuildArgs' own
// auto-derive only knows one pod's GPU count.
func TestConstructMemberPodsAutoTP(t *testing.T) {
	isvc, model := multiNodeFixture()
	isvc.Spec.VLLMConfig = nil
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	pods, _, err := r.constructMemberPods(isvc, model, nil)
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	for _, p := range pods {
		if !containsArg(p.Spec.Containers[0].Args, "--tensor-parallel-size", "2") {
			t.Errorf("%s: want --tensor-parallel-size 2, got %v", p.Name, p.Spec.Containers[0].Args)
		}
	}
	if isvc.Spec.VLLMConfig != nil {
		t.Errorf("the caller's spec must not be mutated")
	}
}

func TestConstructMemberPodsHashStableAndSpecSensitive(t *testing.T) {
	isvc, model := multiNodeFixture()
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	_, h1, _ := r.constructMemberPods(isvc, model, nil)
	_, h2, _ := r.constructMemberPods(isvc, model, nil)
	if h1 != h2 {
		t.Fatalf("hash not stable: %s vs %s", h1, h2)
	}
	isvc.Spec.MultiNode.Members[1].Fabric.IBHCA = "rocep1s0f0"
	_, h3, _ := r.constructMemberPods(isvc, model, nil)
	if h3 == h1 {
		t.Fatalf("hash must change when a member's fabric changes")
	}
}

func TestValidateMultiNode(t *testing.T) {
	isvc, _ := multiNodeFixture()
	if err := validateMultiNode(isvc); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	dup := *isvc
	dup.Spec.MultiNode = &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{
		{Node: "a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}}, {Node: "a"},
	}}
	if err := validateMultiNode(&dup); err == nil {
		t.Errorf("duplicate node must be rejected")
	}
	noAddr := *isvc
	noAddr.Spec.MultiNode = &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{{Node: "a"}, {Node: "b"}}}
	if err := validateMultiNode(&noAddr); err == nil {
		t.Errorf("rank 0 without an address must be rejected")
	}
	rt := *isvc
	rt.Spec.Runtime = "llamacpp"
	if err := validateMultiNode(&rt); err == nil {
		t.Errorf("llamacpp is not supported by this slice and must be rejected")
	}
	var none inferencev1alpha1.InferenceService
	if err := validateMultiNode(&none); err != nil {
		t.Errorf("no multiNode block is valid: %v", err)
	}
}

func TestGroupReadinessAndRecreate(t *testing.T) {
	mk := func(name string, phase corev1.PodPhase, ready bool, restarts int32, hash string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{AnnotationMultiNodeGroupHash: hash}}}
		p.Status.Phase = phase
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: restarts}}
		if ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		return p
	}
	now := time.Now()
	obs := []memberObservation{
		{rank: 0, existing: mk("h", corev1.PodRunning, true, 0, "abc")},
		{rank: 1, existing: mk("w", corev1.PodRunning, false, 0, "abc")},
	}
	if ok, n := groupReadiness(obs); !ok || n != 2 {
		t.Errorf("head Ready + worker Running must be ready (got %v, %d)", ok, n)
	}
	if reason, _ := groupNeedsRecreate(obs, "abc", now); reason != "" {
		t.Errorf("healthy group must not recreate: %s", reason)
	}
	obs[1].existing = mk("w", corev1.PodRunning, false, 1, "abc")
	if reason, _ := groupNeedsRecreate(obs, "abc", now); reason != "MemberRestarted" {
		t.Errorf("restarted worker: got %q", reason)
	}
	obs[1].existing = mk("w", corev1.PodRunning, false, 0, "old")
	if reason, _ := groupNeedsRecreate(obs, "abc", now); reason != "SpecChanged" {
		t.Errorf("stale hash: got %q", reason)
	}
	obs[1].existing = mk("w", corev1.PodFailed, false, 0, "abc")
	if reason, _ := groupNeedsRecreate(obs, "abc", now); reason != "MemberFailed" {
		t.Errorf("failed worker: got %q", reason)
	}
	obs[1].existing = nil
	if reason, _ := groupNeedsRecreate(obs, "abc", now); reason != "" {
		t.Errorf("a missing member is created, not a recreate trigger: got %q", reason)
	}
	if ok, n := groupReadiness(obs); ok || n != 1 {
		t.Errorf("missing member: ready=%v running=%d", ok, n)
	}
	obs[1].existing = mk("w", corev1.PodRunning, false, 0, "abc")
	obs[0].existing = mk("h", corev1.PodRunning, false, 0, "abc")
	obs[0].existing.CreationTimestamp = metav1.NewTime(now.Add(-multiNodeStartupBudget - time.Minute))
	if reason, _ := groupNeedsRecreate(obs, "abc", now); reason != "HeadNotReady" {
		t.Errorf("head past budget: got %q", reason)
	}
	obs[0].existing.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
	if reason, _ := groupNeedsRecreate(obs, "abc", now); reason != "" {
		t.Errorf("head inside budget must wait: got %q", reason)
	}
	if ok, _ := groupReadiness(obs); ok {
		t.Errorf("head not Ready must not be ready")
	}
}
