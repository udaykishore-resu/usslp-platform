// Command usslpd runs the whole USSLP platform in one process: the cloud tier,
// the store tier and a simulated label fleet, wired together against one event
// log and one MQTT broker.
//
// It is the deployment shape for development, for a lab, and for a single-store
// or disconnected pilot. The distributed Kubernetes topology in deploy/ is the
// shape for an estate. See the package comment on
// platform/cmd/usslpd/stack for when each is right and what differs between
// them — the short version is that nothing is stubbed here, the partition
// counts are laptop-sized, and the labels are simulated.
//
//	usslpd                                    a store, on the documented ports
//	usslpd --ephemeral                        a temporary store on random ports
//	usslpd --controllers 8 --labels 200       a bigger store
//	usslpd --tenants demo-retail,acme-foods   two tenants in one runtime
//
// Every flag is also an environment variable with the USSLP_ prefix and the
// flag's name upper-cased with dashes replaced by underscores, because the
// same binary runs from a systemd unit that has no command line.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// shutdownGrace bounds the drain.
//
// Twenty seconds is sized against the slowest thing that can be in flight: a
// store-wide fan-out with forty thousand labels left to publish. Stopping
// halfway leaves some shelves on the new promotion and some on the old price,
// which is worse than not having started.
const shutdownGrace = 20 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "usslpd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dataDir     = flag.String("data-dir", env("DATA_DIR", "./data/usslpd"), "where the event log, state stores and PKI live")
		ephemeral   = flag.Bool("ephemeral", envBool("EPHEMERAL", false), "use a temporary data directory, removed on exit, and let the OS choose every port")
		tenantList  = flag.String("tenants", env("TENANTS", "demo-retail"), "comma-separated tenant identifiers")
		stores      = flag.Int("stores", envInt("STORES", stack.DefaultStores), "stores per tenant")
		controllers = flag.Int("controllers", envInt("CONTROLLERS", stack.DefaultControllers), "shelf edge controllers per store")
		labels      = flag.Int("labels", envInt("LABELS", stack.DefaultLabels), "labels per controller")
		seed        = flag.Int64("seed", int64(envInt("SEED", 1)), "fixes every random draw in the edge simulation")
		simSpeed    = flag.Float64("sim-speed", envFloat("SIM_SPEED", 1), "simulated time per real second; must be 1 for latency numbers to mean anything")
		partitions  = flag.Int("partitions", envInt("PARTITIONS", stack.DefaultDevPartitions), "partitions per stream (canon's estate-sized counts are absurd on a laptop)")
		region      = flag.String("region", env("REGION", "local"), "region stamped on every envelope and MQTT topic")
		currency    = flag.String("currency", env("CURRENCY", "USD"), "trading currency of the generated stores")
		logLevel    = flag.String("log-level", env("LOG_LEVEL", "info"), "debug, info, warn or error")
		logFormat   = flag.String("log-format", env("LOG_FORMAT", "text"), "text or json")
		basePort    = flag.Int("base-port", envInt("BASE_PORT", 0), "shift every listener by this many ports, to run two runtimes side by side")
		showJSON    = flag.Bool("json", false, "print the startup banner as JSON instead of a table")
		statusFile  = flag.String("status-file", env("STATUS_FILE", ""),
			"write the status document to this file once the platform is ready")
	)
	flag.Parse()

	var tenants []canon.TenantID
	for _, t := range strings.Split(*tenantList, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tenants = append(tenants, canon.TenantID(t))
		}
	}

	// An ephemeral run gets a temporary directory unless the operator named one
	// explicitly. Without this check the flag's own default would be treated as
	// a choice, and `--ephemeral` would quietly reuse — and then delete —
	// whatever was in ./data/usslpd.
	if *ephemeral && !flagWasSet("data-dir") {
		*dataDir = ""
	}

	cfg := stack.Config{
		DataDir: *dataDir, Ephemeral: *ephemeral, Tenants: tenants,
		Stores: *stores, ControllersPerStore: *controllers, LabelsPerController: *labels,
		Seed: *seed, SimSpeed: *simSpeed, DevPartitions: *partitions,
		Region: *region, Currency: *currency,
		LogLevel: *logLevel, LogFormat: *logFormat,
	}
	if !*ephemeral {
		cfg.Ports = shiftPorts(stack.DefaultPorts(), *basePort)
	}

	st, err := stack.New(cfg)
	if err != nil {
		return err
	}

	// The signal watcher is installed before Start so that a Ctrl-C during a
	// slow start-up drains cleanly rather than leaving a half-provisioned data
	// directory behind.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- st.Start(ctx) }()

	select {
	case sig := <-sigc:
		fmt.Fprintf(os.Stderr, "\nusslpd: %s during start-up; stopping\n", sig)
		cancel()
		<-startErr
		return stopWithin(st)
	case err := <-startErr:
		if err != nil {
			return err
		}
	}

	// The status file exists because obs.NewRuntime writes every service's log
	// to standard output and offers no way to redirect it, so a script that
	// tried to parse `--json` off stdout would be parsing JSON interleaved with
	// log lines. A file is also the better interface for a supervisor: it
	// appears exactly once, when the platform is genuinely ready to take a
	// price change, so waiting for it to parse is waiting for a store that is
	// open for trade.
	if *statusFile != "" {
		if err := writeStatusFile(st, *statusFile); err != nil {
			return err
		}
	}
	if *showJSON {
		if err := printJSON(st); err != nil {
			return err
		}
	} else {
		fmt.Print(banner(st))
	}

	sig := <-sigc
	fmt.Fprintf(os.Stderr, "\nusslpd: %s received; draining\n", sig)
	return stopWithin(st)
}

