//go:build !windows

package taskrun

import (
	"os"
	"syscall"
)

// processAlive reports whether a pid is still running. Signal 0 performs the
// permission and existence checks without delivering anything.
//
// This only ever runs against a pid recorded on THIS host (Reconcile checks the
// hostname first), so a recycled pid is the only false positive, and its cost is
// waiting out the heartbeat timeout rather than a wrong verdict.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
