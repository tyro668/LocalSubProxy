package backend

import (
	"fmt"
	"strings"

	"github.com/tyro668/avora/internal/lmbp"
)

// Registry is immutable after construction and safe for concurrent lookup.
type Registry struct {
	entries map[string]Definition
	order   []string
}

func NewRegistry(defs ...Definition) *Registry {
	r := &Registry{entries: make(map[string]Definition)}
	for _, d := range defs {
		if d.Type == "" || d.Validate == nil || d.New == nil {
			panic("incomplete backend definition")
		}
		if _, ok := r.entries[d.Type]; ok {
			panic("duplicate backend: " + d.Type)
		}
		r.entries[d.Type] = d.clone()
		r.order = append(r.order, d.Type)
	}
	return r
}
func (r *Registry) Lookup(typ string) (Definition, error) {
	d, ok := r.entries[typ]
	if !ok {
		return Definition{}, fmt.Errorf("unsupported adapter type %q", typ)
	}
	return d.clone(), nil
}

// Definitions are in registration order.
func (r *Registry) Definitions() []Definition {
	out := make([]Definition, 0, len(r.order))
	for _, typ := range r.order {
		d, _ := r.Lookup(typ)
		out = append(out, d)
	}
	return out
}
func (r *Registry) Validate(a *Config, requireRuntime bool) error {
	d, err := r.Lookup(a.Type)
	if err != nil {
		return err
	}
	if !d.AllowsEffort(a.ReasoningEffort) {
		return fmt.Errorf("reasoning effort %q is not available for %s (allowed: default, %s)", a.ReasoningEffort, d.Label, strings.Join(d.Efforts, ", "))
	}
	a.ReasoningEffort = d.Effort(a.ReasoningEffort)
	return d.Validate(a, requireRuntime)
}

// Find resolves a stored type or a CLI name.
func (r *Registry) Find(name string) (Definition, bool) {
	for _, d := range r.Definitions() {
		if d.Type == name {
			return d, true
		}
		for _, n := range d.Names() {
			if n == name {
				return d, true
			}
		}
	}
	return Definition{}, false
}
func (r *Registry) Open(a Config, deps Dependencies) (Backend, error) {
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

func (d Definition) clone() Definition {
	d.DefaultCapabilities = append([]string(nil), d.DefaultCapabilities...)
	d.Parameters = append([]string(nil), d.Parameters...)
	d.Aliases = append([]string(nil), d.Aliases...)
	d.Efforts = append([]string(nil), d.Efforts...)
	aliases := make(map[string]string, len(d.EffortAliases))
	for k, v := range d.EffortAliases {
		aliases[k] = v
	}
	d.EffortAliases = aliases
	return d
}
