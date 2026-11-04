package pluginhost

import (
	"fmt"
	"testing"
)

// The management header must match the loader compiled into this binary: "1"
// only when plugins can actually load (cgo builds and every Windows build).
func TestSupportPluginHeaderMatchesLoader(t *testing.T) {
	loader := fmt.Sprintf("%T", defaultPluginLoader())
	want := "1"
	if loader == "pluginhost.unsupportedLoader" {
		want = "0"
	}
	if got := SupportPluginHeaderValue(); got != want {
		t.Fatalf("SupportPluginHeaderValue() = %q, want %q for loader %s", got, want, loader)
	}
}
