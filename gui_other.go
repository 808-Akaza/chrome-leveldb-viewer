//go:build !windows

package main

import "errors"

// runGUI is Windows-only; other platforms fall back to the browser UI.
func runGUI(url string) error {
	return errors.New("native window mode is only available on Windows")
}

func notifyFatal(msg string) {}
