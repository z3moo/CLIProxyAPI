//go:build windows

package main

import (
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	attachParentProcess = ^uintptr(0) // -1
	ctrlCloseEvent      = 2
)

var (
	conKernel32       = windows.NewLazySystemDLL("kernel32.dll")
	conUser32         = windows.NewLazySystemDLL("user32.dll")
	procAttachConsole = conKernel32.NewProc("AttachConsole")
	procAllocConsole  = conKernel32.NewProc("AllocConsole")
	procFreeConsole   = conKernel32.NewProc("FreeConsole")
	procGetConsoleWnd = conKernel32.NewProc("GetConsoleWindow")
	procSetTitleW     = conKernel32.NewProc("SetConsoleTitleW")
	procSetCtrlHndlr  = conKernel32.NewProc("SetConsoleCtrlHandler")
	procShowWnd       = conUser32.NewProc("ShowWindow")
	procSetForeground = conUser32.NewProc("SetForegroundWindow")

	ctrlHandlerOnce sync.Once
	ctrlHandlerCB   uintptr
)

// consoleHWND returns the HWND of the console window currently attached to
// this process, or 0 if no console is attached.
func consoleHWND() windows.Handle {
	hwnd, _, _ := procGetConsoleWnd.Call()
	return windows.Handle(hwnd)
}

// hasConsole reports whether this process currently has a console attached.
func hasConsole() bool { return consoleHWND() != 0 }

// attachParentConsole tries to attach this process to its parent's console
// (e.g. the PowerShell that launched it). Returns true on success. When the
// binary is double-clicked there is no parent console and this returns false.
func attachParentConsole() bool {
	r1, _, _ := procAttachConsole.Call(attachParentProcess)
	if r1 == 0 {
		return false
	}
	installCloseHandler()
	rebindStdHandles()
	return true
}

// allocFreshConsole creates a new console window for this process and rebinds
// stdout/stderr/stdin to it. Use this when no console is currently attached.
func allocFreshConsole(title string) bool {
	if hasConsole() {
		return true
	}
	if r1, _, _ := procAllocConsole.Call(); r1 == 0 {
		return false
	}
	if title != "" {
		if p, err := windows.UTF16PtrFromString(title); err == nil {
			procSetTitleW.Call(uintptr(unsafe.Pointer(p)))
		}
	}
	installCloseHandler()
	rebindStdHandles()
	return true
}

// freeAttachedConsole detaches and closes the current console (if any) and
// points stdout/stderr at NUL so subsequent writes are harmless.
func freeAttachedConsole() {
	if !hasConsole() {
		return
	}
	procFreeConsole.Call()
	if devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stdout = devNull
		os.Stderr = devNull
	}
}

// rebindStdHandles points os.Stdout/os.Stderr/os.Stdin at the currently
// attached console's CONOUT$/CONIN$ devices.
func rebindStdHandles() {
	if f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
		_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(f.Fd()))
		_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
		os.Stdin = f
		_ = windows.SetStdHandle(windows.STD_INPUT_HANDLE, windows.Handle(f.Fd()))
	}
}

// installCloseHandler registers a Ctrl handler that intercepts the X button on
// the console window so closing it hides the console instead of killing the
// process. The handler is registered exactly once per process.
func installCloseHandler() {
	ctrlHandlerOnce.Do(func() {
		ctrlHandlerCB = windows.NewCallback(func(ctrlType uint32) uintptr {
			if ctrlType == ctrlCloseEvent {
				go func() { onConsoleCloseRequested() }()
				return 1
			}
			return 0
		})
	})
	procSetCtrlHndlr.Call(ctrlHandlerCB, 1)
}

// onConsoleCloseRequested is set by the tray code so it can mirror UI state
// when the user clicks the X on the console window.
var onConsoleCloseRequested = func() { freeAttachedConsole() }

// setConsoleWindowVisible hides or shows the existing console window without
// detaching the process from it. Used when a parent shell owns the console
// and we want to flash its window forward.
func setConsoleWindowVisible(show bool) {
	hwnd := consoleHWND()
	if hwnd == 0 {
		return
	}
	const swHide = 0
	const swShow = 5
	if show {
		procShowWnd.Call(uintptr(hwnd), uintptr(swShow))
		procSetForeground.Call(uintptr(hwnd))
	} else {
		procShowWnd.Call(uintptr(hwnd), uintptr(swHide))
	}
}
