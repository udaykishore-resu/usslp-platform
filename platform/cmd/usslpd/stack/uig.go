package stack

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	otadomain "github.com/usslp/usslp/platform/internal/ota/domain"
	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/adapters/clover"
	"github.com/usslp/usslp/platform/internal/uig/adapters/filedrop"
	"github.com/usslp/usslp/platform/internal/uig/adapters/generic"
	"github.com/usslp/usslp/platform/internal/uig/adapters/lightspeed"
	"github.com/usslp/usslp/platform/internal/uig/adapters/ncr"
	"github.com/usslp/usslp/platform/internal/uig/adapters/oracle"
	"github.com/usslp/usslp/platform/internal/uig/adapters/sap"
	"github.com/usslp/usslp/platform/internal/uig/adapters/shopify"
	"github.com/usslp/usslp/platform/internal/uig/adapters/square"
	"github.com/usslp/usslp/platform/internal/uig/deliveries"
	"github.com/usslp/usslp/platform/internal/uig/gateway"
	"github.com/usslp/usslp/platform/internal/uig/pipeline"
	"github.com/usslp/usslp/platform/internal/uig/reliability"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/idem"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// ShopifyBindingID is the identifier of the Shopify binding usslpd installs for
// every tenant. It appears in the ingest URL: POST /v1/ingest/{tenant}/pos.
const ShopifyBindingID = "pos"

// uigService is the assembled Universal Integration Gateway.
//
// The cmd/uig binary assembles the same objects; it cannot be reused directly
// because it is package main. The list of adapters below is the one thing that
// must be kept in step with it — a gateway whose registry has drifted from the
// binary's capabilities silently stops resolving a binding.
type uigService struct {
	pipe     *pipeline.Pipeline
	gw       *gateway.Gateway
	bindings *adapter.BindingStore
	// hmacKey is the shared secret every generated Shopify binding signs with.
	// It is derived from the data directory rather than randomised so that a
	// restart does not invalidate a curl command someone had in their history.
	hmacKey string
}

// HMACKey is the Shopify signing secret for the generated bindings. A caller
// pushing a webhook has to sign the exact body with it; the adapter verifies in
// constant time and a mismatch is a 401, which is the point.
func (u *uigService) HMACKey() string { return u.hmacKey }

// SignShopify returns the value of the X-Shopify-Hmac-Sha256 header for a body.
func (u *uigService) SignShopify(body []byte) string {
	return adapter.EncodeSignature(adapter.SignHMACSHA256(u.hmacKey, body), adapter.EncodingBase64)
}

func (s *Stack) startUIG(ctx context.Context, c *cloudServices) error {
	rt, err := s.runtimeFor("uig", s.adminPort(1))
	if err != nil {
		return err
	}
	kv, err := s.kvFor("uig", rt, kvstore.SyncEvery)
	if err != nil {
		return err
	}
	guardBackend, err := idem.NewKVBackend(kv, "uig/idem/")
	if err != nil {
		return fmt.Errorf("usslpd: uig idempotency backend: %w", err)
	}
	guard, err := idem.New(guardBackend, idem.WithWindow(idem.DefaultWindow))
	if err != nil {
		return fmt.Errorf("usslpd: uig idempotency guard: %w", err)
	}
	store, err := deliveries.New(kv, deliveries.Options{
		Prefix: "uig/deliveries/", Retention: deliveries.DefaultRetention,
	})
	if err != nil {
		return fmt.Errorf("usslpd: uig delivery store: %w", err)
	}

	breakers := reliability.NewBreakerSet(reliability.BreakerConfig{
		FailureThreshold: 5, Cooldown: 15 * time.Second,
	})
	registry := adapter.NewRegistry()
	for _, a := range []adapter.Adapter{
		shopify.New(), square.New(), ncr.New(), sap.New(), oracle.New(),
		lightspeed.New(), clover.New(nil, breakers), filedrop.New(), generic.New(),
	} {
		if err := registry.Register(a); err != nil {
			return fmt.Errorf("usslpd: registering the %s adapter: %w", a.Name(), err)
		}
	}

	u := &uigService{bindings: adapter.NewBindingStore(registry), hmacKey: s.uigHMACKey()}
	if err := s.installBindings(u); err != nil {
		return err
	}

	metrics := pipeline.NewMetrics(rt.Metrics)
	u.pipe, err = pipeline.New(pipeline.Config{
		Registry: registry, Bindings: u.bindings, Guard: guard,
		Bus: s.log, Deliveries: store,
		Limiter: reliability.NewLimiter(), Breakers: breakers,
		Metrics: metrics, Log: rt.Log, Tracer: rt.Tracer,
		Region: canon.Region(s.cfg.Region),
	})
	if err != nil {
		return fmt.Errorf("usslpd: assembling the UIG pipeline: %w", err)
	}
	s.push("uig pipeline", func(context.Context) error { return u.pipe.Close() })

	u.gw, err = gateway.New(gateway.Config{
		Pipeline: u.pipe, OperatorToken: "usslpd", MaxBodyBytes: gateway.DefaultMaxBodyBytes,
		Log: rt.Log,
	})
	if err != nil {
		return fmt.Errorf("usslpd: assembling the UIG: %w", err)
	}

	ln, err := s.listen("uig", s.cfg.Ports.UIG)
	if err != nil {
		return err
	}
	s.serve("uig", ln, u.gw.Handler(), 20*time.Second)

	rt.Health.Register("event-stream", func(ctx context.Context) error {
		return s.log.EnsureStreams(ctx, canon.StreamPriceUpdates, canon.StreamPOSIngress)
	})
	c.uig = u
	c.uigURL = "http://" + ln.Addr().String()
	c.admin["uig"] = "http://" + rt.Admin.Addr()
	rt.Ready()
	return nil
}

