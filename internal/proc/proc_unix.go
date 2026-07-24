//go:build !windows

package proc

import "syscall"

type sig = syscall.Signal

const (
	sigTerm = syscall.SIGTERM
	sigHup  = syscall.SIGHUP
)

func signalDaemon(pidfile string, s sig) error {
	pid, err := readPid(pidfile)
	if err != nil {
		return err
	}
	return syscall.Kill(pid, s)
}

// processAlive reports whether pid names a live process: signal 0
// checks for existence without delivering anything.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
