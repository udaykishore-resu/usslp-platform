package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// stubAdapter is a minimal adapter used to exercise the registry and the
// binding store without dragging a real vendor's wire format into these tests.
type stubAdapter struct {
	name    string
	options func(json.RawMessage) (any, error)
}

func (s *stubAdapter) Name() string { return s.name }
func (s *stubAdapter) Ingest(context.Context, *Delivery) ([]canon.PriceChangeRequested, error) {
	return nil, nil
}
func (s *stubAdapter) IdempotencyParts(*Delivery) []string     { return nil }
func (s *stubAdapter) Verify(context.Context, *Delivery) error { return nil }
func (s *stubAdapter) CompileOptions(raw json.RawMessage) (any, error) {
	if s.options == nil {
		return nil, errors.New("no options accepted")
	}
	return s.options(raw)
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry()
	a := &stubAdapter{name: "one"}
	if err := reg.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Register(&stubAdapter{name: "one"}); !errors.Is(err, ErrDuplicateAdapter) {
		t.Errorf("duplicate registration err = %v, want ErrDuplicateAdapter", err)
	}
	if got, err := reg.Get("one"); err != nil || got != a {
		t.Errorf("Get = %v, %v", got, err)
	}
	if _, err := reg.Get("nope"); !errors.Is(err, ErrNoAdapter) {
		t.Errorf("Get(nope) err = %v, want ErrNoAdapter", err)
	}
	if err := reg.Register(nil); err == nil {
		t.Error("registering nil must fail")
	}
	_ = reg.Register(&stubAdapter{name: "alpha"})
	names := reg.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "one" {
		t.Errorf("Names = %v, want sorted [alpha one]", names)
	}
}

func TestBindingStorePutAndValidate(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(&stubAdapter{
		name:    "cfg",
		options: func(raw json.RawMessage) (any, error) { return string(raw), nil },
	})
	store := NewBindingStore(reg)

	b := &Binding{
		ID: "shop", TenantID: "acme", Adapter: "cfg",
		DefaultCurrency: "usd",
		Options:         json.RawMessage(`{"x":1}`),
	}
	if err := store.Put(b); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get("acme", "shop")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CompiledOptions() != `{"x":1}` {
		t.Errorf("options not compiled: %v", got.CompiledOptions())
	}
	if got.Currency() != "USD" {
		t.Errorf("currency = %q, want normalised USD", got.Currency())
	}
	// The stored binding must be a copy: mutating the caller's struct after Put
	// must not change what a concurrent delivery is processed under.
	b.DefaultCurrency = "EUR"
	if again, _ := store.Get("acme", "shop"); again.Currency() != "USD" {
		t.Error("binding store did not copy the binding")
	}

	if _, err := store.Get("acme", "missing"); !errors.Is(err, ErrNoBinding) {
		t.Errorf("missing binding err = %v", err)
	}
	if _, err := store.Get("other", "shop"); !errors.Is(err, ErrNoBinding) {
		t.Error("a binding must not be visible to another tenant")
	}

	bad := []*Binding{
		{ID: "", TenantID: "acme", Adapter: "cfg"},
		{ID: "a/b", TenantID: "acme", Adapter: "cfg"},
		{ID: "x", TenantID: "", Adapter: "cfg"},
		{ID: "x", TenantID: "acme", Adapter: ""},
		{ID: "x", TenantID: "acme", Adapter: "nonexistent"},
		{ID: "x", TenantID: "acme", Adapter: "cfg", DefaultCurrency: "US"},
		{ID: "x", TenantID: "acme", Adapter: "cfg", StoreMap: map[string]canon.StoreID{"a": "b/c"}},
		{ID: "x", TenantID: "acme", Adapter: "cfg", RateLimit: RateLimitSpec{RatePerSecond: -1}},
	}
	for i, b := range bad {
		if err := store.Put(b); err == nil {
			t.Errorf("bad binding %d was accepted", i)
		}
	}

	if n := store.Count(); n != 1 {
		t.Errorf("Count = %d, want 1", n)
	}
	if ts := store.Tenants(); len(ts) != 1 || ts[0] != "acme" {
		t.Errorf("Tenants = %v", ts)
	}
	if !store.Delete("acme", "shop") {
		t.Error("Delete reported the binding was absent")
	}
	if store.Delete("acme", "shop") {
		t.Error("Delete reported a second removal")
	}
}

