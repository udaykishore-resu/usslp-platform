package pki

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestJWTSignAndVerify covers the token a tenant's integration presents to the
// Universal Integration Gateway after authenticating with its client
// certificate.
func TestJWTSignAndVerify(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	signer, err := h.NewJWTSigner("https://api.usslp.io")
	if err != nil {
		t.Fatalf("NewJWTSigner: %v", err)
	}
	set := NewJWTKeySet()
	kid, err := set.AddSigner(signer)
	if err != nil {
		t.Fatalf("AddSigner: %v", err)
	}
	if kid != signer.KeyID() {
		t.Errorf("key set filed the signer under %q, want %q", kid, signer.KeyID())
	}

	now := time.Now()
	token, err := signer.Sign(JWTClaims{
		Subject:  "user-4711",
		Audience: "uig",
		TenantID: tenantA,
		Scopes:   []string{"prices:write", "labels:read"},
	}, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if n := len(strings.Split(token, ".")); n != 3 {
		t.Fatalf("token has %d segments, want 3", n)
	}

	claims, err := set.Verify(token, JWTVerifyOptions{At: now, Issuer: "https://api.usslp.io", Audience: "uig"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "user-4711" || claims.TenantID != tenantA {
		t.Errorf("claims = %+v", claims)
	}
	if !claims.HasScope("prices:write") || claims.HasScope("prices:admin") {
		t.Errorf("scopes = %v", claims.Scopes)
	}
	if claims.TokenID == "" {
		t.Error("token has no jti; a token that cannot be named cannot be revoked or audited")
	}
	if claims.ExpiresAt <= claims.IssuedAt {
		t.Error("token expiry is not after its issuance")
	}
}

// TestJWTRejections covers the ways a token must fail.
func TestJWTRejections(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	signer, err := h.NewJWTSigner("https://api.usslp.io")
	if err != nil {
		t.Fatalf("NewJWTSigner: %v", err)
	}
	set := NewJWTKeySet()
	if _, err := set.AddSigner(signer); err != nil {
		t.Fatalf("AddSigner: %v", err)
	}
	now := time.Now()
	token, err := signer.Sign(JWTClaims{Subject: "user-1", Audience: "uig", TenantID: tenantA}, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	t.Run("expired", func(t *testing.T) {
		_, err := set.Verify(token, JWTVerifyOptions{At: now.Add(DefaultTokenTTL + time.Minute)})
		if !errors.Is(err, ErrTokenExpired) {
			t.Fatalf("got %v, want ErrTokenExpired", err)
		}
	})

	t.Run("not yet valid", func(t *testing.T) {
		_, err := set.Verify(token, JWTVerifyOptions{At: now.Add(-time.Hour)})
		if !errors.Is(err, ErrTokenExpired) {
			t.Fatalf("got %v, want ErrTokenExpired", err)
		}
	})

	t.Run("wrong audience", func(t *testing.T) {
		_, err := set.Verify(token, JWTVerifyOptions{At: now, Audience: "somewhere-else"})
		if !errors.Is(err, ErrTokenClaims) {
			t.Fatalf("got %v, want ErrTokenClaims", err)
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		parts := strings.Split(token, ".")
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		var claims JWTClaims
		if err := json.Unmarshal(raw, &claims); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		claims.TenantID = tenantB // help yourself to another retailer's data
		reencoded, err := json.Marshal(claims)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(reencoded) + "." + parts[2]
		if _, err := set.Verify(forged, JWTVerifyOptions{At: now}); !errors.Is(err, ErrTokenSignature) {
			t.Fatalf("got %v, want ErrTokenSignature", err)
		}
	})

	t.Run("alg none is refused", func(t *testing.T) {
		parts := strings.Split(token, ".")
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"` + signer.KeyID() + `"}`))
		forged := header + "." + parts[1] + "."
		if _, err := set.Verify(forged, JWTVerifyOptions{At: now}); !errors.Is(err, ErrTokenMalformed) {
			t.Fatalf("got %v, want ErrTokenMalformed", err)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		other, err := h.NewJWTSigner("https://api.usslp.io")
		if err != nil {
			t.Fatalf("NewJWTSigner: %v", err)
		}
		strange, err := other.Sign(JWTClaims{Subject: "user-2"}, now)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if _, err := set.Verify(strange, JWTVerifyOptions{At: now}); !errors.Is(err, ErrTokenSignature) {
			t.Fatalf("got %v, want ErrTokenSignature", err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		for _, bad := range []string{"", "a.b", "a.b.c.d", "!!!.###.$$$"} {
			if _, err := set.Verify(bad, JWTVerifyOptions{At: now}); err == nil {
				t.Errorf("Verify accepted %q", bad)
			}
		}
	})
}

// TestJWKSRoundTrips covers the document every downstream service fetches to
// verify tenant tokens.
func TestJWKSRoundTrips(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	signer, err := h.NewJWTSigner("https://api.usslp.io")
	if err != nil {
		t.Fatalf("NewJWTSigner: %v", err)
	}
	set := NewJWTKeySet()
	if _, err := set.AddSigner(signer); err != nil {
		t.Fatalf("AddSigner: %v", err)
	}
	doc, err := set.PublishJWKS()
	if err != nil {
		t.Fatalf("PublishJWKS: %v", err)
	}
	if strings.Contains(string(doc), `"d"`) {
		t.Fatal("published JWKS contains a private key component")
	}

	downstream, err := ParseJWKS(doc)
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}
	if downstream.Len() != 1 {
		t.Fatalf("parsed set holds %d keys, want 1", downstream.Len())
	}
	now := time.Now()
	token, err := signer.Sign(JWTClaims{Subject: "user-3", TenantID: tenantA}, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := downstream.Verify(token, JWTVerifyOptions{At: now}); err != nil {
		t.Fatalf("downstream verifier: %v", err)
	}

	// Rotation: the old key is removed and its tokens stop verifying.
	downstream.Remove(signer.KeyID())
	if _, err := downstream.Verify(token, JWTVerifyOptions{At: now}); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("after removing the key: got %v, want ErrTokenSignature", err)
	}
}
