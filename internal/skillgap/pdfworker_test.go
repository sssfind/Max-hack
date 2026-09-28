package skillgap

import (
	"strconv"
	"strings"
	"testing"
)

func TestPDFWorkerMemoryLimitFromEnvironment(t *testing.T) {
	t.Setenv(pdfWorkerMemoryEnv, strconv.FormatInt(defaultPDFMaxMemoryBytes, 10))
	got, err := pdfWorkerMemoryLimitFromEnvironment()
	if err != nil {
		t.Fatalf("pdfWorkerMemoryLimitFromEnvironment() error = %v", err)
	}
	if got != defaultPDFMaxMemoryBytes {
		t.Fatalf("pdfWorkerMemoryLimitFromEnvironment() = %d, want %d", got, defaultPDFMaxMemoryBytes)
	}

	t.Setenv(pdfWorkerMemoryEnv, "invalid")
	if _, err := pdfWorkerMemoryLimitFromEnvironment(); err == nil {
		t.Fatal("pdfWorkerMemoryLimitFromEnvironment() accepted an invalid value")
	}
}

func TestPDFWorkerEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")
	environment := pdfWorkerEnvironment(defaultPDFMaxMemoryBytes)
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "DATABASE_URL=") {
		t.Fatal("pdfWorkerEnvironment() leaked DATABASE_URL")
	}
	want := pdfWorkerMemoryEnv + "=" + strconv.FormatInt(defaultPDFMaxMemoryBytes, 10)
	if !strings.Contains(joined, want) {
		t.Fatalf("pdfWorkerEnvironment() does not contain %q", want)
	}
}

func TestPDFWorkerAddressSpaceLimit(t *testing.T) {
	const (
		current = uint64(1 << 30)
		budget  = int64(512 << 20)
	)
	tests := []struct {
		name     string
		current  uint64
		budget   int64
		existing uint64
		want     uint64
		wantErr  bool
	}{
		{name: "adds parser budget", current: current, budget: budget, existing: ^uint64(0), want: current + uint64(budget)},
		{name: "keeps stricter external limit", current: current, budget: budget, existing: current + 1, want: current + 1},
		{name: "rejects exhausted external limit", current: current, budget: budget, existing: current, wantErr: true},
		{name: "rejects overflow", current: ^uint64(0) - 1, budget: budget, existing: ^uint64(0), wantErr: true},
		{name: "rejects non-positive budget", current: current, budget: 0, existing: ^uint64(0), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := pdfWorkerAddressSpaceLimit(test.current, test.budget, test.existing)
			if (err != nil) != test.wantErr {
				t.Fatalf("pdfWorkerAddressSpaceLimit() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("pdfWorkerAddressSpaceLimit() = %d, want %d", got, test.want)
			}
		})
	}
}
