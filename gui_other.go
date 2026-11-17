//go:build !windows

package main

// 非 Windows 平台没有 Win32 窗口，只会走命令行模式。
func guiSupported() bool { return false }

func runGUI(args []string, selfTestOut, dropTarget string) int { return 1 }
