package connector

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrDuplicateSchemeName is returned when a scheme (adapter) name is already
// used by another scheme. Names compare trimmed and case-insensitively.
var ErrDuplicateSchemeName = errors.New("scheme name already exists")

func schemeNameKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// DefaultSchemeName returns the model itself when free, otherwise
// "<model>_<n>" with the smallest n >= 2 not used by any name in taken (compared like scheme names).
func DefaultSchemeName(model string, taken []string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "scheme"
	}
	if r := []rune(model); len(r) > 90 { // names are limited to 100 bytes-ish
		model = string(r[:90])
	}
	used := map[string]bool{}
	for _, t := range taken {
		used[schemeNameKey(t)] = true
	}
	if !used[schemeNameKey(model)] {
		return model
	}
	for n := 2; ; n++ {
		if c := fmt.Sprintf("%s_%d", model, n); !used[schemeNameKey(c)] {
			return c
		}
	}
}

// namesLocked lists the scheme names in use, except the adapter skip.
func (x *Connector) namesLocked(skip string) []string {
	out := make([]string, 0, len(x.adapters))
	for id, a := range x.adapters {
		if id != skip {
			out = append(out, a.Name)
		}
	}
	return out
}

// settleNameLocked trims a.Name, fills a default for an empty name and
// rejects a name another scheme already uses.
func (x *Connector) settleNameLocked(a *AdapterConfig) error {
	a.Name = strings.TrimSpace(a.Name)
	others := x.namesLocked(a.ID)
	if a.Name == "" {
		m := ""
		if len(a.Models) > 0 {
			m = a.Models[0]
		}
		a.Name = DefaultSchemeName(m, others)
		return nil
	}
	k := schemeNameKey(a.Name)
	for _, o := range others {
		if schemeNameKey(o) == k {
			return fmt.Errorf("%w: %q", ErrDuplicateSchemeName, a.Name)
		}
	}
	return nil
}

// dedupeNamesLocked makes names loaded from older configs unique in memory
// (ordered by ID, later duplicates get " (2)", " (3)", ...), and fills empty
// names. The file is rewritten on the next adapter change.
func (x *Connector) dedupeNamesLocked() {
	ids := make([]string, 0, len(x.adapters))
	for id := range x.adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	used := map[string]bool{}
	taken := []string{}
	for _, id := range ids {
		a := x.adapters[id]
		a.Name = strings.TrimSpace(a.Name)
		if a.Name == "" && len(a.Models) > 0 {
			all := append(append([]string(nil), taken...), x.namesLocked(id)...)
			a.Name = DefaultSchemeName(a.Models[0], all)
		}
		if used[schemeNameKey(a.Name)] {
			base := a.Name
			for n := 2; ; n++ {
				c := fmt.Sprintf("%s (%d)", base, n)
				if !used[schemeNameKey(c)] && !x.nameUsedByOtherLocked(id, c) {
					a.Name = c
					break
				}
			}
		}
		used[schemeNameKey(a.Name)] = true
		taken = append(taken, a.Name)
		x.adapters[id] = a
	}
}

func (x *Connector) nameUsedByOtherLocked(skip, name string) bool {
	k := schemeNameKey(name)
	for id, a := range x.adapters {
		if id != skip && schemeNameKey(a.Name) == k {
			return true
		}
	}
	return false
}
