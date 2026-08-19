package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// A model that omits the optional extra field must still yield a non-nil
// Extra map. Every verdict rail records its evidence, or its inability to
// run, into the terminal extra, and every rail nil-guards rather than
// panics, so a nil map silently disables ALL of them at once: the reviewer
// on wl-1607-live-review-nolayout confabulated a GO and published
// result.extra = [outcome, transcriptRef, turnCount] with not one rail
// marker, the exact starved-extra signature of #1570. Observability must
// not be hostage to the model's submit verbosity.
func TestSubmitResult_OmittedExtraIsInitialized(t *testing.T) {
	res, err := SubmitResultTool{}.Execute(context.Background(),
		json.RawMessage(`{"verdict":"GO","summary":"s","commit_message":"c"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Extra == nil {
		t.Fatal("Extra must be initialized when the model omits it; nil silently disables every verdict rail")
	}
}

// A model that does send extra keeps them verbatim.
func TestSubmitResult_ProvidedExtraSurvives(t *testing.T) {
	res, err := SubmitResultTool{}.Execute(context.Background(),
		json.RawMessage(`{"verdict":"NO-GO","summary":"s","commit_message":"c","extra":{"prBody":"x"}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := res.Extra["prBody"].(string); got != "x" {
		t.Fatalf("provided extra must survive, got %v", res.Extra)
	}
}
