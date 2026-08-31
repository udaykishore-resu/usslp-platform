// Command api-gateway runs the USSLP API Gateway.
//
// It is the platform's front door and the only process in the cloud tier that
// faces an untrusted network. It authenticates callers (API key, ES256 bearer
// token, or a client certificate from the USSLP hierarchy), turns them into
// tenant-bound principals, enforces role-based authorisation and store
// scoping, rate-limits per tenant and per credential, routes to the internal
// services with per-route timeouts, retries and circuit breaking, streams
// live platform events over WebSocket, and serves the operator console and the
// OpenAPI description of the whole surface.
//
// Configuration is twelve-factor with the USSLP_ prefix, and every value is
// also resolvable from a file (NAME_FILE=/run/secrets/x), per §8 of the
// interface contracts.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/usslp/usslp/platform/internal/apigw"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/config"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// serviceName is the identity every metric, log line and span carries.
const serviceName = "api-gateway"

// shutdownGrace bounds the drain.
//
// Fifteen seconds is sized against the work in flight: an estate-wide batch
// price import proxied through the gateway can legitimately still be running,
// and cutting it off mid-flight leaves half a store repriced. It must be
// shorter than the Deployment's terminationGracePeriodSeconds or the drain is
// pointless.
const shutdownGrace = 15 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

