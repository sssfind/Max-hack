package vacancies

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveTrudvsemSearch is opt-in because it depends on an external public
// service. It verifies the current production response shape without making CI
// flaky: TRUDVSEM_LIVE_TEST=1 go test ./internal/vacancies -run TestLive.
func TestLiveTrudvsemSearch(t *testing.T) {
	if os.Getenv("TRUDVSEM_LIVE_TEST") != "1" {
		t.Skip("set TRUDVSEM_LIVE_TEST=1 to query Работа России")
	}

	client, err := NewClient(ClientConfig{
		RequestTimeout:    40 * time.Second,
		RequestsPerSecond: 3,
		MaxPagesPerQuery:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	items, err := client.search(ctx, "7700000000000", "программист", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("live API returned no Moscow programmer vacancies")
	}
	if items[0].ID == "" || items[0].Title == "" {
		t.Fatalf("live vacancy is missing required fields: %+v", items[0])
	}
	if items[0].URL != "" && normalizeVacancyURL(items[0].URL) == "" {
		t.Fatalf("live vacancy contains an unexpected URL: %q", items[0].URL)
	}
}
