//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// A Windows console renders output through a code page. The default is a
// legacy one (437 in the US, 850 in western Europe), under which the UTF-8
// bytes Go writes for a character like U+2588 come out as two or three pieces
// of mojibake. Code page 65001 is UTF-8.
const cpUTF8 = 65001

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleOutCP  = kernel32.NewProc("GetConsoleOutputCP")
	procSetConsoleOutCP  = kernel32.NewProc("SetConsoleOutputCP")
	procGetScreenBufInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	originalOutputCP     uintptr
	restoreOriginalOutCP bool
)

type coord struct{ X, Y int16 }

type smallRect struct{ Left, Top, Right, Bottom int16 }

// consoleScreenBufferInfo mirrors CONSOLE_SCREEN_BUFFER_INFO.
type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// consoleWidth returns the visible width of the console attached to handle, or
// 0 when there is none. The window rectangle is what matters, not the screen
// buffer, which is usually far wider than what is on screen.
func consoleWidth(handle uintptr) int {
	var info consoleScreenBufferInfo
	ret, _, _ := procGetScreenBufInfo.Call(handle, uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		return 0
	}
	return int(info.Window.Right - info.Window.Left + 1)
}

// enableUnicodeOutput switches the console to UTF-8 and reports whether output
// can now carry characters outside ASCII. The previous code page is remembered
// so restoreConsole can put it back: the setting belongs to the console window,
// not to this process, and would otherwise outlive the command.
func enableUnicodeOutput() bool {
	cp, _, _ := procGetConsoleOutCP.Call()
	if cp == 0 {
		// Not attached to a console at all - output is redirected, and
		// whatever consumes it will read raw UTF-8 bytes correctly.
		return true
	}
	if cp == cpUTF8 {
		return true
	}
	if ret, _, _ := procSetConsoleOutCP.Call(cpUTF8); ret == 0 {
		return false
	}
	originalOutputCP, restoreOriginalOutCP = cp, true
	return true
}

func restoreConsole() {
	if restoreOriginalOutCP {
		procSetConsoleOutCP.Call(originalOutputCP)
		restoreOriginalOutCP = false
	}
}
