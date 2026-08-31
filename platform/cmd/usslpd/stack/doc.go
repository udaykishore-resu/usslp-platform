// Package stack assembles every USSLP component into one operating system
// process: one event log, one cloud MQTT broker, the eight cloud services, one
// certificate hierarchy, and N store gateways each with its controllers and its
// simulated label fleet.
//
// # Why a single-process deployment shape exists
//
// USSLP's production topology is nine binaries in multi-region Kubernetes
// behind Kafka, EMQX, Aurora and ClickHouse. That is the right shape for
// 100,000 stores and it is what deploy/helm and deploy/terraform provision. It
// is the wrong shape for three other situations, and this package is for those:
//
//   - Development. A developer changing the attestation path wants to see a
//     price reach a label, not to operate a nine-container compose file. More
//     concretely, the multi-process dev profile cannot deliver the UIG's
//     `price-updates` records to the Label Service at all: pkg/eventlog keeps
//     consumer-group coordination in memory (platform/pkg/eventlog/consumer.go),
//     so two OS processes must not share one log directory, and in compose each
//     service therefore has its own log. deploy/README.md says so. Here every
//     service is handed the same *eventlog.Log value, so the cross-service
//     event stream is real.
//
//   - A lab or a demonstration. The end-to-end claims — three seconds from POS
//     to glass, a store that keeps trading through a WAN outage — are only
//     believable if someone can run them. test/e2e boots this package and
//     asserts on them; `make demo` boots it and narrates them.
//
//   - A single store or a disconnected pilot. A store with one gateway does not
//     need a 1,024-partition Kafka cluster or a 200-node consumer group; it
//     needs correct prices and it needs them when the WAN is down. Every
//     component here is the production component — the same Label Service, the
//     same attestation, the same MQTT broker implementation, the same
//     zero-touch provisioning — assembled differently. Nothing is stubbed or
//     bypassed. What differs is the dial: the stream catalogue is provisioned
//     with dev-sized partition counts (see devStreams), and the store's labels
//     are edge/labelsim rather than silicon.
//
// # What is genuinely exercised
//
// The whole price path, in the order the interface contract defines it:
//
//	POS webhook (the real Shopify adapter, HMAC verified)
//	  -> UIG pipeline: dedupe, normalise, validate, durable append
//	  -> price-updates on the shared event log
//	  -> Label Service: resolve labels from the directory read model, sign the
//	     price with the tenant's Ed25519 price-authority key
//	  -> cloud MQTT broker, QoS 1, retained, on the owning controller's topic
//	  -> Store Gateway Unit bridge -> the store's own MQTT broker
//	  -> Shelf Edge Controller: recompute the digest, verify against the key
//	     ring, render, choose the waveform, transmit over the Zigbee model
//	  -> label: sequence check, E-Ink refresh, acknowledge
//	  -> back up the bridge -> label-delivery -> the SLO read model.
//
// The attestation is real end to end. The key ring the controllers verify
// against is published from the same price authority the Label Service signs
// with; tamper with a price in flight and the label keeps the price it had. See
// test/e2e/attestation_test.go.
//
// # What is not
//
// The labels are simulated (edge/labelsim over edge/sim's discrete-event
// engine) and the radio is modelled (edge/mesh), because the alternative is a
// warehouse of hardware. The simulation is honest about airtime, retries, duty
// cycling and E-Ink waveform duration, which is why the latencies this package
// measures are worth measuring; it is not a substitute for a field trial. The
// event log and the MQTT broker are the platform's own implementations standing
// in for Kafka and EMQX behind pkg/eventbus and pkg/msgbus.
package stack
