//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

// runGUI opens a native application window (hosting the system WebView2
// component) with the viewer UI inside, and blocks until it is closed.
func runGUI(url string) error {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		// Keep browser profile data out of the exe's directory so the app
		// stays portable and works from read-only locations.
		DataPath: filepath.Join(os.TempDir(), "leveldb-viewer-webview2"),
		WindowOptions: webview2.WindowOptions{
			Title:  "LevelDB Viewer — Chrome storage inspector",
			Width:  1360,
			Height: 900,
			Center: true,
		},
	})
	if w == nil {
		return errors.New("could not create a WebView2 window (the WebView2 runtime / Microsoft Edge may be missing)")
	}
	defer w.Destroy()
	w.Navigate(url)
	w.Run()
	return nil
}

// notifyFatal shows a native error dialog; the exe is built without a
// console window, so this is the only way a startup error can be seen.
func notifyFatal(msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	messageBox := user32.NewProc("MessageBoxW")
	title, _ := syscall.UTF16PtrFromString("LevelDB Viewer")
	text, _ := syscall.UTF16PtrFromString(msg)
	const mbIconError = 0x10
	messageBox.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), mbIconError)
}
