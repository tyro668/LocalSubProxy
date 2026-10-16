package connector

import (
	"path/filepath"
	"regexp"
	"strings"
)

// launchSpec is how a Codex binary is started: Path/Args for exec, and on
// Windows an optional verbatim command line (SysProcAttr.CmdLine).
type launchSpec struct {
	Path    string
	Args    []string // argv, Args[0] included
	CmdLine string   // non-empty: pass verbatim (Windows only)
}

var cmdMeta = regexp.MustCompile(`([()\][%!^"` + "`" + `<>&|;, *?])`)

// cmdEscapeArg quotes one argument for a batch file run through
// `cmd.exe /d /s /c "..."`: MSVCRT quoting first, then caret-escaping of cmd
// metacharacters — twice, because npm-style .cmd shims re-parse %* once more.
func cmdEscapeArg(arg string) string {
	var b strings.Builder
	bs := 0
	for _, r := range arg {
		switch r {
		case '\\':
			bs++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, bs*2+1))
			b.WriteRune('"')
		default:
			b.WriteString(strings.Repeat(`\`, bs))
			b.WriteRune(r)
		}
		bs = 0
	}
	b.WriteString(strings.Repeat(`\`, bs*2))
	q := `"` + b.String() + `"`
	q = cmdMeta.ReplaceAllString(q, "^$1")
	return cmdMeta.ReplaceAllString(q, "^$1")
}

// codexLaunch builds the launch for bin on goos. Windows batch shims
// (.cmd/.bat — how npm installs codex) cannot be started by CreateProcess
// with safe quoting, so they run via %ComSpec% with every argument escaped.
func codexLaunch(goos, comspec, bin string, args []string) launchSpec {
	ext := strings.ToLower(filepath.Ext(bin))
	if goos != "windows" || (ext != ".cmd" && ext != ".bat") {
		return launchSpec{Path: bin, Args: append([]string{bin}, args...)}
	}
	if strings.TrimSpace(comspec) == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}
	parts := []string{cmdMeta.ReplaceAllString(`"`+bin+`"`, "^$1")}
	for _, a := range args {
		parts = append(parts, cmdEscapeArg(a))
	}
	line := `"` + comspec + `" /d /s /c "` + strings.Join(parts, " ") + `"`
	return launchSpec{Path: comspec, Args: []string{comspec, "/d", "/s", "/c"}, CmdLine: line}
}

// windowsEnvKeys are needed by cmd.exe and by Codex itself on Windows.
var windowsEnvKeys = []string{"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "HOMEDRIVE", "HOMEPATH", "PROGRAMDATA"}

func limitedEnvironmentFor(goos string, environ []string) []string {
	allowed := map[string]bool{"HOME": true, "USER": true, "PATH": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "CODEX_HOME": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "https_proxy": true, "http_proxy": true, "no_proxy": true}
	win := map[string]bool{}
	for _, k := range windowsEnvKeys {
		win[k] = true
	}
	var env []string
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if allowed[key] || goos == "windows" && (win[strings.ToUpper(key)] || allowed[strings.ToUpper(key)]) {
			env = append(env, entry)
		}
	}
	return env
}
