package gateway

import (
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

// Review9 P2-5: taking a concurrency slot, checking the task is not done and
// recording task.slot were separate steps. A terminal path (cancel, timeout,
// ...) landing between them saw no slot to release, and the request path then
// still dispatched task.run after the caller had been given the terminal
// error. The slot leaked (a 1-slot device stayed busy forever) and the model
// ran for nobody. Reproduced deterministically at the "slot-acquired" point.
func TestSlotHandoffTerminalRace(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	fired := make(chan string, 1)
	raceHook = func(gt *gtask, point string) {
		if point == "slot-acquired" {
			select {
			case fired <- gt.id:
				gt.failLocal(lmbp.ErrCancelled, "injected cancel in the slot handoff window", false)
			default:
			}
		}
	}
	defer func() { raceHook = nil }()
	code, id, body := termChat(t, r.srv.URL, "k-slot", "a b")
	var hooked string
	select {
	case hooked = <-fired:
	default:
		t.Fatal("hook did not fire")
	}
	if hooked != id {
		t.Fatalf("hooked %s, served %s", hooked, id)
	}
	if outcome(code, body) == "success" {
		t.Fatalf("caller got success after the task was cancelled: %d %s", code, body)
	}
	r.g.mu.Lock()
	ds := r.g.devs[r.devID]
	r.g.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		ds.mu.Lock()
		slots := ds.slots
		ds.mu.Unlock()
		if slots == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("concurrency slot leaked: %d in use after the task ended", slots)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := r.exec.count(id); n != 0 {
		t.Errorf("task.run dispatched after the terminal decision: executed %d times", n)
	}
}
