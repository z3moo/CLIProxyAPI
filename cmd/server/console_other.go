//go:build !windows

package main

func attachParentConsole() bool           { return true }
func allocFreshConsole(title string) bool { _ = title; return true }
func freeAttachedConsole()                {}
func hasConsole() bool                    { return true }
func setConsoleWindowVisible(show bool)   { _ = show }

var onConsoleCloseRequested = func() {}
