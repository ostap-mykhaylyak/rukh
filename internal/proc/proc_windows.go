//go:build windows

package proc

import "errors"

type sig = int

const (
	sigTerm sig = 15
	sigHup  sig = 1
)

func signalDaemon(pidfile string, _ sig) error {
	if _, err := readPid(pidfile); err != nil {
		return err
	}
	return errors.New("stop/reload via signals is not supported on windows; use Ctrl+C on the foreground daemon")
}

// processAlive cannot be determined portably on windows without the
// process handle; the service commands are Linux-only anyway.
func processAlive(int) bool { return false }
