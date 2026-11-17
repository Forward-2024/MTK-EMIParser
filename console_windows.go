//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleCP   = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// initConsole：把控制台切到 UTF-8 代码页，并开启 ANSI/VT 支持
func initConsole() {
	_, _, _ = procSetConsoleCP.Call(uintptr(65001))
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		return
	}
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return
	}
	// ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004
	_, _, _ = procSetConsoleMode.Call(uintptr(h), uintptr(mode|0x0004))
}

func timeNow() string { return time.Now().Format("2006-01-02 15:04:05") }

var _ = unsafe.Pointer(nil)
