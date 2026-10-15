package deployment

import (
	"math"
	"testing"
)

func TestPreflightOverlapSaturatesOnOverflow(t *testing.T) {
	got, overflow := preflightOverlap(math.MaxInt64-1, 2)
	if !overflow || got != math.MaxInt64 {
		t.Fatalf("preflightOverlap() = (%d, %v), want saturated overflow", got, overflow)
	}

	got, overflow = preflightOverlap(128<<20, 512<<20)
	if overflow || got != 640<<20 {
		t.Fatalf("preflightOverlap() = (%d, %v), want 640 MiB without overflow", got, overflow)
	}
}
