package skillgap

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveFederalDocument is opt-in so ordinary tests never depend on the
// availability of the federal document host.
func TestLiveFederalDocument(t *testing.T) {
	sourceURL := os.Getenv("SKILLGAP_LIVE_DOCUMENT_URL")
	if sourceURL == "" {
		t.Skip("set SKILLGAP_LIVE_DOCUMENT_URL to run the live document smoke test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := New(Options{}).Analyze(ctx, Request{
		Primary: DocumentRef{URL: sourceURL, Type: DocumentFGOS},
		Skills:  []MarketSkill{{Name: "профессиональная деятельность"}},
	})
	if err != nil {
		t.Fatalf("Analyze() live error = %v", err)
	}
	if result.Source.SHA256 == "" || len(result.Matches) != 1 {
		t.Fatalf("incomplete live result = %+v", result)
	}
}
