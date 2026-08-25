//go:build !windows

package main

import (
	"os"
	"syscall"
)

// reexec replaces the current process with the (new) binary, same args/env.
// The listening socket closes on exec and the new process re-binds it.
func reexec(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}
