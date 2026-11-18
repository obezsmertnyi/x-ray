package xray

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// An in-flight sign-in lives in the browser, not in a replica: the state, the PKCE
// verifier and the OIDC nonce travel in one encrypted cookie, so the provider's
// callback can land on any replica (ADR-068). The key is derived from the OAuth
// client secret every replica reads, the way session hashing is (ADR-047). Replay
// is stopped by the cookie being cleared at the callback, by the provider's code
// being single-use and by PKCE binding that code to the verifier in this cookie.

// loginFlowTTL bounds how long a sign-in may take between the redirect to the
// provider and its callback.
const loginFlowTTL = 10 * time.Minute

// loginFlow is one sign-in in progress.
type loginFlow struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	// UIOrigin is the allowlisted origin to hand the session token to once the
	// callback completes. Empty — the default — redirects to "/" on this host,
	// which is the same-origin case where the cookie is the whole credential.
	UIOrigin string `json:"u,omitempty"`
	// Verifier is the PKCE code verifier (RFC 7636, S256): the provider gets only its
	// hash at the redirect, and the code it returns is good only with the verifier.
	Verifier string `json:"v"`
	// Nonce goes to an OpenID Connect provider at the redirect and must come back in
	// the ID token, which binds the token to this sign-in (OIDC Core 3.1.2.1).
	Nonce   string `json:"n"`
	Expires int64  `json:"e"`
}

// newNonce mints a flow's OIDC nonce; a variable so tests can fix it.
var newNonce = func() string { return randomHex(16) }

// idTokenNonce returns the nonce claim of an ID token received from the provider's
// token endpoint over TLS (its signature is not re-verified, OIDC Core 3.1.3.7).
func idTokenNonce(idToken string) (string, bool) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var c struct {
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(payload, &c) != nil || c.Nonce == "" {
		return "", false
	}
	return c.Nonce, true
}

// nonceMatches reports whether an ID token carries the flow's nonce.
func (f loginFlow) nonceMatches(idToken string) bool {
	n, ok := idTokenNonce(idToken)
	return ok && f.Nonce != "" && subtle.ConstantTimeCompare([]byte(n), []byte(f.Nonce)) == 1
}

// challenge is the PKCE S256 code challenge of the flow's verifier.
func (f loginFlow) challenge() string {
	sum := sha256.Sum256([]byte(f.Verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// processLoginKey is used only when no OAuth client secret is set (local dev), where
// there is one process and no key to share.
var processLoginKey = func() [32]byte {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return k
}()

// loginAEAD returns AES-256-GCM keyed by a derivation of the session key, domain-
// separated from its other uses (token HMAC, identity sealing).
func loginAEAD() cipher.AEAD {
	sessions.mu.Lock()
	base := sessions.hmacKey
	sessions.mu.Unlock()
	key := processLoginKey
	if len(base) > 0 {
		key = sha256.Sum256(append(append([]byte(nil), base...), []byte("login-flow")...))
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic("aes: " + err.Error()) // unreachable: the key is always 32 bytes
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic("gcm: " + err.Error())
	}
	return gcm
}

// loginFlowAAD binds a sealed flow to its purpose, format version and provider.
func loginFlowAAD(provider string) []byte { return []byte("xray-login-flow-v1:" + provider) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// startLoginFlow begins a sign-in with provider: it mints the state and sets the
// cookie that carries the flow. base is BasePath(PublicURL).
func startLoginFlow(w http.ResponseWriter, base, provider, uiOrigin string) loginFlow {
	verifier := make([]byte, 32)
	if _, err := rand.Read(verifier); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	f := loginFlow{
		Provider: provider, State: randomHex(16), UIOrigin: uiOrigin,
		Verifier: base64.RawURLEncoding.EncodeToString(verifier), // 43 characters, RFC 7636 4.1
		Nonce:    newNonce(),
		Expires:  time.Now().Add(loginFlowTTL).Unix(),
	}
	plain, err := json.Marshal(f)
	if err != nil {
		panic("login flow: " + err.Error()) // unreachable: plain struct of strings and an int
	}
	gcm := loginAEAD()
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	sealed := gcm.Seal(nonce, nonce, plain, loginFlowAAD(provider))
	// Secure+HttpOnly+SameSite=Lax matches the session cookie; Lax is required so
	// the cookie survives the cross-site redirect back from the provider.
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    base64.RawURLEncoding.EncodeToString(sealed),
		Path:     base + "/v1/auth/",
		MaxAge:   int(loginFlowTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	return f
}

// finishLoginFlow ends a sign-in at provider's callback: it clears the cookie and
// returns the flow when the cookie opens under this deployment's key, was issued
// for this provider, has not expired and carries queryState.
func finishLoginFlow(w http.ResponseWriter, r *http.Request, base, provider, queryState string) (loginFlow, bool) {
	clearStateCookie(w, base)
	if queryState == "" {
		return loginFlow{}, false
	}
	c, err := r.Cookie(oauthStateCookie)
	if err != nil {
		return loginFlow{}, false
	}
	sealed, err := base64.RawURLEncoding.DecodeString(c.Value)
	gcm := loginAEAD()
	if err != nil || len(sealed) < gcm.NonceSize() {
		return loginFlow{}, false
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], loginFlowAAD(provider))
	if err != nil {
		return loginFlow{}, false
	}
	var f loginFlow
	if json.Unmarshal(plain, &f) != nil || f.Provider != provider || time.Now().Unix() >= f.Expires {
		return loginFlow{}, false
	}
	if subtle.ConstantTimeCompare([]byte(f.State), []byte(queryState)) != 1 {
		return loginFlow{}, false
	}
	return f, true
}
