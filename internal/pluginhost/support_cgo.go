//go:build cgo || windows

package pluginhost

// Windows loads plugins through syscall (loader_windows.go) and does not need cgo.
const supportPluginValue = "1"
