// Package cmdline parses the kernel command line from /proc/cmdline and
// extracts the fog-specific parameters that control fos-agent behaviour.
package cmdline

import (
	"os"
	"strings"
)

const procCmdline = "/proc/cmdline"

// Params holds the fog-relevant values extracted from the kernel command line.
type Params struct {
	// FogServer is the base URL of the fog-next server (e.g. "http://10.0.0.1").
	FogServer string
	// FogAction overrides the action returned by the handshake.
	// Useful for forcing "debug" without a pending task.
	FogAction string
	// FogHost is an optional MAC address or hostname hint passed by iPXE.
	FogHost string
	// FogDebug enables verbose logging when set to "1" or "true".
	FogDebug bool
}

// Parse reads /proc/cmdline and returns the fog-specific parameters.
func Parse() (*Params, error) {
	raw, err := os.ReadFile(procCmdline)
	if err != nil {
		return nil, err
	}
	return ParseString(strings.TrimSpace(string(raw))), nil
}

// ParseString parses a cmdline string directly (used in tests).
func ParseString(cmdline string) *Params {
	p := &Params{}
	for _, tok := range strings.Fields(cmdline) {
		k, v, _ := strings.Cut(tok, "=")
		switch k {
		case "fog_server":
			p.FogServer = v
		case "fog_action":
			p.FogAction = v
		case "fog_host":
			p.FogHost = v
		case "fog_debug":
			p.FogDebug = v == "1" || strings.EqualFold(v, "true")
		}
	}
	return p
}
