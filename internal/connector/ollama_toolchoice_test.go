package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tyro668/avora/internal/lmbp"
)

// Review7 #14: Ollama has no tool_choice; only guaranteeable semantics are
// implemented ("auto" passes tools, "none" withholds them) and the rest is
// rejected before any upstream call.
func TestOllamaToolChoice(t *testing.T) {
	var got map[string]any
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			fmt.Fprint(w, `{"models":[{"name":"llava"}]}`)
			return
		}
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
		fmt.Fprintln(w, `{"message":{"content":"ok"},"done":false}`)
		fmt.Fprintln(w, `{"message":{"content":""},"done":true,"prompt_eval_count":1,"eval_count":1}`)
	}))
	defer srv.Close()
	x := newX(t, AdapterConfig{ID: "ol", Type: "ollama_http", BaseURL: srv.URL, Models: []string{"llava"}, Capabilities: []string{"tools"}})
	exec := func(choice string) *lmbp.ErrorBody {
		r := run("ol/llava", []lmbp.Message{{Role: "user", Content: lmbp.Text("hi")}}, weather)
		if choice != "" {
			r.ToolChoice = json.RawMessage(choice)
		}
		var c collected
		_, fail := x.Execute(context.Background(), r, c.emit)
		return fail
	}
	for _, c := range []struct {
		choice string
		tools  bool
	}{{"", true}, {`"auto"`, true}, {`"none"`, false}} {
		if fail := exec(c.choice); fail != nil {
			t.Fatalf("%s: %v", c.choice, fail)
		}
		if _, has := got["tools"]; has != c.tools {
			t.Fatalf("%s: tools sent=%v want %v", c.choice, has, c.tools)
		}
	}
	before := calls.Load()
	for _, choice := range []string{`"required"`, `{"type":"function","function":{"name":"get_weather"}}`} {
		if fail := exec(choice); fail == nil || fail.Code != lmbp.ErrUnsupported {
			t.Fatalf("%s: want CAPABILITY_UNSUPPORTED, got %v", choice, fail)
		}
	}
	if calls.Load() != before {
		t.Fatal("unenforceable tool_choice still reached the model")
	}
}
