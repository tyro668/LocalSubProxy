package cliruntime

import (
	"strings"
	"testing"
)

// uncaret undoes one cmd.exe parse pass (^x → x).
func uncaret(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '^' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// msvcrtSplit parses a command line by the MSVCRT argv rules.
func msvcrtSplit(s string) []string {
	var args []string
	var cur strings.Builder
	in, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
				} else {
					in = !in
				}
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
				i--
			}
			have = true
		case c == '"':
			in = !in
			have = true
		case (c == ' ' || c == '\t') && !in:
			if have {
				args = append(args, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		args = append(args, cur.String())
	}
	return args
}

var codexArgs = []string{"app-server", "--stdio", "-c", `web_search="disabled"`, "-c", "mcp_servers={}", "-c", `shell_environment_policy.inherit="none"`, `odd & | < > ^ % ! (x) "q" tail\`}

// Review7 #11: npm installs codex as codex.cmd on Windows; it must be run
// via cmd.exe with every argument surviving cmd's and the CRT's parsing.
func TestCodexLaunchWindowsBatch(t *testing.T) {
	for _, bin := range []string{`C:\Program Files\nodejs\codex.cmd`, `C:\x\CODEX.BAT`} {
		spec := Launch("windows", `C:\Windows\system32\cmd.exe`, bin, codexArgs)
		if spec.Path != `C:\Windows\system32\cmd.exe` || spec.CmdLine == "" {
			t.Fatalf("%s: not launched via ComSpec: %+v", bin, spec)
		}
		prefix := `"C:\Windows\system32\cmd.exe" /d /s /c "`
		if !strings.HasPrefix(spec.CmdLine, prefix) || !strings.HasSuffix(spec.CmdLine, `"`) {
			t.Fatalf("cmdline shape: %s", spec.CmdLine)
		}
		// /s strips the outer quotes; cmd parses once for the shim, once for %*
		inner := spec.CmdLine[len(prefix) : len(spec.CmdLine)-1]
		got := msvcrtSplit(uncaret(uncaret(inner)))
		want := append([]string{bin}, codexArgs...)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("args did not round-trip:\n got  %q\n want %q", got, want)
		}
	}
	if s := Launch("windows", "", `C:\x\codex.cmd`, nil); !strings.Contains(s.CmdLine, `cmd.exe`) {
		t.Fatalf("default comspec: %+v", s)
	}
}

func TestCodexLaunchDirect(t *testing.T) {
	for _, c := range []struct{ goos, bin string }{{"windows", `C:\x\codex.exe`}, {"linux", "/usr/bin/codex.cmd"}, {"darwin", "/opt/codex"}} {
		s := Launch(c.goos, "", c.bin, codexArgs)
		if s.CmdLine != "" || s.Path != c.bin || len(s.Args) != len(codexArgs)+1 || s.Args[0] != c.bin {
			t.Fatalf("%v: %+v", c, s)
		}
	}
}

func TestLimitedEnvironmentWindowsKeepsSystemVars(t *testing.T) {
	env := []string{"SystemRoot=C:\\Windows", "ComSpec=C:\\Windows\\system32\\cmd.exe", "Path=C:\\bin", "USERPROFILE=C:\\Users\\a", "SECRET=x", "PATHEXT=.EXE;.CMD"}
	got := strings.Join(EnvironmentFor("windows", env), ";")
	for _, k := range []string{"SystemRoot=", "ComSpec=", "Path=", "USERPROFILE=", "PATHEXT="} {
		if !strings.Contains(got, k) {
			t.Fatalf("windows env lost %s: %s", k, got)
		}
	}
	if strings.Contains(got, "SECRET") {
		t.Fatal("leaked unrelated env")
	}
	if l := strings.Join(EnvironmentFor("linux", env), ";"); strings.Contains(l, "SystemRoot") || strings.Contains(l, "SECRET") {
		t.Fatalf("linux env: %s", l)
	}
}
