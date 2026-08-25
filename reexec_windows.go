//go:build windows

package main

import (
	"os"
	"os/exec"
)

// Windows has no exec(2). Spawn the new binary detached and exit; if we're
// running under a service wrapper it restarts us anyway.
func reexec(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
