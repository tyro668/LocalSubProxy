//go:build windows

package control

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func shortDir(t *testing.T) string { return t.TempDir() }

func TestPipeNamePerDataDir(t *testing.T) {
	a, b := Address(`C:\Users\a\AppData\Roaming\localsubproxy`), Address(`c:\users\a\appdata\roaming\LOCALSUBPROXY\`)
	if a != b || !strings.HasPrefix(a, `\\.\pipe\localsubproxy-`) {
		t.Fatalf("pipe names %q / %q", a, b)
	}
	if Address(`C:\other`) == a {
		t.Fatal("different data dirs share a pipe")
	}
}

// The pipe DACL is protected (P: no inherited ACEs) and has exactly one ACE:
// full access for the current user. No Everyone / Authenticated Users /
// anonymous access.
func TestPipeRestrictedToCurrentUser(t *testing.T) {
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sddl := PipeSDDL(sid)
	if sddl != "D:P(A;;GA;;;"+sid+")" {
		t.Fatalf("sddl %q", sddl)
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("dacl %v err %v", dacl, err)
	}
	for _, bad := range []string{"WD", "AU", "AN", "BU"} {
		if strings.Contains(sddl, ";"+bad+")") {
			t.Fatalf("pipe grants %s", bad)
		}
	}
}