func stopWithin(st *stack.Stack) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return st.Stop(ctx)
}

// shiftPorts moves every listener by an offset, so two runtimes can share a
// machine without an argument about who owns 8080.
func shiftPorts(p stack.Ports, by int) stack.Ports {
	if by == 0 {
		return p
	}
	shift := func(v int) int {
		if v <= 0 {
			return v
		}
		return v + by
	}
	p.Control, p.APIGateway, p.UIG = shift(p.Control), shift(p.APIGateway), shift(p.UIG)
	p.Label, p.Registry, p.OTA = shift(p.Label), shift(p.Registry), shift(p.OTA)
	p.Pricing, p.Promotion, p.Analytics = shift(p.Pricing), shift(p.Promotion), shift(p.Analytics)
	p.CloudMQTT, p.AdminBase = shift(p.CloudMQTT), shift(p.AdminBase)
	p.StoreMQTTBse, p.StoreAdmnBse = shift(p.StoreMQTTBse), shift(p.StoreAdmnBse)
	return p
}

// banner prints every URL a human needs, and the numbers that say whether the
// run is worth trusting.
func banner(st *stack.Stack) string {
	s := st.Status()
	var b strings.Builder
	line := strings.Repeat("─", 76)
	fmt.Fprintf(&b, "\n%s\n", line)
	fmt.Fprintf(&b, "  USSLP — single-process runtime %s\n", s.Version)
	fmt.Fprintf(&b, "  booted in %d ms · %d tenant(s) · %d store(s) · %d controllers · %d labels\n",
		s.BootMS, len(s.Tenants), s.Stores, s.Controllers, s.Labels)
	fmt.Fprintf(&b, "%s\n\n", line)

	fmt.Fprintf(&b, "  OPERATOR\n")
	row(&b, "console", s.Endpoints["console"])
	row(&b, "API (OpenAPI)", s.Endpoints["openapi"])
	row(&b, "API gateway", s.Endpoints["api-gateway"])
	row(&b, "live event feed", strings.Replace(s.Endpoints["api-gateway"], "http://", "ws://", 1)+"/v1/stream")
	row(&b, "usslpd control", s.Endpoints["control"])
	b.WriteString("\n  SERVICES\n")
	for _, name := range []string{"uig", "label-service", "device-registry", "ota-service",
		"pricing-service", "promotion-service", "analytics-service"} {
		admin := s.AdminSurface[name]
		row(&b, name, fmt.Sprintf("%-28s admin %s", s.Endpoints[name], admin))
	}
	b.WriteString("\n  MESSAGING\n")
	row(&b, "cloud MQTT", s.Endpoints["cloud-mqtt"])

	b.WriteString("\n  STORES\n")
	for _, sv := range st.StoreViews() {
		row(&b, string(sv.StoreID), fmt.Sprintf("mqtt %s · diagnostics %s · %s",
			sv.Broker, sv.Diagnostics, sv.Mode))
	}

	b.WriteString("\n  CREDENTIALS\n")
	names := make([]string, 0, len(s.APIKeys))
	for t := range s.APIKeys {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		row(&b, t+" (owner)", s.APIKeys[t])
	}
	row(&b, "shopify HMAC key", s.Endpoints["shopify-hmac-key"])
	row(&b, "shopify ingest", s.Endpoints["shopify-ingest-url"])

	fmt.Fprintf(&b, "\n  Data in %s%s. Streams provisioned with %d partitions each; "+
		"prices attested with %s.\n",
		s.DataDir, map[bool]string{true: " (temporary, removed on exit)", false: ""}[s.Ephemeral],
		s.Partitions, s.PriceKeyID)
	fmt.Fprintf(&b, "  Try:  tools/usslpctl status   |   tools/usslpctl price set --store %s --sku <sku> --price 1.99\n",
		firstStore(st))
	fmt.Fprintf(&b, "%s\n\n", line)
	return b.String()
}

func row(b *strings.Builder, k, v string) {
	fmt.Fprintf(b, "    %-20s %s\n", k, v)
}

func firstStore(st *stack.Stack) string {
	stores := st.Stores()
	if len(stores) == 0 {
		return "<store>"
	}
	return string(stores[0].ID)
}

// writeStatusFile writes the status document atomically, so a reader never
// sees a half-written file and mistakes it for a malformed one.
func writeStatusFile(st *stack.Stack, path string) error {
	body, err := json.MarshalIndent(st.Status(), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing the status file %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("installing the status file %s: %w", path, err)
	}
	return nil
}

func printJSON(st *stack.Stack) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(st.Status())
}

// ---------------------------------------------------------------------------
// Configuration helpers
//
// Every flag falls back to USSLP_<NAME>, which is what lets the same binary be
// driven by a systemd unit's Environment= lines with no command line at all
// (interface contract §8).
// ---------------------------------------------------------------------------

// flagWasSet reports whether a flag appeared on the command line, as opposed to
// carrying its default.
func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func env(name, def string) string {
	if v, ok := os.LookupEnv("USSLP_" + name); ok && v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, ok := os.LookupEnv("USSLP_" + name); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v, ok := os.LookupEnv("USSLP_" + name); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(name string, def bool) bool {
	if v, ok := os.LookupEnv("USSLP_" + name); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}
