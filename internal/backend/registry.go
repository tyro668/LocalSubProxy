package backend

import (
	"fmt"
	"sort"

	"github.com/tyro668/avora/internal/lmbp"
)

// Registry is immutable after construction and safe for concurrent lookup.
type Registry struct{ entries map[string]Definition }

func NewRegistry(defs ...Definition) *Registry {
	r := &Registry{entries: make(map[string]Definition)}
	for _, d := range defs {
		if d.Type == "" || d.Validate == nil || d.New == nil {
			panic("incomplete backend definition")
		}
		if _, ok := r.entries[d.Type]; ok {
			panic("duplicate backend: " + d.Type)
		}
		d.DefaultCapabilities = append([]string(nil), d.DefaultCapabilities...)
		d.Parameters = append([]string(nil), d.Parameters...)
		r.entries[d.Type] = d
	}
	return r
}
func (r *Registry) Lookup(typ string) (Definition, error) {
	d, ok := r.entries[typ]
	if !ok {
		return Definition{}, fmt.Errorf("unsupported adapter type %q", typ)
	}
	d.DefaultCapabilities = append([]string(nil), d.DefaultCapabilities...)
	d.Parameters = append([]string(nil), d.Parameters...)
	return d, nil
}
func (r *Registry) Definitions() []Definition {
	out := make([]Definition, 0, len(r.entries))
	for typ := range r.entries {
		d, _ := r.Lookup(typ)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}
func (r *Registry) Validate(a *Config, requireRuntime bool) error {
	a.ReasoningEffort = NormalizeEffort(a.Type, a.ReasoningEffort)
	d, err := r.Lookup(a.Type)
	if err != nil {
		return err
	}
	return d.Validate(a, requireRuntime)
}
func (r *Registry) Open(a Config, deps Dependencies) (Backend, error) {
	a.ReasoningEffort = NormalizeEffort(a.Type, a.ReasoningEffort)
	d, err := r.Lookup(a.Type)
	if err != nil {
		return nil, err
	}
	return d.New(a, deps), nil
}
func (r *Registry) Capabilities(a Config) []string {
	d, err := r.Lookup(a.Type)
	if err != nil {
		return nil
	}
	caps := append([]string(nil), d.DefaultCapabilities...)
	if len(a.Capabilities) > 0 {
		caps = append([]string(nil), a.Capabilities...)
	}
	caps = append(caps, lmbp.ModelCapParams)
	caps = append(caps, d.Parameters...)
	caps, _ = lmbp.Unique(caps)
	return caps
}
func (r *Registry) Slots(a Config) int {
	d, err := r.Lookup(a.Type)
	if err != nil {
		return 0
	}
	n := a.Concurrency
	if n <= 0 {
		n = d.DefaultConcurrency
	}
	if d.MaxConcurrency > 0 && n > d.MaxConcurrency {
		n = d.MaxConcurrency
	}
	return n
}
