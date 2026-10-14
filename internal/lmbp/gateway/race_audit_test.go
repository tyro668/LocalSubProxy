package gateway

// Fourth audit A1: every terminal path (device done/error, timeout, cancel,
// slow consumer, local errors) must go through one decide-once step whose
// outcome drives BOTH the stored terminal state and what callers see. The
// races are reproduced deterministically with raceHook.

import (
	"context"
	"errors"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// A local terminal path (timeout / cancel / slow consumer) fires in the
// window after the device's task.done was committed but before it was
// published. The caller, the store and a replay after restart must agree.
func TestTerminalDecideOnceRace(t *testing.T) {
	for _, c := range []struct{ name, code string }{
		{"timeout", lmbp.ErrTimeout}, {"client cancel", lmbp.ErrCancelled}, {"slow consumer", lmbp.ErrSlowConsumer},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, lmbp.Flow{}, 0, 0)
			fired := false
			raceHook = func(gt *gtask, point string) {
				if point == "device-terminal-committed" && !fired {
					fired = true
					gt.failLocal(c.code, "injected "+c.name, false) // the racing path, deterministically in the window
				}
			}
			defer func() { raceHook = nil }()
			code, id, body := termChat(t, r.srv.URL, "k-race-"+c.code, "a b")
			if !fired {
				t.Fatal("hook did not fire")
			}
			caller := outcome(code, body)
			time.Sleep(200 * time.Millisecond)
			kind := checkSingleTerminal(t, r.g.cfg.Store, id)
			stored := map[string]string{lmbp.TypeTaskDone: "success", lmbp.TypeTaskError: "error"}[kind]
			if (caller == "success") != (stored == "success") {
				t.Fatalf("caller got %s (%d %q) but storage holds %s", caller, code, body, kind)
			}
			// Restart: replay from storage must equal what the caller saw.
			st2, err := store.OpenSQLite(r.dbPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer st2.Close()
			g2, err := New(Config{Store: st2, Logger: log.New(r.logs, "", 0), AttachGrace: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			_ = g2.RegisterTenant(context.Background(), "acme", "Acme", tenantKey)
			srv2 := httptest.NewServer(g2.Handler())
			defer srv2.Close()
			code2, _, body2 := termChat(t, srv2.URL, "k-race-"+c.code, "a b")
			if (outcome(code2, body2) == "success") != (caller == "success") {
				t.Fatalf("caller got %s but the replay after restart got %d %q", caller, code2, body2)
			}
		})
	}
}

// The reverse order: a local path decides (and commits) first, while the
// device keeps streaming and then sends task.done. The late device frames
// must be dropped; caller, storage and replay all report the local error.
func TestTerminalLocalDecidesFirst(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	fired := false
	raceHook = func(gt *gtask, point string) {
		if point == "device-frame" && !fired {
			fired = true
			gt.failLocal(lmbp.ErrTimeout, "injected timeout", false)
		}
	}
	defer func() { raceHook = nil }()
	code, id, body := termChat(t, r.srv.URL, "k-local-first", "a b c d")
	if !fired || outcome(code, body) == "success" || !strings.Contains(body, lmbp.ErrTimeout) {
		t.Fatalf("caller: %d %s (hook fired %v)", code, body, fired)
	}
	time.Sleep(300 * time.Millisecond) // let the device's late frames arrive and be dropped
	if k := checkSingleTerminal(t, r.g.cfg.Store, id); k != lmbp.TypeTaskError {
		t.Fatalf("stored %s, want the local task.error", k)
	}
	evs, _ := r.g.cfg.Store.ListEvents(context.Background(), id)
	if evs[len(evs)-1].Kind != lmbp.TypeTaskError {
		t.Fatalf("a device frame was stored after the decided terminal event: %+v", evs)
	}
}

// failCreate fails the task creation (before the fix the idempotency record
// was a separate, earlier write, so this was the crash window between the
// two; now both are one CreateTask transaction).
type failCreate struct {
	store.Store
	fail bool
}

func (f *failCreate) CreateTask(ctx context.Context, t store.Task, i *store.Idempotency) error {
	if f.fail {
		f.fail = false
		return errors.New("injected crash while creating the task")
	}
	return f.Store.CreateTask(ctx, t, i)
}

// A key can never exist without its task: after a failed create, a retry
// with the same Idempotency-Key runs the request instead of 404-ing forever.
func TestIdempotencyAndTaskAtomic(t *testing.T) {
	var fc *failCreate
	r := newRigWith(t, lmbp.Flow{}, 0, 0, func(s store.Store) store.Store { fc = &failCreate{Store: s, fail: true}; return fc })
	code, _, body := termChat(t, r.srv.URL, "k-atomic", "a b")
	if code != 500 {
		t.Fatalf("first call: %d %s", code, body)
	}
	if rec, err := r.g.cfg.Store.GetIdempotency(context.Background(), "acme", "alice", "k-atomic"); err == nil {
		if _, err := r.g.cfg.Store.GetTask(context.Background(), rec.TaskID); err != nil {
			t.Errorf("idempotency key stored without its task (%s): %v", rec.TaskID, err)
		}
	}
	code, _, body = termChat(t, r.srv.URL, "k-atomic", "a b")
	if outcome(code, body) != "success" {
		t.Fatalf("retry with the same key after the failed create: %d %s", code, body)
	}
}
