// Package pki implements the certificate authority hierarchy, device identity,
// mutual-TLS plumbing and price-authority signing keys of the Universal Smart
// Shelf Label Platform.
//
// # Why a private PKI at all
//
// USSLP operates under a zero-trust model (NIST SP 800-207): no device, user or
// service is trusted because of where it sits on the network. A shelf label
// hanging on a rail in a store back-aisle is physically reachable by anyone who
// walks past it, and a store's LAN is one of the least defensible networks in
// commercial computing. The platform therefore assumes that some devices are
// already compromised and designs so that a compromised device cannot change a
// displayed price anywhere — not on its own shelf, and certainly not on anybody
// else's.
//
// Three properties deliver that guarantee, and all three are implemented here:
//
//  1. Every hop is mutually authenticated. A label proves possession of a
//     private key issued by the Manufacturing Sub-CA before a Shelf Edge
//     Controller will talk to it; the SEC proves possession of a key issued by
//     the Shelf Controller Sub-CA before the Store Gateway will relay for it;
//     every cloud service proves a SPIFFE identity before another service will
//     answer it. See [Hierarchy], [ServerTLSConfig] and [ClientTLSConfig].
//
//  2. Identity is structured, not free-text. A certificate does not merely say
//     "some device"; it says which tenant, which store and which device, in a
//     form the MQTT broker's authorizer can extract in constant time and turn
//     into a topic ACL. See [Identity] and [IdentityFromCertificate].
//
//  3. Authority to change a price is separate from authority to speak on the
//     network. Holding a valid device certificate lets you join the mesh; it
//     does not let you author a price. Prices are signed with the Ed25519
//     price-authority key (see [PriceAuthority]) and verified by the SEC
//     against a published key ring (see [KeyRing]) before a single E-Ink
//     waveform is driven. Compromising every device in a store still yields no
//     ability to display an unauthorised price — only the ability to display
//     nothing, which is detected within three missed heartbeats.
//
// # The hierarchy
//
//	USSLP Root CA                        RSA-4096, 20y, self-signed, offline
//	├── Device Issuance Intermediate     RSA-2048, 10y, pathlen 1
//	│   ├── Manufacturing Sub-CA         pathlen 0 → label certs, ECDSA P-256, 2y
//	│   └── Shelf Controller Sub-CA      pathlen 0 → SEC/SGU certs, RSA-2048, 1y
//	├── Services Intermediate            pathlen 0 → service mTLS, ECDSA P-256, 90d
//	└── Tenant Management Intermediate   pathlen 0 → tenant API certs, JWT signing keys
//
// The path-length constraints are the structural half of property (1): a leaf
// certificate carries cA=FALSE, and the sub-CA above it carries pathlen 0, so
// even an attacker holding a sub-CA key cannot mint a further CA to hide behind.
// A stolen label key is worth exactly one label.
//
// The root is modelled as a key that can be loaded, used to sign intermediates,
// and then dropped from memory ([Hierarchy.DropRootKey]). In production the
// root key never exists on a networked machine at all; the type reflects that
// operational reality rather than papering over it.
//
// # What this package does not do
//
// It makes no authorisation decisions. Deciding that a particular device is
// entitled to a particular identity is the provisioning service's job; this
// package binds an identity that the caller has already authorised to a public
// key whose possession has been proven. That separation is why [CSRRequest]
// carries an [Identity] alongside the CSR and why the subject inside the CSR is
// ignored entirely: a certificate signing request is attacker-controlled input.
//
// The package depends on nothing outside the Go standard library and
// [github.com/usslp/usslp/platform/pkg/canon].
package pki
