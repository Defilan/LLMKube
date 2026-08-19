package v1alpha1

import "testing"

// Resolve must carry a declared test layout through to the resolved gate, and
// a profile that declares none must keep the preset's (zero) layout. Without
// this test, deleting the overlay in Resolve leaves every other test green
// while the scope-overlap rail silently loses its layout (#1579).
func TestResolveCarriesTestLayout(t *testing.T) {
	p := &GateProfile{
		Language:   GateLanguageGeneric,
		TestLayout: TestLayout{TestRoot: "src/test/java", SourceRoot: "src/main/java"},
	}
	got := p.Resolve().TestLayout
	if got.TestRoot != "src/test/java" || got.SourceRoot != "src/main/java" {
		t.Fatalf("declared layout must survive Resolve, got %+v", got)
	}

	if l := (&GateProfile{Language: GateLanguageGo}).Resolve().TestLayout; !l.IsZero() {
		t.Fatalf("undeclared layout must resolve zero, got %+v", l)
	}
	var nilProfile *GateProfile
	if l := nilProfile.Resolve().TestLayout; !l.IsZero() {
		t.Fatalf("nil profile must resolve zero layout, got %+v", l)
	}
}
