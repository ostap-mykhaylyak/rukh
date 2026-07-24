// Package proc handles the pidfile and daemon signaling: the running
// daemon writes the pidfile, `rukh stop` and `rukh reload` read it and
// send SIGTERM/SIGHUP. Signaling is Unix-only; on other systems it
// returns an explanatory error (development happens cross-platform,
// deployment is Linux).
package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// WritePidfile records the current PID at path, creating the parent
// directory if needed.
func WritePidfile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
}

// RemovePidfile deletes the pidfile, ignoring errors: shutdown must
// not fail because of it.
func RemovePidfile(path string) { os.Remove(path) }

// readPid parses the PID stored at path.
func readPid(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("pidfile %s: %w (is the daemon running?)", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("pidfile %s: invalid content", path)
	}
	return pid, nil
}

// Stop asks the daemon behind the pidfile to shut down (SIGTERM).
func Stop(pidfile string) error { return signalDaemon(pidfile, sigTerm) }

// Reload asks the daemon behind the pidfile to reload (SIGHUP).
func Reload(pidfile string) error { return signalDaemon(pidfile, sigHup) }

// Running reports whether the daemon behind the pidfile is alive.
// A missing or stale pidfile means not running.
func Running(pidfile string) bool {
	pid, err := readPid(pidfile)
	if err != nil {
		return false
	}
	return processAlive(pid)
}

// StopAndWait shuts the daemon down and blocks until it has exited or
// timeout elapses, so a restart never races the old process still
// holding the listening sockets. It is a no-op when nothing is
// running.
func StopAndWait(pidfile string, timeout time.Duration) error {
	if !Running(pidfile) {
		return nil // already stopped: restart just starts a new one
	}
	if err := Stop(pidfile); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !Running(pidfile) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not stop within %s", timeout)
}
