//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// reloadSignalName is what an operator sends to hot-reload config.
const reloadSignalName = "SIGHUP"

// notifyReloadSignal subscribes ch to the reload signal, returning false when the
// platform has no such signal. Referencing syscall.SIGHUP is confined to this
// build-tagged file: the constant happens to exist on Windows too, so relying on
// compilation to catch its absence would not work.
func notifyReloadSignal(ch chan<- os.Signal) bool {
	signal.Notify(ch, syscall.SIGHUP)
	return true
}

func stopReloadSignal(ch chan<- os.Signal) { signal.Stop(ch) }
