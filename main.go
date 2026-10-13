package main

import (
	"errors"
	"fmt"
	"os"

	cmd "github.com/cr1cr1/o365bridge/cmd"

	"github.com/rs/zerolog/log"
)

var version = "dev"

func main() {
	if err := cmd.Run(version, os.Args[1:]); err != nil {
		var exitErr *cmd.ExitError
		if errors.As(err, &exitErr) {
			fmt.Fprintln(os.Stderr, exitErr.Error())
			os.Exit(exitErr.Code)
		}
		log.Fatal().Err(err).Msg("Failed to run application")
	}
}