// installBindings creates one Shopify binding per tenant.
//
// The binding, not the adapter, is where tenancy lives: one Shopify adapter
// serves every retailer, and the shop domain, the signing secret, the store map
// and the currency default are per binding. Mapping each generated store's shop
// domain explicitly — rather than setting AllowUnmappedStores — is deliberate:
// silently inventing a store from a header is how a price change reaches a
// shelf in the wrong building.
func (s *Stack) installBindings(u *uigService) error {
	for _, tenant := range s.cfg.Tenants {
		storeMap := map[string]canon.StoreID{}
		for i := 0; i < s.cfg.Stores; i++ {
			store := StoreIDFor(tenant, i)
			storeMap[ShopDomainFor(store)] = store
		}
		opts, err := json.Marshal(map[string]any{
			"topics":     []string{"products/update", "products/create"},
			"sku_source": "sku",
		})
		if err != nil {
			return err
		}
		b := &adapter.Binding{
			ID: ShopifyBindingID, TenantID: tenant, Adapter: shopify.Name,
			POSInstance:     "usslpd-demo",
			Description:     "Generated Shopify integration for the single-process runtime",
			Secrets:         adapter.Secrets{HMACKey: u.hmacKey},
			DefaultStore:    StoreIDFor(tenant, 0),
			StoreMap:        storeMap,
			DefaultCurrency: s.cfg.Currency,
			Options:         opts,
			InitiatedBy:     "shopify/" + string(tenant),
			// A generous ingress budget: the 1,000-sample latency benchmark in
			// test/e2e pushes webhooks as fast as it can, and a rate limiter
			// tuned for one shop would turn that into a measurement of the
			// limiter.
			RateLimit: adapter.RateLimitSpec{RatePerSecond: 20000, Burst: 20000},
		}
		if err := u.bindings.Put(b); err != nil {
			return fmt.Errorf("usslpd: installing the %s binding: %w", tenant, err)
		}
	}
	return nil
}

// uigHMACKey derives the webhook signing secret from the data directory.
//
// Deterministic per directory, so a restart does not invalidate a signature
// somebody has in a shell script; distinct between directories, so two runtimes
// on one machine do not accept each other's traffic. It is not a secret in any
// meaningful sense and this deployment does not pretend otherwise — a
// production binding's key comes from the retailer's Shopify admin.
func (s *Stack) uigHMACKey() string {
	sum := sha256.Sum256([]byte("usslpd/uig/hmac/" + s.cfg.DataDir))
	return hex.EncodeToString(sum[:16])
}

// firmwareKeys returns the Ed25519 keys the OTA service trusts to sign
// firmware, generating the pair on first use.
//
// A rollout pipeline that cannot verify a signature must accept no artifact, so
// running with an empty ring would make every OTA path in the test suite a test
// of the refusal rather than of the rollout. The private half is kept on the
// Stack so `usslpctl ota start` and the end-to-end suite can produce an
// artifact this service will accept; in production the private half never
// exists on the same machine as the service.
func (s *Stack) firmwareKeys() otadomain.KeyRing {
	if s.fwPriv == nil {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			// ed25519 key generation fails only if crypto/rand does, which is
			// not a condition a retail platform can continue through.
			panic("usslpd: crypto/rand unavailable: " + err.Error())
		}
		s.fwPub, s.fwPriv = pub, priv
	}
	return otadomain.KeyRing{"usslpd-firmware": s.fwPub}
}

// SignFirmware signs an artifact with the runtime's firmware key and returns
// the base64 signature the OTA upload endpoint expects.
//
// The signature covers the release manifest — version, hardware tier and image
// digest — rather than the raw bytes, which is the whole point of the scheme:
// signing the image alone would prove that some authorised party built those
// bytes and say nothing about what hardware they are for, and a properly signed
// image flashed onto the wrong panel is a brick. Delegating to
// domain.SignArtifact rather than reimplementing it means this cannot drift
// from what the service verifies.
func (s *Stack) SignFirmware(version, hardwareTier string, image []byte) string {
	s.firmwareKeys()
	return otadomain.SignArtifact(s.fwPriv, otadomain.Version(version), hardwareTier, image)
}

// FirmwareKeyID names the signing key in an upload.
func (s *Stack) FirmwareKeyID() string { return "usslpd-firmware" }