func TestBindingStoreRejectsOptionsForPlainAdapter(t *testing.T) {
	reg := NewRegistry()
	// An adapter with no CompileOptions method must not silently ignore
	// configuration: a retailer who wrote options that are being thrown away
	// would have no way to discover it.
	reg.MustRegister(&noOptionsAdapter{})
	store := NewBindingStore(reg)
	err := store.Put(&Binding{ID: "x", TenantID: "t", Adapter: "plain", Options: json.RawMessage(`{"a":1}`)})
	if err == nil || !strings.Contains(err.Error(), "takes no options") {
		t.Fatalf("err = %v", err)
	}
}

type noOptionsAdapter struct{}

func (*noOptionsAdapter) Name() string { return "plain" }
func (*noOptionsAdapter) Ingest(context.Context, *Delivery) ([]canon.PriceChangeRequested, error) {
	return nil, nil
}
func (*noOptionsAdapter) IdempotencyParts(*Delivery) []string     { return nil }
func (*noOptionsAdapter) Verify(context.Context, *Delivery) error { return nil }

func TestSecretsRedactOnMarshal(t *testing.T) {
	s := Secrets{
		HMACKey: "super-secret", APIKeyID: "AKID", APIKey: "also-secret",
		SharedToken: "sh4red-v4lue", Username: "svc", NotificationURL: "https://x/y",
		BearerToken: "be4rer-tok", PeerCommonNames: []string{"pos.acme.example"},
	}
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	for _, secret := range []string{"super-secret", "also-secret", "sh4red-v4lue", "be4rer-tok"} {
		if strings.Contains(out, secret) {
			t.Fatalf("marshalled secrets leaked %q: %s", secret, out)
		}
	}
	// Non-secret identifiers stay legible: an operator has to be able to see
	// which key id a binding is configured with.
	for _, visible := range []string{"AKID", "svc", "https://x/y", "pos.acme.example"} {
		if !strings.Contains(out, visible) {
			t.Errorf("expected %q to remain visible in %s", visible, out)
		}
	}
	// Unmarshalling is unaffected, so configuration still loads.
	var back Secrets
	if err := json.Unmarshal([]byte(`{"hmac_key":"k"}`), &back); err != nil || back.HMACKey != "k" {
		t.Fatalf("unmarshal = %+v, %v", back, err)
	}
}

func TestVerifyHMACSHA256(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	key := "shhh"
	sig := EncodeSignature(SignHMACSHA256(key, body), EncodingBase64)

	if err := VerifyHMACSHA256(key, body, sig, EncodingBase64, ""); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// One flipped byte in the body must fail.
	if err := VerifyHMACSHA256(key, []byte(`{"hello":"worlD"}`), sig, EncodingBase64, ""); err == nil {
		t.Error("tampered body accepted")
	}
	// A different key must fail.
	if err := VerifyHMACSHA256("other", body, sig, EncodingBase64, ""); err == nil {
		t.Error("wrong key accepted")
	}
	// An unconfigured secret must fail closed rather than waving the request
	// through, which is the whole point of the check.
	if err := VerifyHMACSHA256("", body, sig, EncodingBase64, ""); err == nil {
		t.Error("empty configured key accepted a signature")
	} else if Classify(err).Reason != "no_secret" {
		t.Errorf("reason = %q, want no_secret", Classify(err).Reason)
	}
	if err := VerifyHMACSHA256(key, body, "", EncodingBase64, ""); Classify(err).Reason != "missing_signature" {
		t.Errorf("missing header reason = %q", Classify(err).Reason)
	}
	if err := VerifyHMACSHA256(key, body, "!!not base64!!", EncodingBase64, ""); err == nil {
		t.Error("undecodable signature accepted")
	} else if Classify(err).Kind != FailureUnauthorized {
		t.Errorf("kind = %v, want unauthorized", Classify(err).Kind)
	}

	hexSig := "sha256=" + EncodeSignature(SignHMACSHA256(key, body), EncodingHex)
	if err := VerifyHMACSHA256(key, body, hexSig, EncodingHex, "sha256="); err != nil {
		t.Fatalf("prefixed hex signature rejected: %v", err)
	}
	if err := VerifyHMACSHA256(key, body, hexSig, EncodingHex, "sha512="); err == nil {
		t.Error("wrong prefix accepted")
	}
}

