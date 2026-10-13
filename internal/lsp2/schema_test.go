package lsp2

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// LoadSchemas compiles spec/schemas (JSON Schema 2020-12) rooted at dir.
func loadSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	dir := filepath.Join("..", "..", "spec", "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if err := c.AddResource("https://localsubproxy.dev/spec/lsp2/"+e.Name(), doc); err != nil {
			t.Fatal(err)
		}
	}
	s, err := c.Compile("https://localsubproxy.dev/spec/lsp2/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func inst(t *testing.T, b []byte) any {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSchemasAgainstVectors(t *testing.T) {
	env := loadSchema(t, "envelope.json")
	payload := loadSchema(t, "task.run.payload.json")
	b, _ := os.ReadFile(vecPath())
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, f := range v.ValidFrames {
		if err := env.Validate(inst(t, f.Frame)); err != nil {
			t.Errorf("valid frame %s rejected: %v", f.Name, err)
		}
	}
	for _, f := range v.BadFrames {
		if env.Validate(inst(t, f.Frame)) == nil {
			t.Errorf("invalid frame %s accepted", f.Name)
		}
	}
	for _, j := range v.JWS {
		if j.Kind == "task_run" {
			if err := payload.Validate(inst(t, j.Payload)); err != nil {
				t.Errorf("task.run payload: %v", err)
			}
		}
	}
}

// Frames produced by the Go reference types must satisfy the schema too.
func TestReferenceTypesMatchSchema(t *testing.T) {
	env := loadSchema(t, "envelope.json")
	mk := func(e Envelope, data any) []byte {
		if data != nil {
			e.Data, _ = json.Marshal(data)
		}
		e.V = Version
		b, _ := json.Marshal(e)
		return b
	}
	frames := [][]byte{
		mk(Envelope{Type: TypeHello, ID: "h1"}, Hello{DeviceID: "dev_1", Client: "c", Models: []string{"m"}, MaxConcurrency: 1, Capabilities: Capabilities, Proof: "a.b.c"}),
		mk(Envelope{Type: TypeWelcome, ID: "h1"}, Welcome{SessionID: "s", HeartbeatSeconds: 20, MaxFrameBytes: 1 << 20, TokenExpiresAt: 1, Capabilities: []string{}}),
		mk(Envelope{Type: TypeTaskRun, TaskID: "t", JWS: "a.b.c"}, nil),
		mk(Envelope{Type: TypeTaskDelta, TaskID: "t", Seq: 1}, TaskDelta{Text: "x"}),
		mk(Envelope{Type: TypeTaskDone, TaskID: "t", Seq: 2}, TaskDone{FinishReason: "stop", Usage: &Usage{}}),
		mk(Envelope{Type: TypeTaskError, TaskID: "t", Seq: 1}, ErrorBody{Code: ErrUpstream, Message: "m"}),
		mk(Envelope{Type: TypeSessionToken, ID: "x"}, SessionToken{AccessToken: "a.b.c", Proof: "a.b.c"}),
		mk(Envelope{Type: TypeSessionToken, ID: "x"}, SessionToken{ExpiresAt: 5}),
		mk(Envelope{Type: TypeModelsUpdate}, ModelsUpdate{Models: []string{"m"}, MaxConcurrency: 1}),
		mk(Envelope{Type: TypePing, ID: "p"}, nil),
	}
	for _, f := range frames {
		if err := env.Validate(inst(t, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
