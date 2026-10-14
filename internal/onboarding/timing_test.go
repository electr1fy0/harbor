package onboarding

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCheckoutTimingSurvivesFailedRepositoryAttempt(t *testing.T) {
	h := newOnboardingHarness(t, nil)
	h.manager.checkout = func(context.Context, string, string, string, string, string) (string, func(), error) {
		time.Sleep(25 * time.Millisecond)
		return "", nil, errors.New("synthetic checkout failure")
	}
	job, err := h.manager.Submit(context.Background(), deployRequest("timed", "https://github.com/example/timed", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")))
	if err != nil {
		t.Fatal(err)
	}
	result := awaitJobTerminal(t, h.manager, job.ID)
	if result.State != "FAILED" || result.CheckoutMilliseconds == nil || *result.CheckoutMilliseconds < 20 {
		t.Fatalf("checkout result=%+v", result)
	}
	persisted, err := h.manager.protected.read()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Jobs[job.ID].Job.CheckoutMilliseconds == nil {
		t.Fatal("checkout timing was not persisted")
	}
}
