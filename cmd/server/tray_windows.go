//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"github.com/getlantern/systray"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cmd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
)

//go:embed tray_icon.ico
var trayIconData []byte

const (
	swHide = 0
	swShow = 5
)

var (
	user32                 = windows.NewLazySystemDLL("user32.dll")
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procShowWindow         = user32.NewProc("ShowWindow")
	procSetForeground      = user32.NewProc("SetForegroundWindow")
	procGetConsole         = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcList = kernel32.NewProc("GetConsoleProcessList")

	consoleMu      sync.Mutex
	consoleVisible = true
)

// ownsConsole returns true when this process is the sole attached process for
// the console window — i.e. Windows allocated it for us at launch. When the
// binary is started from an existing shell (PowerShell, cmd) the parent shell
// is also attached and we must not hide that window.
func ownsConsole() bool {
	var dummy [4]uint32
	n, _, _ := procGetConsoleProcList.Call(uintptr(unsafe.Pointer(&dummy[0])), uintptr(len(dummy)))
	return n == 1
}

func consoleWindow() windows.Handle {
	hwnd, _, _ := procGetConsole.Call()
	return windows.Handle(hwnd)
}

func setConsoleVisible(show bool) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	hwnd := consoleWindow()
	if hwnd == 0 {
		return
	}
	if show {
		procShowWindow.Call(uintptr(hwnd), uintptr(swShow))
		procSetForeground.Call(uintptr(hwnd))
		consoleVisible = true
	} else {
		procShowWindow.Call(uintptr(hwnd), uintptr(swHide))
		consoleVisible = false
	}
}

func toggleConsole() {
	consoleMu.Lock()
	visible := consoleVisible
	consoleMu.Unlock()
	setConsoleVisible(!visible)
}

// runTrayMode starts the proxy server in the background and shows a system
// tray icon. The console window is hidden on launch (when this process owns
// it); left-click toggles it, right-click opens a menu with Show/Hide Console,
// Open Management UI, and Exit.
func runTrayMode(cfg *config.Config, configFilePath, password string) {
	if ownsConsole() {
		setConsoleVisible(false)
	} else {
		log.Info("tray: parent shell owns the console; not hiding it. Launch the .exe directly to start fully minimized.")
	}

	cancel, done := cmd.StartServiceBackground(cfg, configFilePath, password)

	onReady := func() {
		systray.SetIcon(trayIconData)
		systray.SetTitle("CLIProxyAPI")
		systray.SetTooltip(fmt.Sprintf("CLIProxyAPI on :%d", cfg.Port))

		mToggle := systray.AddMenuItem("Show Console", "Show or hide the console window")
		mOpen := systray.AddMenuItem("Open Management UI", "Open the management UI in your browser")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Exit", "Stop the proxy and exit")

		go func() {
			for {
				select {
				case <-mToggle.ClickedCh:
					toggleConsole()
					consoleMu.Lock()
					if consoleVisible {
						mToggle.SetTitle("Hide Console")
					} else {
						mToggle.SetTitle("Show Console")
					}
					consoleMu.Unlock()
				case <-mOpen.ClickedCh:
					url := fmt.Sprintf("http://127.0.0.1:%d/management", cfg.Port)
					if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start(); err != nil {
						log.Warnf("tray: failed to open browser: %v", err)
					}
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}

	onExit := func() {
		setConsoleVisible(true)
		cancel()
		<-done
	}

	systray.Run(onReady, onExit)
}
