package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// TestNoCrossTenantLeakage runs two retailers in one runtime and asserts that
// neither can see the other on any of the three surfaces they share: the HTTP
// API, the event stream, and the MQTT namespace.
//
// Two tenants in one process is the hardest case for isolation, because there
// is no network, no cluster and no namespace between them — only the code. If
// tenancy holds here it holds when the two are in different clusters.
func TestNoCrossTenantLeakage(t *testing.T) {
	if testing.Short() {
		t.Skip("two tenants means two stores and two label fleets; -short skips it")
	}
	st := newStack(t, stack.Config{
		Tenants:             []canon.TenantID{"alpha-grocers", "beta-markets"},
		Stores:              1,
		ControllersPerStore: 1,
		LabelsPerController: 6,
	})
	stores := st.Stores()
	if len(stores) != 2 {
		t.Fatalf("expected one store per tenant, got %d", len(stores))
	}
	alpha, beta := stores[0], stores[1]
	if alpha.Tenant == beta.Tenant {
		t.Fatal("both stores belong to the same tenant")
	}
	t.Logf("tenants: %s (%s) and %s (%s)", alpha.Tenant, alpha.ID, beta.Tenant, beta.ID)

	// A distinguishable price change in each tenant, so there is something
	// specific to leak.
	alphaTarget := pick(t, st, 0, 0, 0)
	betaTarget := pick(t, st, 1, 0, 0)
	alphaPrice := alphaTarget.nudge(111)
	betaPrice := betaTarget.nudge(222)
	pushPrice(t, st, alphaTarget, alphaPrice)
	pushPrice(t, st, betaTarget, betaPrice)

	// --- 1. the HTTP API ---------------------------------------------
	//
	// The Label Service takes the tenant from X-USSLP-Tenant, which the API
	// Gateway sets from the authenticated credential. Asking for alpha's store
	// while presenting beta's tenant must not return alpha's shelves.
	t.Run("api", func(t *testing.T) {
		own := labelServiceStoreLabels(t, st, alpha.Tenant, alpha.ID)
		if own == 0 {
			t.Fatalf("%s cannot see its own store", alpha.Tenant)
		}
		crossed := labelServiceStoreLabels(t, st, beta.Tenant, alpha.ID)
		if crossed != 0 {
			t.Errorf("%s could see %d labels in %s's store %s",
				beta.Tenant, crossed, alpha.Tenant, alpha.ID)
		}
		t.Logf("%s sees %d labels in its own store and %d in the other tenant's",
			alpha.Tenant, own, crossed)

		// And the directory the fan-out is resolved from agrees.
		ps, err := st.Services().Label().Directory().LabelsForSKU(
			t.Context(), beta.Tenant, alpha.ID, alphaTarget.SKU)
		if err == nil && len(ps) > 0 {
			t.Errorf("the fan-out directory resolved %d of %s's labels for tenant %s",
				len(ps), alpha.Tenant, beta.Tenant)
		}
	})

	// --- 2. the event stream ------------------------------------------
	//
	// Every envelope carries its tenant, and a consumer that filters on it must
	// find no record of the other. The check is that the streams are
	// *attributable*, not that they are physically separate: one Kafka cluster
	// serves every tenant in production, and the isolation is the tenant field
	// plus the consumer's own filtering.
	t.Run("stream", func(t *testing.T) {
		records := drainStream(t, st, canon.StreamPriceUpdates.Name, "e2e-tenancy", 3*time.Second)
		if len(records) == 0 {
			t.Fatal("no price records were on the stream at all")
		}
		byTenant := map[canon.TenantID]int{}
		for _, env := range records {
			byTenant[env.TenantID]++
			// A record must never name one tenant and carry another's store.
			if env.StoreID != "" {
				want := alpha.Tenant
				if strings.HasPrefix(string(env.StoreID), string(beta.Tenant)) {
					want = beta.Tenant
				}
				if env.TenantID != want {
					t.Errorf("a record for store %s is attributed to tenant %s",
						env.StoreID, env.TenantID)
				}
			}
		}
		t.Logf("price-updates records by tenant: %v", byTenant)
		if byTenant[alpha.Tenant] == 0 || byTenant[beta.Tenant] == 0 {
			t.Error("one of the tenants produced no records; the test proves nothing")
		}
	})

	// --- 3. the MQTT namespace ----------------------------------------
	//
	// INTERFACE-CONTRACTS §3 puts the tenant immediately below the root so that
	// one ACL rule — subscribe only to usslp/{your-tenant}/# — is complete
	// isolation. The test subscribes with exactly that filter and asserts it
	// catches its own tenant's traffic and none of the other's.
	t.Run("mqtt", func(t *testing.T) {
		// Both stores bridge from the same cloud broker, which is where a
		// cross-tenant subscription would do the most damage.
		client, err := mqtt.Dial(t.Context(), msgbus.Config{
			BrokerURL: st.CloudBrokerURL(), ClientID: "e2e-tenancy-alpha",
			CleanSession: true,
		})
		if err != nil {
			t.Fatalf("connecting to the cloud broker: %v", err)
		}
		defer client.Close()

		// Topics are captured as well as payloads. The topic is the isolation
		// mechanism — one ACL rule on a prefix — and not every message in a
		// tenant's namespace is a canon.Envelope: heartbeats, retained status
		// and the gateway's own probe are not, and treating an unparseable
		// payload as "no tenant" would report the store's own heartbeat as a
		// leak.
		type received struct {
			topic  string
			tenant canon.TenantID
		}
		seen := make(chan received, 512)
		filter := canon.SubscribeTenant(alpha.Tenant)
		if err := client.Subscribe(t.Context(), filter, canon.QoSPrice,
			func(_ context.Context, m msgbus.Message) {
				r := received{topic: m.Topic}
				var env canon.Envelope
				if json.Unmarshal(m.Payload, &env) == nil {
					r.tenant = env.TenantID
				}
				select {
				case seen <- r:
				default:
				}
			}); err != nil {
			t.Fatalf("subscribing to %s: %v", filter, err)
		}

		// New traffic in both tenants after the subscription is installed.
		a2 := pick(t, st, 0, 0, 1)
		b2 := pick(t, st, 1, 0, 1)
		pushPrice(t, st, a2, a2.nudge(17))
		pushPrice(t, st, b2, b2.nudge(19))
		time.Sleep(time.Second)

		// The channel is drained rather than closed: the broker's delivery
		// goroutine is still live until the client is closed, and closing a
		// channel it may write to is a panic waiting for a slow machine.
		prefix := canon.MQTTRoot + "/" + string(alpha.Tenant) + "/"
		own, other := 0, 0
		for {
			var r received
			select {
			case r = <-seen:
			default:
			}
			if r.topic == "" {
				break
			}
			if !strings.HasPrefix(r.topic, prefix) {
				other++
				t.Errorf("a subscription to %s received a message on %s", filter, r.topic)
				continue
			}
			if r.tenant != "" && r.tenant != alpha.Tenant {
				other++
				t.Errorf("a message on %s carries tenant %s", r.topic, r.tenant)
				continue
			}
			own++
		}
		t.Logf("subscribing to %s saw %d messages, all in its own namespace; %d leaked",
			filter, own, other)
		if own == 0 {
			t.Errorf("the tenant wildcard %s saw none of its own traffic; "+
				"the test would pass for the wrong reason", filter)
		}
	})

	// --- 4. the credentials -------------------------------------------
	//
	// Each tenant's API key is minted for that tenant alone.
	t.Run("credentials", func(t *testing.T) {
		ak := st.Services().APIKey(alpha.Tenant)
		bk := st.Services().APIKey(beta.Tenant)
		if ak == "" || bk == "" {
			t.Fatal("a tenant has no API key")
		}
		if ak == bk {
			t.Fatal("both tenants were issued the same API key")
		}
	})
}

