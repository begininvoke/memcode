//go:build windows

package taskrun

import "os"

// processAlive reports whether a pid is still running. On Windows FindProcess
// fails outright for a dead pid, so existence is the whole check.
func processAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}
