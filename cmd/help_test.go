package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func TestServeHelpGroupsFlags(t *testing.T) {
	// Pin help defaults independently of the developer's environment.
	for name, value := range map[string]string{
		"LOG_LEVEL": "info", "LOG_FORMAT": "console", "OB_PROFILING": "false",
		"OB_INTERFACE": "127.0.0.1", "OB_IMAP_PORT": "1143", "OB_SMTP_PORT": "1025",
		"OB_DAV_PORT": "1080", "OB_JMAP_PORT": "18080", "OB_TLS_ENABLED": "false",
		"OB_TLS_MIN_VERSION": "1.3", "OB_IMAP_TLS_MODE": "starttls", "OB_SMTP_TLS_MODE": "starttls",
	} {
		t.Setenv(name, value)
	}
	var out bytes.Buffer
	args := []string{"serve", "--help"}
	cli := CLI{args: args}
	parser, err := kong.New(&cli,
		kong.Name(appName), kong.DefaultEnvars("OB"),
		kong.Writers(&out, &out),
		kong.Exit(func(code int) {
			if code != 0 {
				t.Errorf("help exit code = %d", code)
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Parse(args); err != nil {
		t.Fatal(err)
	}
	help := out.String()
	groups := map[string][]string{
		"Listeners:":         {"--interface", "--imap-port", "--smtp-port", "--dav-port", "--jmap-port"},
		"TLS:":               {"--tls-enabled", "--tls-cert", "--tls-key", "--tls-min-version", "--imap-tls-mode", "--smtp-tls-mode"},
		"Authentication:":    {"--password", "--graph-client-id"},
		"State and browser:": {"--state-dir", "--browser-path"},
		"Logging:":           {"--log-level", "--log-format"},
		"Profiling:":         {"--profiling", "--profiling-listen-on"},
	}
	// Extract each top-level help section and pin the flag placement,
	// not its column widths or wrapping (those depend on terminal size).
	for group, flags := range groups {
		start := strings.Index(help, "\n"+group+"\n")
		if start < 0 {
			t.Fatalf("help lacks group %q:\n%s", group, help)
		}
		section := help[start+len(group)+2:]
		for other := range groups {
			if i := strings.Index(section, "\n"+other+"\n"); i >= 0 {
				section = section[:i]
			}
		}
		for _, flag := range flags {
			if !strings.Contains(section, flag) {
				t.Errorf("%s does not contain %s:\n%s", group, flag, section)
			}
		}
	}
	// Help metadata must not change parsing defaults or flag spelling.
	if cli.Serve.Interface != "127.0.0.1" || cli.Serve.ImapPort != 1143 || cli.Serve.SmtpPort != 1025 || cli.Serve.DavPort != 1080 || cli.Serve.JmapPort != 18080 {
		t.Fatal("help grouping changed listener defaults")
	}
	if cli.Serve.TLSEnabled || cli.Serve.TLSMinVersion != "1.3" || cli.Serve.ImapTLSMode != "starttls" || cli.Serve.SmtpTLSMode != "starttls" {
		t.Fatal("help grouping changed TLS defaults")
	}
}
