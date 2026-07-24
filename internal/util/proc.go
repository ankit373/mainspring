package util

import (
	"net"
	"strings"
)

// FreePort returns an available localhost TCP port. There is an inherent race
// between closing the listener and a child process binding the port, but it is
// the standard approach and the window is negligible in practice.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// FirstLine returns the first line of s, trimmed.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// Tail returns the last n lines of s.
func Tail(s string, n int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	return strings.Join(parts, "\n")
}
