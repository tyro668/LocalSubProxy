package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tyro668/avora/internal/backend"
)

// agy does not offer a per-process override for global customizations. Never
// edit the user's settings: refuse executable/inherited customizations before
// starting the CLI, while its own secure keyring remains responsible for auth.
func checkCustomizations(home string) error {
	deny := func(path string) error {
		return backend.Failure("POLICY_DENIED: Antigravity global customization must be disabled for inference: " + path)
	}
	for _, rel := range []string{".gemini/config/plugins", ".gemini/config/sidecars", ".gemini/config/rules", ".gemini/config/skills", ".gemini/config/agents"} {
		path := filepath.Join(home, filepath.FromSlash(rel))
		entries, err := os.ReadDir(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return deny(path)
		}
	}
	for _, rel := range []string{".gemini/GEMINI.md", ".gemini/AGENTS.md", ".gemini/config/skills.json", ".gemini/config/rules.json", ".gemini/config/agents.json"} {
		path := filepath.Join(home, filepath.FromSlash(rel))
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Size() > 0 {
			return deny(path)
		}
	}
	for _, rel := range []string{".gemini/config/hooks.json", ".gemini/config/mcp_config.json", ".gemini/config/config.json", ".gemini/antigravity-cli/settings.json"} {
		path := filepath.Join(home, filepath.FromSlash(rel))
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		f.Close()
		if err != nil {
			return err
		}
		if len(data) > 1<<20 {
			return deny(path)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var v any
		if err = json.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("cannot validate Antigravity customization %s: %w", path, err)
		}
		base := filepath.Base(path)
		if (base == "hooks.json" || base == "mcp_config.json") && populated(v) {
			return deny(path)
		}
		if unsafeSettings(v) {
			return deny(path)
		}
	}
	return nil
}
func populated(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case map[string]any:
		for _, v := range x {
			if populated(v) {
				return true
			}
		}
		return false
	case []any:
		return len(x) > 0
	case string:
		return strings.TrimSpace(x) != ""
	case bool:
		return x
	default:
		return true
	}
}
func unsafeSettings(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			switch strings.ToLower(k) {
			case "hooks", "plugins", "mcpservers", "mcp_servers", "sidecars", "customizations", "skillspaths", "skills_paths", "rulespaths", "rules_paths":
				if populated(v) {
					return true
				}
			}
			if unsafeSettings(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if unsafeSettings(v) {
				return true
			}
		}
	}
	return false
}
