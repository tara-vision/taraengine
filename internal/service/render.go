package service

import (
	"bytes"
	"embed"
	"encoding/xml"
	"fmt"
	"regexp"
	"text/template"
)

//go:embed templates
var templateFiles embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{"xml": xmlText}).
	ParseFS(templateFiles, "templates/*.tmpl"))

// unitSafePath is a binary path systemd reads as written: no space, quote, backslash or %, which a unit file would
// split, unquote, unescape or expand.
var unitSafePath = regexp.MustCompile(`^/[A-Za-z0-9/._+-]+$`)

// linuxConfig is the configuration a system service starts with. A file already in place is never replaced.
const linuxConfig = `# taraengine system service configuration. The README's Configuration section lists every key.
data_dir: /var/lib/taraengine
`

// RenderPlist is the LaunchAgent that runs binary as `serve --service`, starts at login, keeps it alive for as long as
// binary exists, and sends what it prints to logPath. Every path is escaped, so it cannot fail.
func RenderPlist(binary, logPath string) []byte {
	return mustRender("launchd.plist.tmpl", map[string]string{"Binary": binary, "Log": logPath})
}

// RenderUnit is the systemd system unit that runs binary as `serve --service` under the taraengine user.
func RenderUnit(binary string) ([]byte, error) {
	if !unitSafePath.MatchString(binary) {
		return nil, fmt.Errorf("service: the binary path %q holds characters a unit file would misread; "+
			"install taraengine under a plain path such as /usr/local/bin", binary)
	}
	return mustRender("systemd.service.tmpl", map[string]string{"Binary": binary}), nil
}

// LinuxConfig is the configuration file a system service gets when it has none.
func LinuxConfig() []byte { return []byte(linuxConfig) }

// mustRender renders a template of this package. It fails only for a name the package does not embed, which no caller
// passes.
func mustRender(name string, data map[string]string) []byte {
	out, err := render(name, data)
	if err != nil {
		panic(err)
	}
	return out
}

func render(name string, data map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	return buf.Bytes(), nil
}

// xmlText escapes s for the text of an XML element. Writing to a buffer cannot fail.
func xmlText(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
