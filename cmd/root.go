package cmd

import (
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// ExitError carries a process exit code for a CLI failure.
// Usage/parse errors use code 2; runtime errors use code 1.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

const appName = "o365bridge"

// CurrentLogFormat records the active log format set at runtime.
var CurrentLogFormat string

// RootFlags holds global CLI flags.
type RootFlags struct {
	LogLevel          string `help:"Log level (trace,debug,info,warn,error)" enum:"trace,debug,info,warn,error" default:"info" env:"LOG_LEVEL"`
	LogFormat         string `help:"Log format (console,json)" enum:"console,json" default:"console" env:"LOG_FORMAT"`
	Profiling         bool   `help:"Enable pprof profiling server" default:"false"`
	ProfilingListenOn string `help:"Listen address for pprof profiling server" default:"0.0.0.0:6060"`
}

// CLI represents the main CLI structure.
type CLI struct {
	RootFlags `kong:"embed"`

	Version VersionCmd `cmd:"" help:"Show version information"`
	Serve   ServeCmd   `cmd:"" help:"Serve the local mail servers"`

	args []string // original args, used to detect --help before side effects
}

// AfterApply is called after Kong parses the CLI but before the command runs.
func (cli *CLI) AfterApply(kctx *kong.Context) error {
	if kctx.Command() == "version" || slices.Contains(cli.args, "--help") || slices.Contains(cli.args, "-h") {
		return nil
	}
	if err := setGlobalLoggerLogLevel(cli.LogLevel); err != nil {
		return fmt.Errorf("set log level: %w", err)
	}
	if err := setGlobalLoggerLogFormat(cli.LogFormat); err != nil {
		return fmt.Errorf("set log format: %w", err)
	}
	if cli.Profiling {
		log.Info().Str("listen", cli.ProfilingListenOn).Msg("Starting pprof profiling server")
		runtime.SetBlockProfileRate(1)
		go func() { http.ListenAndServe(cli.ProfilingListenOn, nil) }()
	}
	return nil
}

func setGlobalLoggerLogLevel(levelStr string) error {
	level, err := zerolog.ParseLevel(levelStr)
	if err != nil {
		return fmt.Errorf("parse log level %q: %w", levelStr, err)
	}
	zerolog.SetGlobalLevel(level)
	log.Logger = log.Logger.Level(level)
	return nil
}

func setGlobalLoggerLogFormat(format string) error {
	CurrentLogFormat = format
	switch format {
	case "console":
		log.Logger = log.Logger.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	case "json":
		log.Logger = log.Logger.Output(os.Stderr)
	default:
		return fmt.Errorf("invalid log format %q", format)
	}
	return nil
}

// Run executes the CLI with the given version.
func Run(version string, args []string) error {
	_ = godotenv.Load(".env", ".local.env", ".dev.env")

	var cli CLI
	parser, err := kong.New(
		&cli,
		kong.Name(appName),
		kong.Description("Outlook365 Bridge"),
		kong.UsageOnError(),
		kong.DefaultEnvars("OB"),
		kong.Exit(func(code int) {
			if testing.Testing() {
				return
			}
			if code == 1 {
				code = 2
			}
			os.Exit(code)
		}),
	)
	if err != nil {
		return &ExitError{Code: 2, Err: fmt.Errorf("create CLI parser: %w", err)}
	}

	cli.args = args
	kctx, err := parser.Parse(args)
	if err != nil {
		if slices.Contains(args, "--help") || slices.Contains(args, "-h") {
			return nil
		}
		return &ExitError{Code: 2, Err: err}
	}
	if slices.Contains(args, "--help") || slices.Contains(args, "-h") {
		return nil
	}

	deps, err := newDeps(version)
	if err != nil {
		return &ExitError{Code: 1, Err: err}
	}

	if err := kctx.Run(deps); err != nil {
		// A command's own ExitError (its code was chosen deliberately)
		// must not be re-wrapped to 1.
		var ee *ExitError
		if errors.As(err, &ee) {
			return ee
		}
		return &ExitError{Code: 1, Err: err}
	}
	return nil
}
