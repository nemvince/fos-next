// Package netup handles NIC enumeration, DHCP, and connectivity detection.
// It brings up all non-loopback interfaces and waits for at least one to
// obtain an IP address before returning.
package netup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	sysNetPath   = "/sys/class/net"
	pollInterval = 500 * time.Millisecond
)

// NIC holds information about a single network interface.
type NIC struct {
	Name string
	MAC  string
}

// ListNICs returns all non-loopback NICs present on the system, read from sysfs.
func ListNICs() ([]NIC, error) {
	entries, err := os.ReadDir(sysNetPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sysNetPath, err)
	}
	var nics []NIC
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		mac, err := os.ReadFile(filepath.Join(sysNetPath, name, "address"))
		if err != nil {
			continue
		}
		nics = append(nics, NIC{Name: name, MAC: strings.TrimSpace(string(mac))})
	}
	return nics, nil
}

// BringUp runs ip-link up on every NIC and then spawns udhcpc on each.
// It blocks until at least one interface has an IPv4 address or ctx is
// cancelled. The primary MAC (from the first interface that gets an address)
// is returned.
func BringUp(ctx context.Context) (primaryMAC string, err error) {
	nics, err := ListNICs()
	if err != nil {
		return "", err
	}
	if len(nics) == 0 {
		return "", errors.New("no non-loopback NICs found")
	}

	for _, nic := range nics {
		linkUp(nic.Name)
	}

	for _, nic := range nics {
		go runUDHCPC(nic.Name)
	}

	slog.Info("waiting for IP address")
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
			mac, ok := firstAddressedMAC(nics)
			if ok {
				slog.Info("network up", "mac", mac)
				return mac, nil
			}
		}
	}
}

// linkUp runs `ip link set <iface> up`.
func linkUp(iface string) {
	cmd := exec.Command("ip", "link", "set", iface, "up")
	if err := cmd.Run(); err != nil {
		slog.Warn("ip link set up failed", "iface", iface, "err", err)
	}
}

// runUDHCPC runs udhcpc on the given interface and keeps it running so that
// DHCP lease renewals are handled during long imaging sessions.
// The -q flag (quit after lease) is intentionally omitted.
// It is expected to be called from a goroutine; errors are logged.
func runUDHCPC(iface string) {
	cmd := exec.Command("udhcpc", "-i", iface, "-f", "-n")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		slog.Warn("udhcpc exited with error", "iface", iface, "err", err)
	}
}

// firstAddressedMAC returns the MAC address of the first NIC that has a
// non-loopback, non-link-local IPv4 address assigned.
func firstAddressedMAC(nics []NIC) (string, bool) {
	for _, nic := range nics {
		iface, err := net.InterfaceByName(nic.Name)
		if err != nil {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}
			if ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return nic.MAC, true
			}
		}
	}
	return "", false
}
