package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// endpoints are the two surfaces usslpctl talks to. Keeping them apart in the
// type, rather than hiding them behind one "server" flag, is deliberate: the
// distinction between "what a retailer can do" and "what only this runtime can
// do" is the honest one, and a tool that blurred it would make usslpd look like
// the product.
type endpoints struct {
	control string
	api     string
	key     string
	tenant  string
	asJSON  bool
	client  *http.Client
}

// commonFlags installs the flags every command takes.
func commonFlags(fs *flag.FlagSet) *endpoints {
	e := &endpoints{client: &http.Client{Timeout: 60 * time.Second}}
	fs.StringVar(&e.control, "control", env("USSLP_CONTROL_URL", "http://127.0.0.1:8079"),
		"usslpd control surface")
	fs.StringVar(&e.api, "api", env("USSLP_API_URL", "http://127.0.0.1:8080"),
		"USSLP API gateway")
	fs.StringVar(&e.key, "key", os.Getenv("USSLP_API_KEY"), "API key")
	fs.StringVar(&e.tenant, "tenant", os.Getenv("USSLP_TENANT"), "tenant identifier")
	fs.BoolVar(&e.asJSON, "json", false, "print the raw JSON response")
	return e
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// parse installs the common flags, parses, and returns the flag set for
// command-specific flags to have been registered on first.
func parse(fs *flag.FlagSet, args []string) (*endpoints, error) {
	e := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return e, nil
}

// resolve fills in the tenant and the API key from the running runtime.
//
// A tool that refuses to work until the operator has copied a key out of a log
// is a tool nobody uses, and the control surface is loopback-only and
// unauthenticated anyway — so if it is reachable, it is authoritative about
// what this runtime's credentials are. When it is not reachable the key has to
// be supplied, and the error says so.
func (e *endpoints) resolve(ctx context.Context) error {
	if e.key != "" && e.tenant != "" {
		return nil
	}
	var status struct {
		Tenants []string          `json:"tenants"`
		APIKeys map[string]string `json:"api_keys"`
	}
	if err := e.getJSON(ctx, e.control+"/v1/status", &status); err != nil {
		if e.key == "" {
			return fmt.Errorf("no API key: pass --key or USSLP_API_KEY, "+
				"or point --control at a running usslpd (%v)", err)
		}
		return nil
	}
	if e.tenant == "" && len(status.Tenants) > 0 {
		e.tenant = status.Tenants[0]
	}
	if e.key == "" {
		e.key = status.APIKeys[e.tenant]
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func (e *endpoints) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return e.do(req, out)
}

// apiGet calls the platform's API gateway with the caller's credential.
func (e *endpoints) apiGet(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.api+path, nil)
	if err != nil {
		return err
	}
	e.authorise(req)
	return e.do(req, out)
}

// apiPost calls the platform's API gateway with a JSON body.
func (e *endpoints) apiPost(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.api+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	e.authorise(req)
	return e.do(req, out)
}

// controlPost calls usslpd's own control surface.
func (e *endpoints) controlPost(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.control+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return e.do(req, out)
}

func (e *endpoints) authorise(req *http.Request) {
	if e.key != "" {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	if e.tenant != "" {
		req.Header.Set("X-USSLP-Tenant", e.tenant)
	}
}

func (e *endpoints) do(req *http.Request, out any) error {
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Path, resp.Status,
			strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = body
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s returned something that is not JSON: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

// emit prints raw JSON when --json is set and otherwise runs the table
// renderer. Every command goes through it so that --json is uniformly
// available and uniformly the *unmodified* response, which is what makes the
// tool composable with jq.
func (e *endpoints) emit(raw json.RawMessage, table func()) error {
	if e.asJSON {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, raw, "", "  "); err != nil {
			os.Stdout.Write(raw)
			fmt.Println()
			return nil
		}
		pretty.WriteByte('\n')
		_, err := os.Stdout.Write(pretty.Bytes())
		return err
	}
	table()
	return nil
}

// newTable returns a tab writer configured the way every table in this tool is.
func newTable() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

// heading prints a section title.
func heading(s string) {
	fmt.Printf("\n%s\n%s\n", s, strings.Repeat("-", len(s)))
}

// sortedKeys is a small helper so map output is stable between runs.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// errNoStore is returned when a command that needs a store was not given one
// and the runtime has more than one.
var errNoStore = errors.New("--store is required")

// defaultStore picks the store to act on: the one named, or the runtime's only
// one. Guessing when there are several would be how a price change lands in the
// wrong building.
func (e *endpoints) defaultStore(ctx context.Context, named string) (string, error) {
	if named != "" {
		return named, nil
	}
	var stores []struct {
		StoreID string `json:"store_id"`
	}
	if err := e.getJSON(ctx, e.control+"/v1/stores", &stores); err != nil {
		return "", fmt.Errorf("%w (and the control surface is unreachable: %v)", errNoStore, err)
	}
	if len(stores) == 1 {
		return stores[0].StoreID, nil
	}
	names := make([]string, 0, len(stores))
	for _, s := range stores {
		names = append(names, s.StoreID)
	}
	return "", fmt.Errorf("%w: this runtime has %d stores (%s)",
		errNoStore, len(stores), strings.Join(names, ", "))
}
