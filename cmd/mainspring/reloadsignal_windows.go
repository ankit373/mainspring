//go:build windows

package main

import "os"

// Windows has no SIGHUP. syscall.SIGHUP is nonetheless *defined* on Windows, so
// the previous `signal.Notify(hup, syscall.SIGHUP)` compiled here and simply
// never fired — config reload appeared to be wired up and silently did nothing.
// That is the failure mode this project exists to eliminate, so the capability is
// declared absent and the working alternative is named at startup.
const reloadSignalName = "SIGHUP (unavailable on Windows)"

func notifyReloadSignal(chan<- os.Signal) bool { return false }

func stopReloadSignal(chan<- os.Signal) {}