func TestVerifySharedTokenAndPeer(t *testing.T) {
	if err := VerifySharedToken("abc", "abc", "auth code"); err != nil {
		t.Fatalf("matching token rejected: %v", err)
	}
	if err := VerifySharedToken("abc", "abd", "auth code"); err == nil {
		t.Error("mismatched token accepted")
	}
	if err := VerifySharedToken("", "abc", "auth code"); err == nil {
		t.Error("unconfigured secret accepted a token")
	}

	b := &Binding{Secrets: Secrets{PeerCommonNames: []string{"a.example", "b.example"}}}
	if ok, configured := VerifyPeerIdentity(b, "b.example"); !ok || !configured {
		t.Error("allowed peer rejected")
	}
	if ok, configured := VerifyPeerIdentity(b, "c.example"); ok || !configured {
		t.Error("unlisted peer accepted")
	}
	if ok, configured := VerifyPeerIdentity(&Binding{}, "a.example"); ok || configured {
		t.Error("a binding with no peer list must report unconfigured")
	}
}

func TestStoreResolution(t *testing.T) {
	r := BindingResolver{}
	ctx := context.Background()
	b := &Binding{
		DefaultStore: "S-HQ",
		StoreMap:     map[string]canon.StoreID{"0042": "GB-0042", "SHOP.myshopify.com": "GB-0001"},
	}
	for _, c := range []struct {
		external string
		want     canon.StoreID
	}{
		{"", "S-HQ"},
		{"0042", "GB-0042"},
		{"shop.myshopify.com", "GB-0001"}, // case-insensitive second pass
	} {
		got, err := r.Resolve(ctx, b, c.external)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", c.external, err)
		}
		if got != c.want {
			t.Errorf("Resolve(%q) = %q, want %q", c.external, got, c.want)
		}
	}
	// An unmapped code must fail rather than being invented: a price change
	// landing in the wrong building is the failure this prevents.
	if _, err := r.Resolve(ctx, b, "9999"); err == nil {
		t.Error("unmapped store accepted")
	} else if Classify(err).Reason != "store_unmapped" {
		t.Errorf("reason = %q", Classify(err).Reason)
	}
	// Unless the binding explicitly opts in.
	b2 := &Binding{AllowUnmappedStores: true}
	if got, err := r.Resolve(ctx, b2, "9999"); err != nil || got != "9999" {
		t.Errorf("pass-through = %q, %v", got, err)
	}
	if _, err := r.Resolve(ctx, b2, "bad/store"); err == nil {
		t.Error("a store code with reserved characters must be refused even when pass-through is on")
	}
	if _, err := r.Resolve(ctx, &Binding{}, ""); err == nil {
		t.Error("no store and no default must fail")
	}
}

func TestClassifyAndStatusCodes(t *testing.T) {
	// An unclassified error is treated as malformed, not as an internal fault:
	// answering 5xx to a body that will never parse makes the POS retry it for
	// hours.
	cls := Classify(errors.New("boom"))
	if cls.Kind != FailureMalformed || cls.Kind.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Fatalf("classify = %+v, status %d", cls, cls.Kind.HTTPStatus())
	}
	for kind, want := range map[FailureKind]int{
		FailureUnauthorized: http.StatusUnauthorized,
		FailureNotFound:     http.StatusNotFound,
		FailureRateLimited:  http.StatusTooManyRequests,
		FailureUnavailable:  http.StatusServiceUnavailable,
		FailureMalformed:    http.StatusUnprocessableEntity,
		FailureInvalid:      http.StatusUnprocessableEntity,
	} {
		if got := kind.HTTPStatus(); got != want {
			t.Errorf("%s status = %d, want %d", kind, got, want)
		}
	}
	if !FailureMalformed.RetainsBody() || !FailureInvalid.RetainsBody() {
		t.Error("malformed and invalid deliveries must retain their body for support")
	}
	if FailureUnauthorized.RetainsBody() {
		t.Error("an unauthenticated caller's payload must not be stored")
	}
	wrapped := Malformed("r", "d", errors.New("cause"))
	if !errors.Is(wrapped, wrapped.Err) {
		t.Error("Unwrap is not exposing the cause")
	}
	if _, ok := IsPartial(&PartialError{Total: 3}); !ok {
		t.Error("IsPartial did not recognise a PartialError")
	}
	if _, ok := IsPartial(errors.New("x")); ok {
		t.Error("IsPartial matched a plain error")
	}
}

func TestDeliveryHelpers(t *testing.T) {
	d := &Delivery{Headers: http.Header{"X-Thing": []string{"v"}}}
	if d.Header("x-thing") != "v" {
		t.Error("header lookup must be case-insensitive")
	}
	if (&Delivery{}).Header("anything") != "" {
		t.Error("a delivery with no headers must return empty, not panic")
	}
	if d.Options() != nil {
		t.Error("a delivery with no binding has no options")
	}
}
