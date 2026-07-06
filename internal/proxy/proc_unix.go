//go:build !windows

package proxy

import "syscall"

// detachProcAttr returns SysProcAttr that creates a new session (setsid)
// so the daemon process is fully detached from the parent terminal.
func detachProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
