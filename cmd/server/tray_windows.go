//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/getlantern/systray"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cmd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
)

//go:embed tray_icon.ico
var trayIconData []byte

// defaultTrayMode is the default value for the --tray flag on Windows: true,
// so launching the exe with no arguments drops to the tray.
func defaultTrayMode() bool { return true }

const ringCap = 2000

var (
	ringMu  sync.Mutex
	ringBuf [][]byte

	trayMu        sync.Mutex
	trayConsoleOn bool
	trayConsoleFD io.Writer
	mToggleRef    *systray.MenuItem
)

// logRingHook captures every formatted log line into a bounded ring buffer
// and, when the on-demand console is open, also writes it live to that
// console. Independent of the global logrus output writer (which may be a
// rotating file when LoggingToFile is enabled).
type logRingHook struct{}

func (logRingHook) Levels() []log.Level { return log.AllLevels }

func (logRingHook) Fire(e *log.Entry) error {
	fmtter := e.Logger.Formatter
	if fmtter == nil {
		fmtter = &logging.LogFormatter{}
	}
	line, err := fmtter.Format(e)
	if err != nil {
		return err
	}
	cp := make([]byte, len(line))
	copy(cp, line)

	ringMu.Lock()
	if len(ringBuf) >= ringCap {
		ringBuf = ringBuf[1:]
	}
	ringBuf = append(ringBuf, cp)
	ringMu.Unlock()

	trayMu.Lock()
	w := trayConsoleFD
	trayMu.Unlock()
	if w != nil {
		_, _ = w.Write(line)
	}
	return nil
}

func replayRingTo(w io.Writer) {
	ringMu.Lock()
	snap := make([][]byte, len(ringBuf))
	copy(snap, ringBuf)
	ringMu.Unlock()
	for _, line := range snap {
		_, _ = w.Write(line)
	}
}

func showTrayConsole() {
	trayMu.Lock()
	if trayConsoleOn {
		trayMu.Unlock()
		setConsoleWindowVisible(true)
		return
	}
	trayMu.Unlock()

	if !allocFreshConsole("CLIProxyAPI Console") {
		log.Warn("tray: failed to allocate console")
		return
	}

	// allocFreshConsole rebinds os.Stdout/os.Stderr to CONOUT$.
	out := os.Stdout

	trayMu.Lock()
	trayConsoleFD = out
	trayConsoleOn = true
	if mToggleRef != nil {
		mToggleRef.SetTitle("Hide Console")
	}
	trayMu.Unlock()

	go replayRingTo(out)
}

func hideTrayConsole() {
	trayMu.Lock()
	if !trayConsoleOn {
		trayMu.Unlock()
		return
	}
	trayConsoleFD = nil
	trayConsoleOn = false
	if mToggleRef != nil {
		mToggleRef.SetTitle("Show Console")
	}
	trayMu.Unlock()

	freeAttachedConsole()
}

// runTrayMode starts the proxy server in the background and shows a system
// tray icon. The console is never attached until the user picks Show Console.
// Built with the windowsgui subsystem, this binary inherits no console, so
// closing the parent terminal cannot kill the process.
func runTrayMode(cfg *config.Config, configFilePath, password string) {
	// If main attached to a parent shell so --help/login flows work, detach
	// now: tray mode owns its own console window from this point on.
	freeAttachedConsole()

	log.AddHook(logRingHook{})
	onConsoleCloseRequested = func() { hideTrayConsole() }

	cancel, done := cmd.StartServiceBackground(cfg, configFilePath, password)

	onReady := func() {
		systray.SetIcon(trayIconData)
		systray.SetTitle("CLIProxyAPI")
		systray.SetTooltip(fmt.Sprintf("CLIProxyAPI on :%d", cfg.Port))

		mToggle := systray.AddMenuItem("Show Console", "Show or hide the console window")
		mOpen := systray.AddMenuItem("Open Management UI", "Open the management UI in your browser")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Exit", "Stop the proxy and exit")

		trayMu.Lock()
		mToggleRef = mToggle
		trayMu.Unlock()

		go func() {
			for {
				select {
				case <-mToggle.ClickedCh:
					trayMu.Lock()
					on := trayConsoleOn
					trayMu.Unlock()
					if on {
						hideTrayConsole()
					} else {
						showTrayConsole()
					}
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
		hideTrayConsole()
		cancel()
		<-done
	}

	systray.Run(onReady, onExit)
}
