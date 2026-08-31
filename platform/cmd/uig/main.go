// Command uig is the Universal Integration Gateway: the component that makes
// USSLP universal.
//
// Every other electronic-shelf-label platform ships bespoke middleware per POS
// vendor, which is why a retailer running a custom or legacy ERP cannot buy
// ESLs at all. The UIG replaces that with one pipeline and a protocol-adapter
// seam: Shopify's webhook, a Square catalogue event, an NCR item-price message
// in XML or JSON, a SAP PRICAT IDoc, an Oracle Retail SOAP envelope, a
// Lightspeed item update, a Clover object reference that has to be fetched
// back, and a fixed-width file an AS/400 wrote at two in the morning all arrive
// downstream as the same canon.PriceChangeRequested.
//
// The gateway owns 50ms of the platform's 3-second price path
// (docs/architecture/INTERFACE-CONTRACTS.md §4). It spends them on
// authentication, deduplication, parsing, normalisation, validation, store
// enrichment and a durable publish — and it acknowledges the caller the instant
// the change is durable, because a POS that is kept waiting retries, and a
// retry is more work for everyone than a fast 202.
//
// Run it with:
//
//	USSLP_UIG_BINDINGS_FILE=/etc/usslp/bindings.json \
//	USSLP_UIG_OPERATOR_TOKEN_FILE=/run/secrets/uig-operator \
//	uig
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		// Configuration errors go to stderr and exit non-zero before any
		// observability exists: there is nowhere else for them to go, and a
		// process that starts with half a configuration is worse than one that
		// does not start at all.
		fmt.Fprintln(os.Stderr, "uig: "+err.Error())
		os.Exit(2)
	}
	svc, err := NewService(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "uig: "+err.Error())
		os.Exit(1)
	}
	svc.Runtime().Log.Info("uig: configured", describeConfig(cfg)...)

	// The signal watcher is started before Serve so that a SIGTERM arriving
	// during start-up still drains cleanly rather than killing deliveries that
	// are already in flight.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)

	serveErr := make(chan error, 1)
	go func() { serveErr <- svc.Serve() }()

	select {
	case sig := <-sigc:
		svc.Runtime().Log.Info("uig: shutdown signal received", "signal", sig.String())
		// Kubernetes sends SIGTERM and then waits
		// terminationGracePeriodSeconds before SIGKILL, so this grace must be
		// shorter than the one in the Deployment or draining achieves nothing.
		svc.Shutdown(cfg.ShutdownGrace)
	case err := <-serveErr:
		if err != nil {
			svc.Runtime().Log.Error("uig: serving failed", "error", err)
			svc.Shutdown(cfg.ShutdownGrace)
			os.Exit(1)
		}
		svc.Shutdown(cfg.ShutdownGrace)
	}
}
