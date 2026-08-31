package pki

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestConcurrentUse exercises the package the way a running service does: one
// Hierarchy and one PriceAuthority shared by every goroutine, verifying and
// signing continuously while revocation and key rotation change state
// underneath them. Under -race this is what proves the locking is right; a
// verifier that had to be quiesced to publish a CRL or rotate a key would be
// useless in a platform that never stops serving.
func TestConcurrentUse(t *testing.T) {
	t.Parallel()

	p := TestProfile()
	h, err := Bootstrap(BootstrapConfig{Profile: &p})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	pa, err := NewPriceAuthority(PriceAuthorityConfig{})
	if err != nil {
		t.Fatalf("NewPriceAuthority: %v", err)
	}

	// A pool of already-issued certificates, so the loops below measure
	// concurrent verification rather than concurrent key generation.
	const pool = 8
	issued := make([]*Issued, pool)
	for i := range issued {
		issued[i] = mustIssueLabel(t, h, tenantA, store1, canon.LabelID("label-conc-"+string(rune('a'+i))))
	}

	const workers = 8
	const iterations = 25
	var wg sync.WaitGroup
	errCh := make(chan error, workers*4)

	// Verifiers: the TLS callback's hot path.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				cert := issued[(w+i)%pool]
				if _, err := h.VerifyChain(cert.Certificate, cert.Chain, VerifyOptions{}); err != nil &&
					!errors.Is(err, ErrRevoked) {
					errCh <- err
					return
				}
			}
		}(w)
	}

	// Issuers: the provisioning path.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations/5; i++ {
				if _, _, err := h.IssueService("usslp-prod", "concurrent-worker"); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}

	// Revokers and CRL publishers: the state that changes underneath everyone.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations/5; i++ {
				cert := issued[(w+i)%pool].Certificate
				if err := h.RevokeCertificate(cert, ReasonSuperseded, time.Now()); err != nil {
					errCh <- err
					return
				}
				if _, err := h.GenerateCRL(RoleManufacturing, CRLOptions{}); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}

	// Signers and rotators: the price authority under load.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				input := priceInput(int64(100+i), int64(i))
				att, err := pa.Sign(input)
				if err != nil {
					errCh <- err
					return
				}
				ring, err := pa.KeyRing()
				if err != nil {
					errCh <- err
					return
				}
				// The key may have rotated between signing and verifying, which
				// is exactly the race the overlap exists to survive.
				if err := ring.Verify(input, att); err != nil {
					errCh <- err
					return
				}
				if w == 0 && i%10 == 9 {
					if _, err := pa.Rotate(time.Now()); err != nil {
						errCh <- err
						return
					}
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent use: %v", err)
	}
}
