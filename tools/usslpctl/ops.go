package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// promo
// ---------------------------------------------------------------------------

func cmdPromo(args []string) error {
	if len(args) == 0 {
		return errors.New("promo needs a subcommand: activate or list")
	}
	switch args[0] {
	case "activate":
		return cmdPromoActivate(args[1:])
	case "list":
		return cmdPromoList(args[1:])
	default:
		return fmt.Errorf("unknown promo subcommand %q; try activate or list", args[0])
	}
}

func cmdPromoActivate(args []string) error {
	fs := flag.NewFlagSet("promo activate", flag.ContinueOnError)
	id := fs.String("id", "", "promotion identifier")
	by := fs.String("by", "usslpctl", "who activated it, for the audit trail")
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *id == "" {
		return errors.New("promo activate needs --id")
	}
	ctx, cancel := newContext()
	defer cancel()
	if err := e.resolve(ctx); err != nil {
		return err
	}
	started := time.Now()
	var raw json.RawMessage
	if err := e.apiPost(ctx, "/v1/promotions/"+*id+"/activate",
		map[string]any{"by": *by}, &raw); err != nil {
		return err
	}
	return e.emit(raw, func() {
		var rec struct {
			Rule struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"rule"`
			State string `json:"state"`
		}
		_ = json.Unmarshal(raw, &rec)
		fmt.Printf("promotion %s (%s, %s) is %s — accepted in %s\n",
			rec.Rule.ID, rec.Rule.Name, rec.Rule.Type, rec.State,
			time.Since(started).Round(time.Millisecond))
		fmt.Println("the fan-out runs on the promotion-events stream; " +
			"watch the shelves with:  usslpctl labels")
	})
}

func cmdPromoList(args []string) error {
	fs := flag.NewFlagSet("promo list", flag.ContinueOnError)
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := newContext()
	defer cancel()
	if err := e.resolve(ctx); err != nil {
		return err
	}
	var raw json.RawMessage
	if err := e.apiGet(ctx, "/v1/promotions", &raw); err != nil {
		return err
	}
	return e.emit(raw, func() {
		var out struct {
			Promotions []struct {
				Rule struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"rule"`
				State string `json:"state"`
			} `json:"promotions"`
		}
		_ = json.Unmarshal(raw, &out)
		w := newTable()
		fmt.Fprintln(w, "PROMOTION\tNAME\tTYPE\tSTATE")
		for _, p := range out.Promotions {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Rule.ID, p.Rule.Name, p.Rule.Type, p.State)
		}
		w.Flush()
	})
}

// ---------------------------------------------------------------------------
// ota
// ---------------------------------------------------------------------------

func cmdOTA(args []string) error {
	if len(args) == 0 {
		return errors.New("ota needs a subcommand: start or list")
	}
	switch args[0] {
	case "start":
		return cmdOTAStart(args[1:])
	case "list":
		return cmdOTAList(args[1:])
	default:
		return fmt.Errorf("unknown ota subcommand %q; try start or list", args[0])
	}
}

