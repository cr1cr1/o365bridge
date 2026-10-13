package cmd

import (
	"fmt"
)

// VersionCmd shows version information
type VersionCmd struct{}

// Run prints the application name and version.
func (v *VersionCmd) Run(d *Deps) error {
	fmt.Fprintf(d.Out, "%s %s\n", appName, d.Version)
	return nil
}
