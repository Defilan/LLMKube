package v1alpha1

import "testing"

func TestMultiNodeSpecRendezvousPortOrDefault(t *testing.T) {
	var nilSpec *MultiNodeSpec
	if got := nilSpec.RendezvousPortOrDefault(); got != DefaultMultiNodeRendezvousPort {
		t.Fatalf("nil spec: got %d, want %d", got, DefaultMultiNodeRendezvousPort)
	}
	unset := &MultiNodeSpec{}
	if got := unset.RendezvousPortOrDefault(); got != 29500 {
		t.Fatalf("unset: got %d, want 29500", got)
	}
	p := int32(31000)
	set := &MultiNodeSpec{RendezvousPort: &p}
	if got := set.RendezvousPortOrDefault(); got != 31000 {
		t.Fatalf("set: got %d, want 31000", got)
	}
}
