// Package schemas embeds the LMBP JSON Schemas (this directory is the single
// source) so the reference gateway and the conformance tool validate against
// exactly the published files.
package schemas

import (
	"bytes"
	"embed"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed *.json
var FS embed.FS

// Base is the $id prefix of every schema in this directory.
const Base = "https://localsubproxy.dev/spec/lmbp/"

var (
	once     sync.Once
	compiled = map[string]*jsonschema.Schema{}
	errLoad  error
)

func load(name string) (*jsonschema.Schema, error) {
	once.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		ents, err := FS.ReadDir(".")
		if err != nil {
			errLoad = err
			return
		}
		for _, e := range ents {
			b, err := FS.ReadFile(e.Name())
			if err != nil {
				errLoad = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				errLoad = err
				return
			}
			if err := c.AddResource(Base+e.Name(), doc); err != nil {
				errLoad = err
				return
			}
		}
		for _, n := range []string{"task.run.payload.json", "hello.json", "models.update.json", "task.done.json", "task.error.json"} {
			s, err := c.Compile(Base + n)
			if err != nil {
				errLoad = err
				return
			}
			compiled[n] = s
		}
	})
	return compiled[name], errLoad
}

// TaskRun returns the compiled task.run.payload schema.
func TaskRun() (*jsonschema.Schema, error) { return load("task.run.payload.json") }

// Hello returns the compiled hello data schema (spec §7.4).
func Hello() (*jsonschema.Schema, error) { return load("hello.json") }

// ModelsUpdate returns the compiled models.update data schema (spec §7.4).
func ModelsUpdate() (*jsonschema.Schema, error) { return load("models.update.json") }

// TaskDone returns the compiled task.done data schema (device→gateway).
func TaskDone() (*jsonschema.Schema, error) { return load("task.done.json") }

// TaskError returns the compiled task.error data schema (device→gateway).
func TaskError() (*jsonschema.Schema, error) { return load("task.error.json") }