func cmdOTAStart(args []string) error {
	fs := flag.NewFlagSet("ota start", flag.ContinueOnError)
	artifact := fs.String("artifact", "", "artifact id of an already-uploaded image")
	version := fs.String("version", "", "target firmware version")
	from := fs.String("from", "", "restrict the rollout to devices on this version")
	cohorts := fs.String("cohorts", "1,5,25,100", "cohort percentages")
	store := fs.String("store", "", "restrict the rollout to one store")
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *artifact == "" {
		return errors.New("ota start needs --artifact; upload a signed image first " +
			"(POST /v1/firmware on the OTA service) — usslpctl deliberately cannot sign firmware, " +
			"because a tool that could would be a tool that could roll out anything")
	}
	ctx, cancel := newContext()
	defer cancel()
	if err := e.resolve(ctx); err != nil {
		return err
	}
	spec := map[string]any{
		"tenant_id": e.tenant, "artifact_id": *artifact,
		"cohort_percentages": parseInts(*cohorts),
		"created_by":         "usslpctl", "start": true,
	}
	if *from != "" {
		spec["from_version"] = *from
	}
	if *store != "" {
		spec["stores"] = []string{*store}
	}
	var raw json.RawMessage
	if err := e.apiPost(ctx, "/v1/ota/jobs", spec, &raw); err != nil {
		return err
	}
	return e.emit(raw, func() {
		var job struct {
			JobID     string `json:"job_id"`
			ToVersion string `json:"to_version"`
			State     string `json:"state"`
			Cohorts   []int  `json:"cohort_percentages"`
		}
		_ = json.Unmarshal(raw, &job)
		fmt.Printf("rollout %s started: %s -> %s, cohorts %v, state %s\n",
			job.JobID, orDash(*from), orDash(job.ToVersion), job.Cohorts, job.State)
		fmt.Println("the rollout halts itself if a cohort's health gates fail; " +
			"follow it with:  usslpctl ota list")
		_ = version
	})
}

func cmdOTAList(args []string) error {
	fs := flag.NewFlagSet("ota list", flag.ContinueOnError)
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := newContext()
	defer cancel()
	if err := e.resolve(ctx); err != nil {
		return err
	}
	var raw json.RawMessage
	if err := e.apiGet(ctx, "/v1/ota/jobs", &raw); err != nil {
		return err
	}
	return e.emit(raw, func() {
		var out struct {
			Jobs []struct {
				JobID       string `json:"job_id"`
				ToVersion   string `json:"to_version"`
				State       string `json:"state"`
				CurrentWave int    `json:"current_wave"`
				Cohorts     []int  `json:"cohort_percentages"`
				HaltReason  string `json:"halt_reason"`
			} `json:"jobs"`
		}
		_ = json.Unmarshal(raw, &out)
		w := newTable()
		fmt.Fprintln(w, "JOB\tTO\tSTATE\tWAVE\tCOHORTS\tREASON")
		for _, j := range out.Jobs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%v\t%s\n",
				j.JobID, j.ToVersion, j.State, j.CurrentWave, j.Cohorts, orDash(j.HaltReason))
		}
		w.Flush()
	})
}

