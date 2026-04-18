//go:build darwin || linux

package main

import "syscall"

func newSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
