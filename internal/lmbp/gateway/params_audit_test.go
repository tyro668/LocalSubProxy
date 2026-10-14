package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/spec/testvectors"
)

// Third audit #2: request parameters are validated against the protocol
// before anything is signed. spec/testvectors/params.json is shared by Go,
// Node, Python and the conformance tool (Z8).

type paramVectors struct {
	Base  map[string]map[string]any `json:"base"`
	Cases []paramCase               `json:"cases"`
}

type paramCase struct {
	Name      string         `json:"name"`
	Endpoint  string         `json:"endpoint"`
	Set       map[string]any `json:"set"`
	Remove    []string       `json:"remove"`
	Status    int            `json:"status"`
	Code      string         `json:"code"`
	Param     string         `json:"param"`
	Run       map[string]any `json:"run"`
	RunAbsent []string       `json:"run_absent"`
}

func loadParamVectors(t *testing.T) paramVectors {
	t.Helper()
	var v paramVectors
	if err := json.Unmarshal(testvectors.Params, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) < 80 {
		t.Fatalf("only %d param vectors", len(v.Cases))
	}
	return v
}

// body builds the request: base[endpoint] with $MODEL, then set and remove.
func (c paramCase) body(v paramVectors, model string) []byte {
	m := map[string]any{}
	for k, x := range v.Base[c.Endpoint] {
		m[k] = x
	}
	m["model"] = model
	for k, x := range c.Set {
		m[k] = x
	}
	for _, k := range c.Remove {
		delete(m, k)
	}
	b, _ := json.Marshal(m)
	return b
}

// errParam reads code and param from either error shape.
func errParam(b []byte) (code, param string) {
	var e struct {
		Error struct {
			Code      string `json:"code"`
			Param     string `json:"param"`
			LMBPCode  string `json:"lmbp_code"`
			LMBPParam string `json:"lmbp_param"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	code, param = e.Error.Code, e.Error.Param
	if code == "" {
		code, param = e.Error.LMBPCode, e.Error.LMBPParam
	}
	return
}

func TestParamVectors(t *testing.T) {
	v := loadParamVectors(t)
	r := newRig(t, lmbp.Flow{}, 0, 0)
	for _, c := range v.Cases {
		t.Run(c.Endpoint+"/"+c.Name, func(t *testing.T) {
			path := map[string]string{"chat": "/v1/chat/completions", "messages": "/v1/messages"}[c.Endpoint]
			req, _ := http.NewRequest("POST", r.srv.URL+path, bytes.NewReader(c.body(v, "echo")))
			req.Header.Set("Authorization", "Bearer "+tenantKey)
			req.Header.Set("X-End-User", "alice")
			req.Header.Set("Content-Type", "application/json")
			before := countRuns(r.exec)
			resp := doReq(t, req)
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != c.Status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, c.Status, b)
			}
			if c.Status != 200 {
				code, param := errParam(b)
				if code != c.Code || param != c.Param {
					t.Fatalf("error code %q param %q, want %q %q: %s", code, param, c.Code, c.Param, b)
				}
				if n := countRuns(r.exec); n != before {
					t.Fatalf("rejected request reached the device (%d → %d runs)", before, n)
				}
				return
			}
			id := resp.Header.Get("X-Task-Id")
			x, ok := r.exec.runs.Load(id)
			if !ok {
				t.Fatalf("no task.run for %s", id)
			}
			rb, _ := json.Marshal(x)
			var run map[string]any
			_ = json.Unmarshal(rb, &run)
			for k, want := range c.Run {
				if !reflect.DeepEqual(norm(run[k]), norm(want)) {
					t.Fatalf("task.run %s = %v, want %v (run %s)", k, run[k], want, rb)
				}
			}
			for _, k := range c.RunAbsent {
				if _, has := run[k]; has {
					t.Fatalf("task.run has %s: %s", k, rb)
				}
			}
		})
	}
}

func countRuns(f *fakeExec) int {
	n := 0
	f.runs.Range(func(_, _ any) bool { n++; return true })
	return n
}

func norm(x any) any {
	b, _ := json.Marshal(x)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

var _ = strings.Contains

// The schema backstop runs on the exact payload that would be signed: values
// the edge checks could miss are still never signed.
func TestTaskRunSchemaBackstop(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	base := lmbp.TaskRun{TaskID: "task_1", Iat: 1, Exp: 2, Nonce: "nnnnnnnnnnnnnnnnnnnn", DeviceID: "dev_1", Model: "m",
		Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hi")}}}
	if e := ValidateTaskRun(base); e != nil {
		t.Fatalf("valid payload rejected: %v", e)
	}
	for name, mut := range map[string]func(*lmbp.TaskRun){
		"temperature": func(r *lmbp.TaskRun) { r.Temperature = f(3) },
		"max_tokens":  func(r *lmbp.TaskRun) { r.MaxTokens = -1 },
		"top_p":       func(r *lmbp.TaskRun) { r.TopP = f(1.5) },
		"stop":        func(r *lmbp.TaskRun) { r.Stop = []string{"a", "b", "c", "d", "e"} },
		"messages":    func(r *lmbp.TaskRun) { r.Messages = []lmbp.Message{{Role: "robot"}} },
	} {
		r := base
		mut(&r)
		e := ValidateTaskRun(r)
		if e == nil || e.Param != name {
			t.Fatalf("%s: got %+v", name, e)
		}
	}
}
