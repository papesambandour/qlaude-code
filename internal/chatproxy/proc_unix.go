//go:build !windows

package chatproxy

import "syscall"

func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
