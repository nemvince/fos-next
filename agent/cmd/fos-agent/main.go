// fos-agent is the init process for the fos-next initramfs.
// It runs as PID 1, brings up the network, handshakes with fog-next,
// and dispatches the appropriate imaging action.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	fogapi "github.com/nemvince/fos-next/internal/api"
	"github.com/nemvince/fos-next/internal/actions"
	"github.com/nemvince/fos-next/internal/cmdline"
	"github.com/nemvince/fos-next/internal/netup"
)

func main() {
	setupLogging()

	slog.Info("fos-agent starting")

	params, err := cmdline.Parse()
	if err != nil {
		slog.Error("cannot read kernel cmdline", "err", err)
		halt(1)
	}
	slog.Info("cmdline parsed", "server", params.FogServer, "action", params.FogAction, "host", params.FogHost)

	if params.FogServer == "" {
		slog.Error("fog_server not set on kernel command line")
		halt(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Network bringup — block until we have an IP or timeout.
	netCtx, netCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer netCancel()
	primaryMAC, err := netup.BringUp(netCtx)
	if err != nil {
		slog.Error("network bringup failed", "err", err)
		halt(1)
	}

	client := fogapi.New(params.FogServer)

	// Collect all MACs for the handshake.
	nics, _ := netup.ListNICs()
	macs := make([]string, 0, len(nics))
	for _, n := range nics {
		macs = append(macs, n.MAC)
	}
	// Ensure primary MAC is first.
	reorder(macs, primaryMAC)

	slog.Info("starting handshake", "macs", macs)
	resp, err := client.Handshake(ctx, fogapi.HandshakeRequest{MACs: macs})
	if err != nil {
		slog.Error("handshake failed", "err", err)
		halt(1)
	}

	// Override action from cmdline if set (e.g. fog_action=debug).
	if params.FogAction != "" && params.FogAction != resp.Action {
		slog.Info("cmdline overrides action", "from", resp.Action, "to", params.FogAction)
		resp.Action = params.FogAction
	}

	// Register action skips the handshake task path entirely.
	if resp.Action == "register" {
		if err := actions.Register(ctx, client, macs); err != nil {
			slog.Error("register failed", "err", err)
		}
		return
	}

	if err := actions.Dispatch(ctx, client, resp); err != nil {
		slog.Error("action failed", "err", err)
		halt(1)
	}
}

// setupLogging configures slog to write to /dev/kmsg so messages appear in
// dmesg and over serial console.  Falls back to stderr if kmsg is unavailable.
func setupLogging() {
	kmsg, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0)
	if err != nil {
		// Running outside initramfs (e.g. CI / unit tests) — use stderr.
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(kmsg, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
}

// halt shuts down or panics hard — called when a non-recoverable error occurs.
func halt(code int) {
	slog.Error("fos-agent halting", "code", code)
	os.Exit(code)
}

// reorder moves the element matching primary to the front of the slice.
func reorder(macs []string, primary string) {
	for i, m := range macs {
		if m == primary && i != 0 {
			macs[0], macs[i] = macs[i], macs[0]
			return
		}
	}
}
