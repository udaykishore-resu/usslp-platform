package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func newContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 120*time.Second)
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := newContext()
	defer cancel()

	var raw json.RawMessage
	if err := e.getJSON(ctx, e.control+"/v1/status", &raw); err != nil {
		return err
	}
	var s struct {
		Version     string            `json:"version"`
		UptimeS     int64             `json:"uptime_seconds"`
		BootMS      int64             `json:"boot_ms"`
		DataDir     string            `json:"data_dir"`
		Ephemeral   bool              `json:"ephemeral"`
		Tenants     []string          `json:"tenants"`
		Stores      int               `json:"stores"`
		Controllers int               `json:"controllers"`
		Labels      int               `json:"labels"`
		Partitions  int               `json:"stream_partitions"`
		PriceKeyID  string            `json:"price_authority_key_id"`
		Endpoints   map[string]string `json:"endpoints"`
		Admin       map[string]string `json:"admin"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	return e.emit(raw, func() {
		fmt.Printf("USSLP %s — up %s, booted in %d ms\n",
			s.Version, (time.Duration(s.UptimeS) * time.Second).String(), s.BootMS)
		fmt.Printf("%d tenant(s), %d store(s), %d controllers, %d labels\n",
			len(s.Tenants), s.Stores, s.Controllers, s.Labels)
		fmt.Printf("streams at %d partitions each; prices attested with %s\n",
			s.Partitions, s.PriceKeyID)
		fmt.Printf("data in %s%s\n", s.DataDir,
			map[bool]string{true: " (temporary)", false: ""}[s.Ephemeral])

		heading("ENDPOINTS")
		w := newTable()
		for _, k := range sortedKeys(s.Endpoints) {
			fmt.Fprintf(w, "  %s\t%s\n", k, s.Endpoints[k])
		}
		w.Flush()

		heading("ADMIN (metrics, healthz, readyz)")
		w = newTable()
		for _, k := range sortedKeys(s.Admin) {
			fmt.Fprintf(w, "  %s\t%s\n", k, s.Admin[k])
		}
		w.Flush()
		fmt.Println()
	})
}

// ---------------------------------------------------------------------------
// stores
// ---------------------------------------------------------------------------

type storeView struct {
	StoreID     string `json:"store_id"`
	TenantID    string `json:"tenant_id"`
	Mode        string `json:"mode"`
	Broker      string `json:"broker"`
	Diagnostics string `json:"diagnostics"`
	Labels      int    `json:"labels"`
	Controllers []struct {
		SECID             string `json:"sec_id"`
		Labels            int    `json:"labels"`
		Online            bool   `json:"online"`
		Applied           uint64 `json:"updates_applied"`
		AttestationFailed uint64 `json:"attestation_failures"`
		DeliveryFailed    uint64 `json:"delivery_failures"`
		MeshJoined        int    `json:"mesh_nodes_joined"`
		MeshNodes         int    `json:"mesh_nodes"`
		ChannelUtil       string `json:"channel_utilisation"`
	} `json:"controllers"`
	Queue struct {
		Depth int   `json:"depth"`
		Bytes int64 `json:"bytes"`
	} `json:"upstream_queue"`
	Link struct {
		Cut         bool  `json:"cut"`
		DelayMS     int64 `json:"injected_delay_ms"`
		LossPercent int64 `json:"injected_loss_percent"`
	} `json:"wan_link"`
}

func cmdStores(args []string) error {
	fs := flag.NewFlagSet("stores", flag.ContinueOnError)
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := newContext()
	defer cancel()

	var raw json.RawMessage
	if err := e.getJSON(ctx, e.control+"/v1/stores", &raw); err != nil {
		return err
	}
	var stores []storeView
	if err := json.Unmarshal(raw, &stores); err != nil {
		return err
	}
	return e.emit(raw, func() {
		w := newTable()
		fmt.Fprintln(w, "STORE\tTENANT\tMODE\tLABELS\tSECS\tQUEUE\tWAN\tBROKER")
		for _, s := range stores {
			online := 0
			for _, c := range s.Controllers {
				if c.Online {
					online++
				}
			}
			wan := "up"
			switch {
			case s.Link.Cut:
				wan = "CUT"
			case s.Link.DelayMS > 0 || s.Link.LossPercent > 0:
				wan = fmt.Sprintf("degraded +%dms/%d%%", s.Link.DelayMS, s.Link.LossPercent)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d/%d\t%d\t%s\t%s\n",
				s.StoreID, s.TenantID, s.Mode, s.Labels,
				online, len(s.Controllers), s.Queue.Depth, wan, s.Broker)
		}
		w.Flush()

		for _, s := range stores {
			heading("CONTROLLERS — " + s.StoreID)
			w := newTable()
			fmt.Fprintln(w, "  SEC\tONLINE\tLABELS\tAPPLIED\tATTEST FAIL\tDELIVERY FAIL\tMESH\tCHANNEL")
			for _, c := range s.Controllers {
				fmt.Fprintf(w, "  %s\t%v\t%d\t%d\t%d\t%d\t%d/%d\t%s\n",
					c.SECID, c.Online, c.Labels, c.Applied, c.AttestationFailed,
					c.DeliveryFailed, c.MeshJoined, c.MeshNodes, c.ChannelUtil)
			}
			w.Flush()
		}
		fmt.Println()
	})
}

// ---------------------------------------------------------------------------
// labels
// ---------------------------------------------------------------------------

func cmdLabels(args []string) error {
	fs := flag.NewFlagSet("labels", flag.ContinueOnError)
	store := fs.String("store", "", "store identifier")
	limit := fs.Int("limit", 20, "how many labels to show; 0 for all")
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := newContext()
	defer cancel()

	id, err := e.defaultStore(ctx, *store)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/v1/stores/%s/labels?limit=%d", e.control, id, *limit)
	var raw json.RawMessage
	if err := e.getJSON(ctx, url, &raw); err != nil {
		return err
	}
	var labels []struct {
		LabelID     string `json:"label_id"`
		SECID       string `json:"sec_id"`
		SKU         string `json:"sku"`
		Controller  string `json:"controller_price"`
		Glass       string `json:"displayed_price"`
		Sequence    int64  `json:"sequence"`
		Attested    bool   `json:"attested"`
		KeyID       string `json:"attestation_key_id"`
		Refreshes   int64  `json:"panel_refreshes"`
		BatteryPct  int    `json:"battery_percent"`
		PromotionID string `json:"promotion_id"`
		LastError   string `json:"last_error"`
	}
	if err := json.Unmarshal(raw, &labels); err != nil {
		return err
	}
	return e.emit(raw, func() {
		w := newTable()
		fmt.Fprintln(w, "LABEL\tSKU\tON THE GLASS\tSEQ\tATTESTED\tREFRESHES\tBATT\tPROMO")
		for _, l := range labels {
			promo := l.PromotionID
			if promo == "" {
				promo = "-"
			}
			attested := "no"
			if l.Attested {
				attested = "yes"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%d\t%d%%\t%s\n",
				l.LabelID, l.SKU, l.Glass, l.Sequence, attested, l.Refreshes, l.BatteryPct, promo)
		}
		w.Flush()
		if len(labels) == 0 {
			fmt.Println("no labels")
		}
	})
}

// ---------------------------------------------------------------------------
// price
// ---------------------------------------------------------------------------

func cmdPrice(args []string) error {
	if len(args) == 0 {
		return errors.New("price needs a subcommand: set or batch")
	}
	switch args[0] {
	case "set":
		return cmdPriceSet(args[1:])
	case "batch":
		return cmdPriceBatch(args[1:])
	default:
		return fmt.Errorf("unknown price subcommand %q; try set or batch", args[0])
	}
}

func cmdPriceSet(args []string) error {
	fs := flag.NewFlagSet("price set", flag.ContinueOnError)
	store := fs.String("store", "", "store identifier")
	sku := fs.String("sku", "", "product code")
	price := fs.String("price", "", "new price, e.g. 1.99")
	was := fs.String("was", "", "struck-through comparison price")
	currency := fs.String("currency", "USD", "ISO 4217 currency code")
	reason := fs.String("reason", "usslpctl", "why the price changed, for the audit trail")
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *sku == "" || *price == "" {
		return errors.New("price set needs --sku and --price")
	}
	ctx, cancel := newContext()
	defer cancel()
	if err := e.resolve(ctx); err != nil {
		return err
	}
	id, err := e.defaultStore(ctx, *store)
	if err != nil {
		return err
	}
	amount, err := minorUnits(*price, *currency)
	if err != nil {
		return err
	}
	// The gateway rejects an unknown field rather than ignoring it, which is
	// the right choice for a public API — a typo in a field name that is
	// silently dropped is a price change that silently does not happen — so the
	// body is exactly apigw.PriceChangeRequest. Who initiated the change comes
	// from the credential, not from the caller's claim about itself.
	body := map[string]any{
		"store_id": id, "sku": *sku,
		"price":        map[string]any{"amount_minor": amount, "currency": *currency},
		"effective_at": time.Now().UTC().Format(time.RFC3339Nano),
		"reason":       *reason,
	}
	if *was != "" {
		w, err := minorUnits(*was, *currency)
		if err != nil {
			return err
		}
		body["was_price"] = map[string]any{"amount_minor": w, "currency": *currency}
	}

	started := time.Now()
	var raw json.RawMessage
	if err := e.apiPost(ctx, "/v1/prices", body, &raw); err != nil {
		return err
	}
	return e.emit(raw, func() {
		fmt.Printf("accepted in %s: %s in %s is now %s %s\n",
			time.Since(started).Round(time.Millisecond), *sku, id, *price, *currency)
		fmt.Println("the platform has taken durable responsibility for the change; " +
			"watch it reach the glass with:  usslpctl labels --store " + id)
	})
}

func cmdPriceBatch(args []string) error {
	fs := flag.NewFlagSet("price batch", flag.ContinueOnError)
	file := fs.String("file", "", "JSON file: an array of {sku, price, was_price?}")
	store := fs.String("store", "", "store identifier")
	currency := fs.String("currency", "USD", "ISO 4217 currency code")
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *file == "" {
		return errors.New("price batch needs --file")
	}
	ctx, cancel := newContext()
	defer cancel()
	if err := e.resolve(ctx); err != nil {
		return err
	}
	id, err := e.defaultStore(ctx, *store)
	if err != nil {
		return err
	}

	body, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *file, err)
	}
	var rows []struct {
		SKU   string `json:"sku"`
		Price string `json:"price"`
		Was   string `json:"was_price,omitempty"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return fmt.Errorf("%s is not an array of {sku, price}: %w", *file, err)
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		amount, err := minorUnits(r.Price, *currency)
		if err != nil {
			return fmt.Errorf("%s: %w", r.SKU, err)
		}
		item := map[string]any{
			"store_id": id, "sku": r.SKU,
			"price":        map[string]any{"amount_minor": amount, "currency": *currency},
			"effective_at": time.Now().UTC().Format(time.RFC3339Nano),
			"reason":       "usslpctl batch", "initiated_by": "usslpctl",
		}
		if r.Was != "" {
			w, err := minorUnits(r.Was, *currency)
			if err != nil {
				return fmt.Errorf("%s was_price: %w", r.SKU, err)
			}
			item["was_price"] = map[string]any{"amount_minor": w, "currency": *currency}
		}
		items = append(items, item)
	}

	started := time.Now()
	var raw json.RawMessage
	// No tenant_id in the body: the Label Service takes it from the
	// X-USSLP-Tenant header the gateway sets from the authenticated credential,
	// and it refuses a body that tries to name one. That refusal is the right
	// behaviour and worth not working around — a caller that could put a tenant
	// in a batch body could reprice somebody else's estate.
	if err := e.apiPost(ctx, "/v1/prices:batch", map[string]any{
		"items": items, "initiated_by": "usslpctl",
	}, &raw); err != nil {
		return err
	}
	var report struct {
		Requested int `json:"requested"`
		Resolved  int `json:"resolved"`
		Applied   int `json:"applied"`
		Scheduled int `json:"scheduled"`
		Rejected  int `json:"rejected"`
		Failed    int `json:"failed"`
	}
	_ = json.Unmarshal(raw, &report)
	return e.emit(raw, func() {
		fmt.Printf("%d items -> %d labels: %d applied, %d scheduled, %d rejected, %d failed, in %s\n",
			report.Requested, report.Resolved, report.Applied, report.Scheduled,
			report.Rejected, report.Failed, time.Since(started).Round(time.Millisecond))
	})
}

// minorUnits converts a decimal string to minor units without ever touching a
// float, because a price that drifts by a cent between the till and the shelf
// is a weights-and-measures problem rather than a rounding curiosity.
func minorUnits(s, currency string) (int64, error) {
	exp := 2
	switch strings.ToUpper(currency) {
	case "JPY", "KRW", "VND", "CLP", "ISK":
		exp = 0
	case "BHD", "KWD", "OMR", "TND", "JOD":
		exp = 3
	}
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if !hasFrac {
		frac = ""
	}
	if len(frac) > exp {
		return 0, fmt.Errorf("%q has more than %d decimal places for %s", s, exp, currency)
	}
	for len(frac) < exp {
		frac += "0"
	}
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a price: %w", s, err)
	}
	if neg {
		n = -n
	}
	return n, nil
}