func run() error {
	loader := config.New("USSLP")
	svcCfg := config.LoadService(loader, serviceName)

	httpAddr := loader.String("GATEWAY_HTTP_ADDR", ":8080")
	adminAddr := loader.String("ADMIN_ADDR", ":9080")

	// Upstream addresses. The defaults are the dev ports from the interface
	// contracts, so `make dev` needs no configuration at all.
	upstreamAddrs := map[string]string{
		apigw.UpstreamUIG:       loader.String("UIG_ADDR", "http://127.0.0.1:8081"),
		apigw.UpstreamLabel:     loader.String("LABEL_SERVICE_ADDR", "http://127.0.0.1:8082"),
		apigw.UpstreamRegistry:  loader.String("DEVICE_REGISTRY_ADDR", "http://127.0.0.1:8083"),
		apigw.UpstreamOTA:       loader.String("OTA_SERVICE_ADDR", "http://127.0.0.1:8084"),
		apigw.UpstreamPricing:   loader.String("PRICING_SERVICE_ADDR", "http://127.0.0.1:8085"),
		apigw.UpstreamPromotion: loader.String("PROMOTION_SERVICE_ADDR", "http://127.0.0.1:8086"),
		apigw.UpstreamAnalytics: loader.String("ANALYTICS_SERVICE_ADDR", "http://127.0.0.1:8087"),
	}

	tenantRate := loader.Int("RATE_TENANT_PER_SECOND", int(apigw.DefaultTenantLimit.Rate))
	tenantBurst := loader.Int("RATE_TENANT_BURST", int(apigw.DefaultTenantLimit.Burst))
	credRate := loader.Int("RATE_CREDENTIAL_PER_SECOND", int(apigw.DefaultCredentialLimit.Rate))
	credBurst := loader.Int("RATE_CREDENTIAL_BURST", int(apigw.DefaultCredentialLimit.Burst))
	costlyRate := loader.Int("RATE_EXPENSIVE_PER_SECOND", int(apigw.DefaultExpensiveLimit.Rate))
	costlyBurst := loader.Int("RATE_EXPENSIVE_BURST", int(apigw.DefaultExpensiveLimit.Burst))

	upstreamTimeout := loader.Duration("UPSTREAM_TIMEOUT", 0)
	maxRequestBytes := loader.Int("MAX_REQUEST_BYTES", apigw.DefaultMaxRequestBytes)
	maxResponseBytes := loader.Int("MAX_RESPONSE_BYTES", apigw.DefaultMaxResponseBytes)
	breakerFailures := loader.Int("BREAKER_FAILURE_THRESHOLD", apigw.DefaultFailureThreshold)
	breakerSuccesses := loader.Int("BREAKER_SUCCESS_THRESHOLD", apigw.DefaultSuccessThreshold)
	breakerTimeout := loader.Duration("BREAKER_OPEN_TIMEOUT", apigw.DefaultOpenTimeout)

	streamQueue := loader.Int("STREAM_QUEUE_DEPTH", apigw.DefaultStreamQueue)
	streamPing := loader.Duration("STREAM_PING_INTERVAL", apigw.DefaultPingInterval)
	streamPong := loader.Duration("STREAM_PONG_TIMEOUT", apigw.DefaultPongTimeout)

	jwksPath := loader.String("JWKS_PATH", "")
	jwtIssuer := loader.String("JWT_ISSUER", "")
	jwtAudience := loader.String("JWT_AUDIENCE", "")
	keyPrefix := loader.String("API_KEY_PREFIX", apigw.KeyPrefixLive)
	kdfIterations := loader.Int("API_KEY_KDF_ITERATIONS", apigw.DefaultKDFIterations)
	bootstrapTenant := loader.String("BOOTSTRAP_TENANT", "")

	tlsCert := loader.String("TLS_CERT_FILE", "")
	tlsKey := loader.String("TLS_KEY_FILE", "")
	clientCAFile := loader.String("TLS_CLIENT_CA_FILE", "")

	if err := loader.Err(); err != nil {
		return err
	}

	rt, err := obs.NewRuntime(obs.RuntimeConfig{
		Service: serviceName, Version: svcCfg.Version, Region: svcCfg.Region,
		LogLevel: svcCfg.LogLevel, LogFormat: svcCfg.LogFormat,
		AdminAddr: adminAddr, EnablePprof: svcCfg.EnablePprof,
		TraceSampleOneIn: uint64(svcCfg.TraceSample),
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- credentials ----------------------------------------------------
	keyStore := apigw.NewMemoryKeyStore()
	issuer, err := apigw.NewKeyIssuer(apigw.KeyIssuerConfig{
		Store: keyStore, Prefix: keyPrefix, Iterations: kdfIterations,
	})
	if err != nil {
		return err
	}
	if bootstrapTenant != "" {
		if err := bootstrap(ctx, issuer, canon.TenantID(bootstrapTenant), rt.Log); err != nil {
			return err
		}
	}

	jwtKeys, err := loadJWKS(jwksPath, rt.Log)
	if err != nil {
		return err
	}

	// --- event source ---------------------------------------------------
	// Every replica consumes the whole stream into its own subscribers, so it
	// needs its own consumer group: members of a group share partitions, and
	// two replicas in one group would each show half the events.
	var source apigw.EventSource
	dataDir := svcCfg.DataDir
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("creating the data directory %s: %w", dataDir, err)
	}
	bus, err := eventlog.Open(filepath.Join(dataDir, "eventlog"),
		eventlog.WithMetrics(rt.Metrics), eventlog.WithLogger(rt.Log))
	if err != nil {
		rt.Log.Warn("event log unavailable; the live stream will accept connections and emit nothing",
			"error", err)
	} else {
		rt.OnShutdown(func(context.Context) error { return bus.Close() })
		if err := bus.EnsureStreams(ctx, canon.AllStreams()...); err != nil {
			rt.Log.Warn("could not provision streams", "error", err)
		}
		source = &apigw.BusSource{
			Bus:   bus,
			Group: serviceName + "-" + canon.NewSpanID(),
			Log:   rt.Log,
		}
	}

	// --- assembly -------------------------------------------------------
	upstreams := make([]apigw.UpstreamConfig, 0, len(upstreamAddrs))
	for name, addr := range upstreamAddrs {
		upstreams = append(upstreams, apigw.UpstreamConfig{
			Name: name, Address: addr, Timeout: upstreamTimeout,
			MaxRequestBytes: int64(maxRequestBytes), MaxResponseBytes: int64(maxResponseBytes),
			Breaker: apigw.BreakerConfig{
				FailureThreshold: breakerFailures,
				SuccessThreshold: breakerSuccesses,
				OpenTimeout:      breakerTimeout,
			},
		})
	}

	gw, err := apigw.New(apigw.Config{
		Service: serviceName, Version: svcCfg.Version,
		Log: rt.Log, Tracer: rt.Tracer, Registry: rt.Metrics, Health: rt.Health,
		Auth: apigw.AuthConfig{
			Keys: issuer, JWTKeys: jwtKeys,
			JWTIssuer: jwtIssuer, JWTAudience: jwtAudience,
		},
		Keys: issuer,
		RateLimit: apigw.RateLimitConfig{
			Tenant:     apigw.Limit{Rate: float64(tenantRate), Burst: float64(tenantBurst)},
			Credential: apigw.Limit{Rate: float64(credRate), Burst: float64(credBurst)},
			Expensive:  apigw.Limit{Rate: float64(costlyRate), Burst: float64(costlyBurst)},
		},
		Upstreams: upstreams,
		Stream: apigw.StreamConfig{
			QueueDepth: streamQueue, PingInterval: streamPing, PongTimeout: streamPong,
		},
		Source: source,
	})
	if err != nil {
		return err
	}
	gw.Start(ctx)

	srv := &http.Server{
		Addr:    httpAddr,
		Handler: gw.Handler(),
		// The header timeout is what actually stops a slow-loris. There is no
		// WriteTimeout: it would apply to the WebSocket stream too, and a
		// deadline on a connection that is meant to live for hours would close
		// every console at the same interval.
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          nil,
	}
	if tlsCert != "" && tlsKey != "" {
		tlsCfg, terr := buildTLS(tlsCert, tlsKey, clientCAFile)
		if terr != nil {
			return terr
		}
		srv.TLSConfig = tlsCfg
	}

	go func() {
		rt.Log.Info("gateway listening", "addr", httpAddr, "tls", srv.TLSConfig != nil,
			"upstreams", len(upstreams), "routes", len(apigw.Routes()))
		var serveErr error
		if srv.TLSConfig != nil {
			serveErr = srv.ListenAndServeTLS("", "")
		} else {
			serveErr = srv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			rt.Log.Error("http server stopped", "error", serveErr)
			cancel()
		}
	}()

	rt.OnShutdown(func(shutdownCtx context.Context) error {
		// WebSockets first: hijacked connections are invisible to
		// http.Server.Shutdown, so a graceful HTTP drain would return with
		// every stream still open and then have them killed by process exit —
		// which a client sees as a reset rather than a "going away".
		if err := gw.Shutdown(shutdownCtx); err != nil {
			rt.Log.Warn("stream drain incomplete", "error", err)
		}
		if err := srv.Shutdown(shutdownCtx); err != nil {
			rt.Log.Error("http shutdown failed", "error", err)
		}
		cancel()
		return nil
	})

	rt.Ready()
	rt.WaitForSignal(shutdownGrace)
	return nil
}

// loadJWKS reads the published verification key set.
//
// An absent path is not an error: a deployment that authenticates only with
// API keys and client certificates should not be forced to carry a JWKS, and
// the authenticator refuses bearer tokens outright when it has no keys.
func loadJWKS(path string, log *obs.Logger) (*pki.JWTKeySet, error) {
	if path == "" {
		log.Info("no JWKS configured; bearer tokens will not be accepted")
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the JWKS at %s: %w", path, err)
	}
	set, err := pki.ParseJWKS(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing the JWKS at %s: %w", path, err)
	}
	log.Info("jwks loaded", "path", path, "keys", set.Len())
	return set, nil
}

// bootstrap mints the first owner key for a fresh environment.
//
// It exists because a gateway whose only credential-issuing endpoint requires
// a credential cannot be started. The key is printed once, to stdout, and is
// gated behind an explicit configuration value so it cannot happen by
// accident in production.
func bootstrap(ctx context.Context, issuer *apigw.KeyIssuer, tenant canon.TenantID, log *obs.Logger) error {
	rec, plaintext, err := issuer.Issue(ctx, apigw.IssueRequest{
		TenantID: tenant, Name: "bootstrap-owner",
		Roles: []apigw.Role{apigw.RoleOwner}, TTL: 24 * time.Hour,
		CreatedBy: "bootstrap",
	})
	if err != nil {
		return fmt.Errorf("issuing the bootstrap key: %w", err)
	}
	log.Warn("bootstrap owner key issued; it is valid for 24 hours and is printed once",
		"tenant", string(tenant), "key_id", rec.KeyID)
	fmt.Fprintf(os.Stdout, "USSLP bootstrap key for tenant %s: %s\n", tenant, plaintext)
	return nil
}

// buildTLS assembles the server's TLS configuration.
//
// Client certificates are requested but not required, because the same
// listener serves the console to browsers that have none. A certificate that
// is presented is fully verified against the platform's client CA; the
// authenticator then refuses anything that is not a tenant identity.
func buildTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading the server certificate: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if clientCAFile != "" {
		pem, rerr := os.ReadFile(clientCAFile)
		if rerr != nil {
			return nil, fmt.Errorf("reading the client CA bundle: %w", rerr)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("client CA bundle %s contains no certificates", clientCAFile)
		}
		cfg.ClientCAs = pool
		// VerifyClientCertIfGiven, not RequireAndVerify: a browser loading
		// /console has no certificate and must not be rejected at the TLS
		// handshake, while a machine that does present one has it verified
		// before any application code sees it.
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
	}
	return cfg, nil
}
