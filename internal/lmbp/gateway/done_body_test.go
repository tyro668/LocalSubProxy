package gateway

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// audit3 P2-6: a device terminal frame is validated (envelope + body against
// task.done.json / task.error.json) BEFORE its seq is accepted, persisted or
// ACKed. task.done {} used to be stored as SUCCEEDED and answered with
// finish_reason "stop".
func TestInvalidTerminalBodyIsNotAccepted(t *testing.T) {
	r := newRig(t, lmbp.Flow{TaskWindow: lmbp.DefaultTaskWindow, ConnWindow: lmbp.DefaultConnWindow}, 2, 10*time.Second)
	cases := []struct{ name, typ, data string }{
		{"done empty object", lmbp.TypeTaskDone, `{}`},
		{"done null", lmbp.TypeTaskDone, `null`},
		{"done finish_reason number", lmbp.TypeTaskDone, `{"finish_reason":5}`},
		{"done usage wrong type", lmbp.TypeTaskDone, `{"finish_reason":"stop","usage":{"prompt_tokens":"x","completion_tokens":1}}`},
		{"done array", lmbp.TypeTaskDone, `[]`},
		{"error empty object", lmbp.TypeTaskError, `{}`},
		{"error code number", lmbp.TypeTaskError, `{"code":1,"message":"m"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req := r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("slow"), "stream": true,
				"messages": []map[string]string{{"role": "user", "content": "go"}}}, nil).WithContext(ctx)
			resp, err := httpc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			id := resp.Header.Get("X-Task-Id")
			buf := make([]byte, 256)
			_, _ = io.ReadAtLeast(resp.Body, buf, 200) // the device's first delta (seq 1) arrived
			r.g.mu.Lock()
			ds := r.g.devs[r.devID]
			r.g.mu.Unlock()
			for ds.in.Recv(id) < 1 {
				time.Sleep(5 * time.Millisecond)
			}
			r.g.onDeviceData(ds, lmbp.Envelope{V: lmbp.Version, Type: c.typ, TaskID: id, Seq: 2, Data: json.RawMessage(c.data)}, 64)
			if got := ds.in.Recv(id); got >= 2 {
				t.Fatalf("invalid %s accepted: recv seq %d", c.typ, got)
			}
			rest, _ := io.ReadAll(resp.Body)
			st, err := r.g.cfg.Store.GetTask(context.Background(), id)
			if err != nil || st.State == "SUCCEEDED" || st.State == "RUNNING" {
				t.Fatalf("task state %q after invalid %s %s (err %v); stream tail %s", st.State, c.typ, c.data, err, rest)
			}
			evs, _ := r.g.cfg.Store.ListEvents(context.Background(), id)
			last := evs[len(evs)-1]
			var eb lmbp.ErrorBody
			_ = json.Unmarshal(last.Data, &eb)
			if last.Kind != lmbp.TypeTaskError || eb.Code != lmbp.ErrInternal {
				t.Fatalf("terminal event %s %s, want task.error INTERNAL", last.Kind, last.Data)
			}
		})
	}
}
