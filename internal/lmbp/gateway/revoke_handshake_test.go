package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Review7 #2 (Go): a revoke that lands while a reconnecting device is being
// activated must not leave a live session, and the record stays revoked.
func TestRevokeDuringActivationLeavesNoSession(t *testing.T) {
	r := newRig(t, lmbp.Flow{TaskWindow: lmbp.DefaultTaskWindow, ConnWindow: lmbp.DefaultConnWindow}, 1, time.Millisecond)
	dev, err := r.g.cfg.Store.GetDevice(context.Background(), r.devID)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	activateHook = func(id string) {
		once.Do(func() {
			if err := r.g.RevokeDevice(context.Background(), dev.TenantID, dev.OwnerID, id); err != nil {
				t.Error(err)
			}
		})
	}
	defer func() { activateHook = nil }()
	r.g.dropDevice(r.devID, lmbp.ErrDeviceOffline, "test: force a reconnect")
	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, _ := r.g.cfg.Store.GetDevice(context.Background(), r.devID)
		if cur.Status == "revoked" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hook never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if r.g.Live(r.devID).Online {
		t.Fatal("revoked device has a live session")
	}
	if cur, _ := r.g.cfg.Store.GetDevice(context.Background(), r.devID); cur.Status != "revoked" {
		t.Fatalf("status %q", cur.Status)
	}
}