// labelServiceStoreLabels asks the Label Service for a store's roster as a
// given tenant, the way the API Gateway does.
func labelServiceStoreLabels(t *testing.T, st *stack.Stack, tenant canon.TenantID, store canon.StoreID) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		st.Services().LabelURL()+"/v1/stores/"+string(store)+"/labels", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-USSLP-Tenant", string(tenant))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("listing %s's labels as %s: %v", store, tenant, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return 0
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing %s's labels as %s: status %s: %s", store, tenant, resp.Status, body)
	}
	var out struct {
		Labels []json.RawMessage `json:"labels"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding the label roster: %v: %s", err, body)
	}
	return len(out.Labels)
}

// drainStream reads everything currently on a stream.
func drainStream(t *testing.T, st *stack.Stack, streamName, group string, within time.Duration) []canon.Envelope {
	t.Helper()
	consumer, err := st.EventLog().Subscribe(eventbus.SubscribeOptions{
		Group: group, Topics: []string{streamName}, FromBeginning: true,
	})
	if err != nil {
		t.Fatalf("subscribing to %s: %v", streamName, err)
	}
	defer consumer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	// The handler runs once per partition, concurrently, so the accumulator is
	// guarded. A consumer group is a fan-out by design and a test that forgot
	// that would be a test with a race in it rather than a test of a race.
	var mu sync.Mutex
	var out []canon.Envelope
	_ = consumer.Run(ctx, func(_ context.Context, m eventbus.Message) error {
		var env canon.Envelope
		if json.Unmarshal(m.Value, &env) != nil {
			return nil
		}
		mu.Lock()
		out = append(out, env)
		mu.Unlock()
		return nil
	})
	mu.Lock()
	defer mu.Unlock()
	return append([]canon.Envelope(nil), out...)
}
