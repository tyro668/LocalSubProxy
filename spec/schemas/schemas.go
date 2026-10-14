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
	once    sync.Once
	taskRun *jsonschema.Schema
	errLoad error
)

// TaskRun returns the compiled task.run.payload schema.
func TaskRun() (*jsonschema.Schema, error) {
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
		taskRun, errLoad = c.Compile(Base + "task.run.payload.json")
	})
	return taskRun, errLoad
}
