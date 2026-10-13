package cmd

import (
	"io"
	"os"
)

// Deps carries the process-level dependencies commands run against.
// Deliberately minimal: a version string and output writers. There
// is no store or persistence handle here — the bridge's own session
// cache is owned by the bridge registry built inside ServeCmd.
type Deps struct {
	Version string
	Out     io.Writer
	ErrOut  io.Writer
}

func newDeps(version string) (*Deps, error) {
	return &Deps{Version: version, Out: os.Stdout, ErrOut: os.Stderr}, nil
}
