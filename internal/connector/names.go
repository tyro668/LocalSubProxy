package connector

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
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
	used := map[string]bool{}
	for _, t := range taken {
		used[schemeNameKey(t)] = true
	}
	if c := withSuffix(model, ""); !used[schemeNameKey(c)] {
		return c
	}
	for n := 2; ; n++ {
		if c := withSuffix(model, fmt.Sprintf("_%d", n)); !used[schemeNameKey(c)] {
			return c
		}
	}
}

// clipBytes cuts s to at most n bytes at a UTF-8 character boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n])
}

// withSuffix appends suffix to base, cutting base so the result fits in
// MaxSchemeNameBytes: the suffix is always kept whole.
func withSuffix(base, suffix string) string {
	return clipBytes(base, MaxSchemeNameBytes-len(suffix)) + suffix
}

// checkSchemeName is the final check on a settled name, before it is persisted.
func checkSchemeName(name string) error {
	if len(name) > MaxSchemeNameBytes {
		return fmt.Errorf("scheme name is %d bytes; at most %d allowed", len(name), MaxSchemeNameBytes)
	}
	return nil
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
		return checkSchemeName(a.Name)
	}
	if err := checkSchemeName(a.Name); err != nil {
		return err
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
		// An over-long name (written by an older version) is cut to fit:
		// loading never fails because of a name.
		a.Name = clipBytes(strings.TrimSpace(a.Name), MaxSchemeNameBytes)
		if a.Name == "" && len(a.Models) > 0 {
			all := append(append([]string(nil), taken...), x.namesLocked(id)...)
			a.Name = DefaultSchemeName(a.Models[0], all)
		}
		if used[schemeNameKey(a.Name)] {
			base := a.Name
			for n := 2; ; n++ {
				c := withSuffix(base, fmt.Sprintf(" (%d)", n))
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

// MaxSchemeNameBytes is the scheme name limit (UTF-8 bytes) checked on
// every save and load.
const MaxSchemeNameBytes = 100
