package client_test

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
)

// dupExec reports repeated models and capability flags, as a buggy executor might.
type dupExec struct{ chunkExec }

func (dupExec) Models() []string { return []string{"m", "n", "m"} }
func (dupExec) ModelCapabilities() map[string][]string {
	return map[string][]string{"m": {"tools", "vision", "tools"}, "n": {"vision", "vision"}}
}

// capdedupe: the hello/models.update builder is the last line of defence.
// Repeated entries are dropped (order kept) so the gateway's unique-items
// schema never rejects the whole handshake.
func TestAnnounceDedupesModelsAndCapabilities(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	g := newGWCfg(t, ctx, nil)
	c, err := client.New(client.Config{Executor: dupExec{}, MaxConcurrency: 2, Logger: log.New(io.Discard, "", 0)}, g.id)
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	ready := make(chan error, 1)
	go c.Run(rctx, ready)
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("handshake rejected: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handshake never completed")
	}
	dev := g.call("GET", "/lmbp/v1/tenant/devices", nil)
	if !strings.Contains(dev, `"models":["m","n"]`) {
		t.Fatalf("models not announced once each, in order: %s", dev)
	}
	if !strings.Contains(dev, `"`+lmbp.ModelCapTools+`","`+lmbp.ModelCapVision+`"]`) || strings.Contains(dev, `"vision","vision"`) || strings.Contains(dev, `"tools","vision","tools"`) {
		t.Fatalf("capabilities not deduped in order: %s", dev)
	}
}
