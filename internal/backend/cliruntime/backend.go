package cliruntime

import (
	"fmt"

	"github.com/tyro668/avora/internal/backend"
)

func Validate(a *backend.Config, required bool, executable, label string) error {
	if len(a.Capabilities) > 0 {
		return fmt.Errorf("the %s adapter is text-only", label)
	}
	if a.Executable == "" {
		a.Executable = executable
	}
	path, err := Resolve(a.Executable, label)
	if err != nil {
		if required {
			return err
		}
		return nil
	}
	a.Executable = path
	return nil
}
