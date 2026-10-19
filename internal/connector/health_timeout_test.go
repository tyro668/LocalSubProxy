package connector

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSlowCLIRemainsAvailableAfterManualTest(t *testing.T) {
	// The official CLI's global configuration is unrelated to this fixture.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	oldInterval, oldKick, oldRound := healthInterval, healthKick, healthRound
	kick, rounds := make(chan struct{}), make(chan struct{}, 4)
	healthInterval, healthKick, healthRound = time.Hour, kick, func() { rounds <- struct{}{} }
	defer func() { healthInterval, healthKick, healthRound = oldInterval, oldKick, oldRound }()

	a := AdapterConfig{ID: "agy", Type: "antigravity_cli", Executable: os.Args[0], Models: []string{"gemini-3.8-flash-high"}, Enabled: true}
	x, err := New(Config{Adapters: []AdapterConfig{a}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer func() { cancel(); x.Shutdown() }()
	h, err := x.TestAdapter(ctx, a)
	if err != nil || h.Health != "ready" {
		t.Fatalf("manual test: %+v %v", h, err)
	}

	// The fake agy models command takes six seconds. The old periodic
	// five-second deadline changed this successful scheme to unavailable.
	x.Start(ctx)
	for round := 0; round < 2; round++ {
		if round > 0 {
			select {
			case kick <- struct{}{}:
			case <-ctx.Done():
				t.Fatal("health loop did not accept the next round")
			}
		}
		select {
		case <-rounds:
		case <-ctx.Done():
			t.Fatal("background health check did not finish")
		}
		view := x.ListAdapters()[0]
		if view.Health != "ready" || view.HealthError != "" {
			t.Fatalf("background round %d replaced success: %+v", round, view)
		}
		if models := x.Models(); len(models) != 1 || models[0] != "agy/gemini-3.8-flash-high" {
			t.Fatalf("healthy model withdrawn: %v", models)
		}
	}

	// A shorter caller deadline still wins; no cached success masks failure.
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	h, err = x.TestAdapter(short, a)
	if err != nil || h.Health != "unavailable" || !strings.Contains(h.Error, "context deadline exceeded") {
		t.Fatalf("caller deadline was ignored: %+v %v", h, err)
	}
	if strings.Contains(h.Error, "sign in") {
		t.Fatalf("timeout incorrectly diagnosed as an authentication problem: %s", h.Error)
	}
}