func parseInts(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(part, "%d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---------------------------------------------------------------------------
// slo
// ---------------------------------------------------------------------------

func cmdSLO(args []string) error {
	fs := flag.NewFlagSet("slo", flag.ContinueOnError)
	store := fs.String("store", "", "store identifier; omit for every store")
	reset := fs.Bool("reset", false, "discard the measured deliveries and start a fresh window")
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := newContext()
	defer cancel()

	if *reset {
		var out struct {
			Discarded int `json:"discarded"`
		}
		if err := e.controlPost(ctx, "/v1/slo/reset", map[string]any{}, &out); err != nil {
			return err
		}
		fmt.Printf("measurement window reset; %d earlier deliveries discarded\n", out.Discarded)
		return nil
	}

	id := *store
	if id == "" {
		id = "all"
	}
	var raw json.RawMessage
	if err := e.getJSON(ctx, fmt.Sprintf("%s/v1/stores/%s/slo", e.control, id), &raw); err != nil {
		return err
	}
	var out struct {
		Budget []struct {
			Hop      string `json:"hop"`
			BudgetMS int64  `json:"budget_ms"`
			What     string `json:"what"`
		} `json:"budget"`
		Measured struct {
			Deliveries int     `json:"deliveries"`
			Failed     int     `json:"failed"`
			BudgetMS   int64   `json:"budget_ms"`
			WithinSLO  int     `json:"within_slo"`
			Attainment string  `json:"attainment"`
			P50MS      int64   `json:"p50_ms"`
			P95MS      int64   `json:"p95_ms"`
			P99MS      int64   `json:"p99_ms"`
			MaxMS      int64   `json:"max_ms"`
			MeanMS     int64   `json:"mean_ms"`
			Partial    string  `json:"partial_refresh_share"`
			MeshHops   float64 `json:"mean_mesh_hops"`
			DriftMS    int64   `json:"clock_drift_ms"`
			Hops       []struct {
				Hop        string `json:"hop"`
				BudgetMS   int64  `json:"budget_ms"`
				P50MS      int64  `json:"p50_ms"`
				P99MS      int64  `json:"p99_ms"`
				Within     bool   `json:"p99_within_budget"`
				Observable bool   `json:"separately_measured"`
				Note       string `json:"note"`
			} `json:"measured_hops"`
		} `json:"measured"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	return e.emit(raw, func() {
		m := out.Measured
		fmt.Printf("\n%d deliveries measured, %d failed; %s inside the %d ms budget\n",
			m.Deliveries, m.Failed, m.Attainment, m.BudgetMS)
		fmt.Printf("p50 %d ms   p95 %d ms   p99 %d ms   max %d ms   mean %d ms\n",
			m.P50MS, m.P95MS, m.P99MS, m.MaxMS, m.MeanMS)
		fmt.Printf("%s of refreshes were partial waveforms; %.2f mesh hops on average\n",
			m.Partial, m.MeshHops)
		// The error bar on every number above. The edge simulation's clock is
		// paced against real time and can only fall behind, never catch up, so
		// a large drift means the platform's own latencies are under-reported
		// by roughly that much — and a negative p50 means it is under-reporting
		// by more than the latency itself.
		if m.DriftMS >= 50 {
			fmt.Printf("simulated clock is %d ms behind the wall clock; "+
				"the latencies above are under-reported by about that much\n", m.DriftMS)
		}

		heading("THE CONTRACT'S BUDGET (INTERFACE-CONTRACTS §4)")
		w := newTable()
		for _, b := range out.Budget {
			fmt.Fprintf(w, "  %s\t%d ms\t%s\n", b.Hop, b.BudgetMS, b.What)
		}
		w.Flush()

		heading("MEASURED")
		w = newTable()
		fmt.Fprintln(w, "  HOP\tBUDGET\tP50\tP99\tOK\tHOW")
		for _, h := range m.Hops {
			how := "measured"
			if !h.Observable {
				how = "residual"
			}
			ok := "yes"
			if !h.Within {
				ok = "NO"
			}
			fmt.Fprintf(w, "  %s\t%d ms\t%d ms\t%d ms\t%s\t%s\n",
				h.Hop, h.BudgetMS, h.P50MS, h.P99MS, ok, how)
		}
		w.Flush()
		fmt.Println()
	})
}

// ---------------------------------------------------------------------------
// chaos
// ---------------------------------------------------------------------------

func cmdChaos(args []string) error {
	if len(args) == 0 {
		return errors.New("chaos needs a subcommand: wan-outage, kill-sec, degrade-link or kill-relay")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("chaos "+sub, flag.ContinueOnError)
	store := fs.String("store", "", "store identifier; omit for every store")
	sec := fs.String("sec", "", "controller identifier")
	node := fs.String("node", "", "mesh node identifier; omit to pick a relay")
	restore := fs.Bool("restore", false, "reverse a wan-outage")
	seconds := fs.Int("seconds", 0, "restore the link automatically after this many seconds")
	delay := fs.Duration("delay", 0, "one-way latency to inject")
	loss := fs.Int("loss", 0, "packet loss percentage to inject")
	e, err := parse(fs, rest)
	if err != nil {
		return err
	}
	ctx, cancel := newContext()
	defer cancel()

	body := map[string]any{
		"store_id": *store, "sec_id": *sec, "node": *node,
		"restore": *restore, "seconds": *seconds,
		"delay_ms": int(delay.Milliseconds()), "loss_percent": *loss,
	}
	var path string
	switch sub {
	case "wan-outage":
		path = "/v1/chaos/wan-outage"
	case "kill-sec":
		if *sec == "" {
			return errors.New("chaos kill-sec needs --sec")
		}
		path = "/v1/chaos/kill-sec"
	case "degrade-link":
		path = "/v1/chaos/degrade-link"
	case "kill-relay":
		path = "/v1/chaos/kill-relay"
	default:
		return fmt.Errorf("unknown chaos subcommand %q", sub)
	}

	var raw json.RawMessage
	if err := e.controlPost(ctx, path, body, &raw); err != nil {
		return err
	}
	return e.emit(raw, func() {
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		if action, ok := out["action"].(string); ok {
			fmt.Printf("%s: %s\n", sub, action)
		}
		if note, ok := out["note"].(string); ok {
			fmt.Println(note)
		}
		if stores, ok := out["stores"].([]any); ok {
			w := newTable()
			fmt.Fprintln(w, "  STORE\tMODE\tWAN")
			for _, s := range stores {
				m, _ := s.(map[string]any)
				link, _ := m["wan_link"].(map[string]any)
				state := "up"
				if cut, _ := link["cut"].(bool); cut {
					state = "CUT"
				}
				fmt.Fprintf(w, "  %v\t%v\t%s\n", m["store_id"], m["mode"], state)
			}
			w.Flush()
		}
		if *seconds > 0 && !*restore {
			fmt.Printf("the link will be restored automatically in %ds\n", *seconds)
		}
	})
}

// ---------------------------------------------------------------------------
// watch
// ---------------------------------------------------------------------------

// cmdWatch streams the API gateway's WebSocket event feed to the terminal.
//
// The handshake and frame decoding are done here rather than with a library
// because the repository has no external dependencies, and the subset needed to
// *read* a server's text frames is small: an HTTP upgrade, then a loop over
// unmasked frames. Anything the server sends that is not a text or close frame
// is skipped rather than guessed at.
func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	limit := fs.Int("events", 0, "stop after this many events; 0 means run until interrupted")
	e, err := parse(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := newContext()
	defer cancel()
	if err := e.resolve(ctx); err != nil {
		return err
	}

	u, err := url.Parse(e.api)
	if err != nil {
		return fmt.Errorf("--api is not a URL: %w", err)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), map[string]string{"https": "443"}[u.Scheme])
		if strings.HasSuffix(host, ":") {
			host += "80"
		}
	}
	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", host, err)
	}
	defer conn.Close()

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req := "GET /v1/stream HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n"
	if e.key != "" {
		req += "Authorization: Bearer " + e.key + "\r\n"
	}
	if e.tenant != "" {
		req += "X-USSLP-Tenant: " + e.tenant + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fmt.Errorf("reading the upgrade response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body := make([]byte, 512)
		n, _ := br.Read(body)
		return fmt.Errorf("the gateway refused the stream: %s: %s", resp.Status, strings.TrimSpace(string(body[:n])))
	}
	// The accept value is checked because skipping it is how a client ends up
	// talking WebSocket frames at something that never agreed to speak them.
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if want := base64.StdEncoding.EncodeToString(sum[:]); resp.Header.Get("Sec-WebSocket-Accept") != want {
		return errors.New("the gateway's Sec-WebSocket-Accept does not match the key we sent")
	}

	fmt.Println("streaming " + e.api + "/v1/stream as tenant " + e.tenant + " — Ctrl-C to stop")
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() { <-sigc; conn.Close() }()

	seen := 0
	w := newTable()
	fmt.Fprintln(w, "TIME\tEVENT\tSTORE\tAGGREGATE\tDETAIL")
	w.Flush()
	for {
		payload, opcode, err := readFrame(br)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		switch opcode {
		case 0x8: // close
			return nil
		case 0x9: // ping; the server does not require a pong to keep sending
			continue
		case 0x1, 0x0: // text or continuation
		default:
			continue
		}
		if e.asJSON {
			fmt.Println(strings.TrimSpace(string(payload)))
		} else {
			printEvent(payload)
		}
		seen++
		if *limit > 0 && seen >= *limit {
			return nil
		}
	}
}

// readFrame reads one server-to-client WebSocket frame. Server frames are never
// masked, so the masking path is deliberately absent rather than implemented
// and untested.
func readFrame(br *bufio.Reader) ([]byte, byte, error) {
	var head [2]byte
	if _, err := readFull(br, head[:]); err != nil {
		return nil, 0, err
	}
	opcode := head[0] & 0x0f
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := readFull(br, ext[:]); err != nil {
			return nil, 0, err
		}
		length = uint64(ext[0])<<8 | uint64(ext[1])
	case 127:
		var ext [8]byte
		if _, err := readFull(br, ext[:]); err != nil {
			return nil, 0, err
		}
		length = 0
		for _, b := range ext {
			length = length<<8 | uint64(b)
		}
	}
	if length > 32<<20 {
		return nil, 0, fmt.Errorf("a %d-byte frame is larger than anything this stream sends", length)
	}
	var mask [4]byte
	if masked {
		if _, err := readFull(br, mask[:]); err != nil {
			return nil, 0, err
		}
	}
	payload := make([]byte, length)
	if _, err := readFull(br, payload); err != nil {
		return nil, 0, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, opcode, nil
}

func readFull(br *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := br.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// printEvent renders one envelope as a line a person can scan.
func printEvent(payload []byte) {
	// The gateway's frames are tagged: an "event" carries an envelope, and the
	// control frames — the subscription acknowledgement, the periodic keepalive
	// — carry a message instead. Printing the control frames as blank rows is
	// what the first version of this did, and it made a working stream look
	// broken.
	var env struct {
		Type        string          `json:"type"`
		EventType   string          `json:"event_type"`
		AggregateID string          `json:"aggregate_id"`
		StoreID     string          `json:"store_id"`
		RecordedAt  time.Time       `json:"recorded_at"`
		Payload     json.RawMessage `json:"payload"`
		Message     string          `json:"message"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		fmt.Println(strings.TrimSpace(string(payload)))
		return
	}
	if env.EventType == "" {
		if env.Message != "" {
			fmt.Printf("%-8s  %s: %s\n", "", env.Type, env.Message)
		}
		return
	}
	detail := ""
	var body struct {
		Price     json.RawMessage `json:"price"`
		LatencyMS int64           `json:"latency_ms"`
		SKU       string          `json:"sku"`
		Mode      string          `json:"mode"`
		Reason    string          `json:"reason"`
		Sequence  int64           `json:"sequence"`
		MeshHops  int             `json:"mesh_hops"`
		RefreshMS int             `json:"refresh_ms"`
	}
	if json.Unmarshal(env.Payload, &body) == nil {
		var parts []string
		if body.SKU != "" {
			parts = append(parts, body.SKU)
		}
		if len(body.Price) > 0 {
			var p struct {
				Display string `json:"display"`
			}
			if json.Unmarshal(body.Price, &p) == nil && p.Display != "" {
				parts = append(parts, p.Display)
			}
		}
		if body.LatencyMS > 0 {
			parts = append(parts, fmt.Sprintf("%dms end to end", body.LatencyMS))
		}
		if body.RefreshMS > 0 {
			parts = append(parts, fmt.Sprintf("%dms waveform", body.RefreshMS))
		}
		if body.MeshHops > 0 {
			parts = append(parts, fmt.Sprintf("%d hop(s)", body.MeshHops))
		}
		if body.Sequence > 0 {
			parts = append(parts, fmt.Sprintf("seq %d", body.Sequence))
		}
		if body.Mode != "" {
			parts = append(parts, body.Mode)
		}
		if body.Reason != "" {
			parts = append(parts, body.Reason)
		}
		detail = strings.Join(parts, "  ")
	}
	fmt.Printf("%s  %-28s %-22s %-38s %s\n",
		env.RecordedAt.Format(time.TimeOnly), env.EventType,
		env.StoreID, env.AggregateID, detail)
}
