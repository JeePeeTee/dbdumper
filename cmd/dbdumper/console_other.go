//go:build !windows

package main

// Everywhere except Windows the terminal encoding is UTF-8 and there is
// nothing to negotiate.
func enableUnicodeOutput() bool { return true }

func restoreConsole() {}

// consoleWidth is not probed off Windows; the caller falls back to a default.
func consoleWidth(uintptr) int { return 0 }
