// Command usslpctl drives a running USSLP platform from a terminal.
//
// It talks to two surfaces and is explicit about which. Anything a retailer
// would do — change a price, activate a promotion, start a firmware rollout,
// read the SLO — goes through the platform's own API Gateway with an API key,
// exactly as a customer's integration would. Anything about the *runtime* —
// which stores exist in this process, what is on a simulated label's glass,
// cutting a store's WAN link — goes to usslpd's control surface, which no
// distributed deployment has.
//
//	usslpctl status                       what is running, and where
//	usslpctl stores                       per-store mode, controllers, queue depth
//	usslpctl labels --store S             every label with what is on its glass
//	usslpctl price set --store S --sku K --price 1.99
//	usslpctl price batch --store S --file prices.json
//	usslpctl promo activate --id promo-1
//	usslpctl ota start --version 1.5.0
//	usslpctl slo [--store S]              measured latency against the 3 s budget
//	usslpctl watch                        the live event feed
//	usslpctl chaos wan-outage --store S [--restore] [--seconds 20]
//	usslpctl chaos kill-sec --sec SEC
//	usslpctl chaos degrade-link --store S --delay 250ms --loss 5
//
// Every command takes --json to print the raw response instead of a table, so
// the tool composes with jq as well as it reads.
//
// The endpoint defaults to the documented local ports and can be overridden
// with --control and --api, or USSLP_CONTROL_URL and USSLP_API_URL. The API key
// comes from --key or USSLP_API_KEY; when it is absent and the control surface
// is reachable, usslpctl reads the runtime's bootstrap key from it, because a
// tool that cannot be used without first copying a key out of a log is a tool
// nobody uses.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "usslpctl: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(os.Stdout)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "status":
		return cmdStatus(rest)
	case "stores":
		return cmdStores(rest)
	case "labels":
		return cmdLabels(rest)
	case "price":
		return cmdPrice(rest)
	case "promo", "promotion":
		return cmdPromo(rest)
	case "ota":
		return cmdOTA(rest)
	case "slo":
		return cmdSLO(rest)
	case "watch":
		return cmdWatch(rest)
	case "chaos":
		return cmdChaos(rest)
	case "version":
		fmt.Println("usslpctl (part of USSLP)")
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, strings.TrimLeft(`
usslpctl — drive a running USSLP platform

  status                         what is running, and on which ports
  stores                         every store: mode, controllers, upstream queue
  labels --store S [--limit N]   every label, with what is on its glass
  slo [--store S] [--reset]      measured latency against the 3-second budget
  watch [--events N]             stream the platform's live event feed

  price set --store S --sku K --price 1.99 [--was 2.49]
  price batch --store S --file prices.json
  promo activate --id ID [--tenant T]
  ota start --version V [--from V] [--cohorts 1,5,25,100]

  chaos wan-outage --store S [--restore] [--seconds N]
  chaos kill-sec --sec SEC
  chaos degrade-link --store S [--delay 250ms] [--loss 5]

Common flags:
  --control URL   usslpd control surface (default http://127.0.0.1:8079,
                  or USSLP_CONTROL_URL)
  --api URL       API gateway (default http://127.0.0.1:8080, or USSLP_API_URL)
  --key KEY       API key (or USSLP_API_KEY; discovered from --control if unset)
  --tenant T      tenant (default: the runtime's first tenant)
  --json          print the raw JSON response

`, "\n"))
}
