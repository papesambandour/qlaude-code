//go:build windows

package proxy

import "syscall"

// detachProcAttr returns a SysProcAttr with CREATE_NEW_PROCESS_GROUP so the
// daemon is detached from the parent console on Windows.
func detachProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: 0x00000200, // CREATE_NEW_PROCESS_GROUP
	}
}
