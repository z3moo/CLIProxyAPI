//go:build !windows

package main

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// runTrayMode is a stub on non-Windows platforms. The system tray feature is
// Windows-only; on other platforms the user should run the proxy normally or
// use --tui.
func runTrayMode(cfg *config.Config, configFilePath, password string) {
	fmt.Println("--tray is only supported on Windows; falling back to normal foreground mode")
	_ = cfg
	_ = configFilePath
	_ = password
}
