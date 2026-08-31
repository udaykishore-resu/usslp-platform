package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/adapters/filedrop"
	"github.com/usslp/usslp/platform/internal/uig/adapters/shopify"
	"github.com/usslp/usslp/platform/internal/uig/reliability"
)

const (
	shopifyKey    = "service-test-shopify-key"
	operatorToken = "service-test-operator-token"
)

// bindingsFile writes the configuration a real deployment would mount.
func bindingsFile(t *testing.T, dir, dropDir string) string {
	t.Helper()
	bindings := []map[string]any{
		{
			"id":               "shop",
			"tenant_id":        "acme",
			"adapter":          shopify.Name,
			"pos_instance":     "shopify-uk",
			"default_currency": "GBP",
			"default_store":    "GB-0001",
			"initiated_by":     "pos:acme/shopify",
			"secrets":          map[string]any{"hmac_key": shopifyKey},
		},
		{
			"id":               "nightly",
			"tenant_id":        "acme",
			"adapter":          filedrop.Name,
			"default_currency": "GBP",
			"default_store":    "GB-0001",
			"options": map[string]any{
				"format": "delimited",
				"header": "auto",
				"columns": map[string]any{
					"sku":      map[string]any{"name": "ITEMCODE"},
					"price":    map[string]any{"name": "PRICE"},
					"currency": map[string]any{"const": "GBP"},
				},
				"watch": map[string]any{
					"dir":      dropDir,
					"pattern":  "PRICE_*.csv",
					"interval": "20ms",
					"settle":   "1ns",
				},
			},
		},
	}
	raw, err := json.MarshalIndent(bindings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bindings.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startService(t *testing.T) (*Service, Config) {
	t.Helper()
	root := t.TempDir()
	dropDir := filepath.Join(root, "drops")
	if err := os.MkdirAll(dropDir, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Addr:              "127.0.0.1:0",
		EventLogDir:       filepath.Join(root, "eventlog"),
		StateDir:          filepath.Join(root, "state"),
		BindingsFile:      bindingsFile(t, root, dropDir),
		OperatorToken:     operatorToken,
		DedupeWindow:      time.Hour,
		DeliveryRetention: time.Hour,
		MaxBodyBytes:      1 << 20,
		ShutdownGrace:     5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		DefaultRateLimit:  reliability.DefaultRate,
		DefaultBurst:      reliability.DefaultBurst,
		BreakerThreshold:  5,
		BreakerCooldown:   time.Second,
	}
	cfg.Service.Service = ServiceName
	cfg.Service.Version = "test"
	cfg.Service.Region = "eu-west-1"
	cfg.Service.LogLevel = "error"
	cfg.Service.LogFormat = "json"
	// Port zero on the admin surface too, so parallel tests do not collide on
	// the default 9090.
	cfg.Service.AdminAddr = "127.0.0.1:0"
	cfg.Service.DataDir = root
	cfg.Service.TraceSample = 1

	svc, err := NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	go func() {
		if err := svc.Serve(); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() { svc.Shutdown(2 * time.Second) })
	// Wait for the listener to answer before the test posts to it.
	waitReady(t, svc)
	return svc, cfg
}

func waitReady(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + svc.Runtime().Admin.Addr() + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the service never became ready")
}

const productBody = `{"id":1,"title":"Espresso","updated_at":"2026-08-30T10:00:00Z","variants":[
  {"id":11,"sku":"ESP-1KG","price":"12.99","updated_at":"2026-08-30T10:00:00Z"}
]}`

func TestServiceIngestsAWebhookEndToEnd(t *testing.T) {
	svc, _ := startService(t)

	req, err := http.NewRequest(http.MethodPost,
		"http://"+svc.Addr()+"/v1/ingest/acme/shop", strings.NewReader(productBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(shopify.HeaderTopic, "products/update")
	req.Header.Set(shopify.HeaderShopDomain, "acme-uk.myshopify.com")
	req.Header.Set(shopify.HeaderWebhookID, "wh-service-1")
	req.Header.Set(shopify.HeaderHMAC, adapter.EncodeSignature(
		adapter.SignHMACSHA256(shopifyKey, []byte(productBody)), adapter.EncodingBase64))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Status   string `json:"status"`
		Accepted int    `json:"changes_accepted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "accepted" || out.Accepted != 1 {
		t.Fatalf("response = %+v", out)
	}
}

func TestServiceRunsTheConfiguredFileWatcher(t *testing.T) {
	svc, cfg := startService(t)
	dropDir := filepath.Join(cfg.Service.DataDir, "drops")

	file := filepath.Join(dropDir, "PRICE_20260830.csv")
	if err := os.WriteFile(file,
		[]byte("ITEMCODE,PRICE\nESP-1KG,12.99\nTEA-500,3.75\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}

	// A file watcher configured inside a binding means adding a nightly feed is
	// a configuration change rather than a deployment change; this asserts the
	// service actually starts one.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(file + filedrop.MarkerSuffix); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(file + filedrop.MarkerSuffix); err != nil {
		t.Fatalf("the drop was never processed: %v", err)
	}

	// And that the changes reached the operator's view of the binding.
	req, _ := http.NewRequest(http.MethodGet, "http://"+svc.Addr()+"/v1/bindings/acme", nil)
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), shopifyKey) {
		t.Fatal("the operator API leaked a signing key")
	}
	if !strings.Contains(string(raw), `"changes_emitted":2`) {
		t.Errorf("the file drop's changes are not visible on the binding health:\n%s", raw)
	}
}

func TestServiceExposesItsMetricsAndHealth(t *testing.T) {
	svc, _ := startService(t)
	admin := "http://" + svc.Runtime().Admin.Addr()

	resp, err := http.Get(admin + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// These names are a contract: dashboards and alert rules are written
	// against them and outlive any given release.
	for _, want := range []string{
		"usslp_uig_ingest_total",
		"usslp_uig_ingest_duration_seconds",
		"usslp_uig_dedupe_hits_total",
		"usslp_uig_quarantined_total",
		"usslp_uig_changes_emitted_total",
		"usslp_uig_parse_errors_total",
		"usslp_uig_latency_budget_exceeded_total",
		"usslp_uig_bindings_configured",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		r, err := http.Get(admin + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("%s = %d", path, r.StatusCode)
		}
	}
}

func TestServiceRefusesReadinessWithNoBindings(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "bindings.json")
	if err := os.WriteFile(empty, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Addr:          "127.0.0.1:0",
		EventLogDir:   filepath.Join(root, "eventlog"),
		StateDir:      filepath.Join(root, "state"),
		BindingsFile:  empty,
		OperatorToken: operatorToken,
		ShutdownGrace: 2 * time.Second,
	}
	cfg.Service.AdminAddr = "127.0.0.1:0"
	cfg.Service.LogLevel = "error"
	svc, err := NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Shutdown(2 * time.Second)
	svc.Runtime().Ready()

	// A gateway with no bindings cannot serve anyone, and the overwhelmingly
	// likely cause is a configuration mount that did not arrive. Failing
	// readiness keeps it out of the load balancer instead of letting it 404 a
	// retailer's whole price book.
	resp, err := http.Get("http://" + svc.Runtime().Admin.Addr() + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a gateway with no bindings reported itself ready")
	}
}

func TestServiceRejectsABrokenBindingsFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bindings.json")
	if err := os.WriteFile(path,
		[]byte(`[{"id":"x","tenant_id":"acme","adapter":"does-not-exist"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Addr: "127.0.0.1:0", EventLogDir: filepath.Join(root, "eventlog"),
		StateDir: filepath.Join(root, "state"), BindingsFile: path,
	}
	cfg.Service.AdminAddr = "127.0.0.1:0"
	cfg.Service.LogLevel = "error"
	if _, err := NewService(cfg); err == nil {
		t.Fatal("a binding naming an unknown adapter was installed")
	}
}

func TestRegisterAdaptersCoversEverySupportedSource(t *testing.T) {
	reg := adapter.NewRegistry()
	if err := RegisterAdapters(reg, reliability.NewBreakerSet(reliability.BreakerConfig{})); err != nil {
		t.Fatalf("RegisterAdapters: %v", err)
	}
	// The registry is the binary's actual capability list; a registry that
	// drifts from it is how a binding silently stops resolving after a
	// refactor.
	want := []string{
		"clover", "filedrop", "lightspeed", "mapping", "ncr",
		"oracle-retail", "sap-idoc", "shopify", "square",
	}
	got := reg.Names()
	if len(got) != len(want) {
		t.Fatalf("registered %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("adapter %d = %q, want %q", i, got[i], want[i])
		}
	}
	// Registering twice must fail rather than rebinding a name that appears in
	// customer configuration.
	if err := RegisterAdapters(reg, nil); err == nil {
		t.Error("a duplicate registration was accepted")
	}
}

func TestLoadConfigResolvesSecretsFromFiles(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "operator-token")
	if err := os.WriteFile(secret, []byte("  from-a-mounted-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Contract §8: every value is resolvable from a file, because a Store
	// Gateway Unit takes its credentials from a mounted secret on a device that
	// is not running Kubernetes.
	t.Setenv("USSLP_UIG_OPERATOR_TOKEN_FILE", secret)
	t.Setenv("USSLP_UIG_ADDR", "127.0.0.1:0")
	t.Setenv("USSLP_DATA_DIR", dir)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OperatorToken != "from-a-mounted-secret" {
		t.Errorf("operator token = %q", cfg.OperatorToken)
	}
	if cfg.EventLogDir != dir+"/eventlog" || cfg.StateDir != dir+"/uig-state" {
		t.Errorf("derived dirs = %q, %q", cfg.EventLogDir, cfg.StateDir)
	}
	if cfg.DedupeWindow != 24*time.Hour {
		t.Errorf("dedupe window = %s, want the contract's 24 hours", cfg.DedupeWindow)
	}
	fields := describeConfig(cfg)
	for i := 0; i+1 < len(fields); i += 2 {
		if s, ok := fields[i+1].(string); ok && strings.Contains(s, "from-a-mounted-secret") {
			t.Fatalf("describeConfig leaked the operator token: %v", fields)
		}
	}
}

func TestShutdownIsIdempotentAndDrains(t *testing.T) {
	svc, _ := startService(t)
	svc.Shutdown(2 * time.Second)
	// A second call must be a no-op rather than a panic on a closed channel: a
	// signal arriving during an already-running drain is normal.
	svc.Shutdown(2 * time.Second)

	resp, err := http.Get("http://" + svc.Runtime().Admin.Addr() + "/readyz")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("the service still reports ready after shutdown")
		}
	}
}
