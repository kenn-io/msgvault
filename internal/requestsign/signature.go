// Package requestsign implements msgvault's fixed RFC 9421 HMAC-SHA256 profile.
package requestsign

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultMaxRequestBytes int64 = 16 << 20
	MaxRequestBytes        int64 = 64 << 20
	Lifetime                     = 30 * time.Second
	FutureSkew                   = 5 * time.Second
	components                   = `("@method" "@target-uri" "content-digest" "content-type" "x-api-key")`
)

var (
	ErrInvalidSignature = errors.New("invalid request signature")
	idPattern           = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	timePattern         = regexp.MustCompile(`^[1-9][0-9]{0,11}$`)
)

// Metadata contains only the authenticated profile parameters, not credentials.
type Metadata struct {
	KeyID           string
	Nonce           string
	Created         int64
	Expires         int64
	Digest          string
	signatureParams string
}

func ValidID(id string) bool { return idPattern.MatchString(id) }

// NewNonce returns 24 cryptographically random bytes encoded without padding.
func NewNonce() string {
	var bytes [24]byte
	_, err := rand.Read(bytes[:])
	if err != nil {
		panic("request signing random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:])
}

// SingleHeader rejects duplicate, empty and whitespace-ambiguous covered fields.
func SingleHeader(h http.Header, name string) (string, error) {
	var values []string
	for key, v := range h {
		if strings.EqualFold(key, name) {
			values = append(values, v...)
		}
	}
	if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] {
		return "", ErrInvalidSignature
	}
	for _, b := range []byte(values[0]) {
		if b < 0x20 || b > 0x7e {
			return "", ErrInvalidSignature
		}
	}
	return values[0], nil
}

// ParseMetadata accepts exactly the fixed profile, with no extra dictionary
// members or parameters. It does not authenticate the returned metadata.
func ParseMetadata(r *http.Request) (Metadata, error) {
	input, err := SingleHeader(r.Header, "Signature-Input")
	if err != nil || len(input) > 512 {
		return Metadata{}, ErrInvalidSignature
	}
	prefix := "sig1=" + components + ";"
	if !strings.HasPrefix(input, prefix) {
		return Metadata{}, ErrInvalidSignature
	}
	parameters := strings.Split(strings.TrimPrefix(input, prefix), ";")
	if len(parameters) != 5 {
		return Metadata{}, ErrInvalidSignature
	}
	seen := make(map[string]bool, 5)
	m := Metadata{signatureParams: strings.TrimPrefix(input, "sig1=")}
	for _, parameter := range parameters {
		name, value, ok := strings.Cut(parameter, "=")
		if !ok || seen[name] {
			return Metadata{}, ErrInvalidSignature
		}
		seen[name] = true
		switch name {
		case "created", "expires":
			if !timePattern.MatchString(value) {
				return Metadata{}, ErrInvalidSignature
			}
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return Metadata{}, ErrInvalidSignature
			}
			if name == "created" {
				m.Created = number
			} else {
				m.Expires = number
			}
		case "nonce", "keyid":
			if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
				return Metadata{}, ErrInvalidSignature
			}
			value = value[1 : len(value)-1]
			if name == "keyid" {
				if !ValidID(value) {
					return Metadata{}, ErrInvalidSignature
				}
				m.KeyID = value
			} else {
				nonce, err := base64.RawURLEncoding.DecodeString(value)
				if err != nil || len(nonce) != 24 || base64.RawURLEncoding.EncodeToString(nonce) != value {
					return Metadata{}, ErrInvalidSignature
				}
				m.Nonce = value
			}
		case "alg":
			if value != `"hmac-sha256"` {
				return Metadata{}, ErrInvalidSignature
			}
		default:
			return Metadata{}, ErrInvalidSignature
		}
	}
	if m.Expires-m.Created != int64(Lifetime/time.Second) {
		return Metadata{}, ErrInvalidSignature
	}
	return m, nil
}

func transformations(r *http.Request) bool {
	return len(r.Trailer) > 0 || len(r.Header.Values("Trailer")) > 0 || len(r.Header.Values("Content-Encoding")) > 0
}

func params(m Metadata) string {
	if m.signatureParams != "" {
		return m.signatureParams
	}
	return fmt.Sprintf(`%s;created=%d;expires=%d;nonce="%s";alg="hmac-sha256";keyid="%s"`, components, m.Created, m.Expires, m.Nonce, m.KeyID)
}

func signatureBase(r *http.Request, target string, m Metadata) (string, error) {
	if transformations(r) || len(target) > 8192 {
		return "", ErrInvalidSignature
	}
	if _, err := ValidateTarget(target); err != nil {
		return "", err
	}
	if r.Method == "" {
		return "", ErrInvalidSignature
	}
	for _, b := range []byte(r.Method) {
		if b < 'A' || b > 'Z' {
			return "", ErrInvalidSignature
		}
	}
	digest, err := SingleHeader(r.Header, "Content-Digest")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(digest, "sha-256=:") || !strings.HasSuffix(digest, ":") {
		return "", ErrInvalidSignature
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(digest, "sha-256=:"), ":"))
	if err != nil || len(decoded) != sha256.Size || digest != "sha-256=:"+base64.StdEncoding.EncodeToString(decoded)+":" {
		return "", ErrInvalidSignature
	}
	contentType, err := SingleHeader(r.Header, "Content-Type")
	if err != nil {
		return "", err
	}
	apiKey, err := SingleHeader(r.Header, "X-Api-Key")
	if err != nil {
		return "", err
	}
	return "\"@method\": " + r.Method + "\n\"@target-uri\": " + target + "\n\"content-digest\": " + digest + "\n\"content-type\": " + contentType + "\n\"x-api-key\": " + apiKey + "\n\"@signature-params\": " + params(m), nil
}

// Sign hashes a bounded body and signs the externally addressed native request.
func Sign(r *http.Request, secret []byte, keyID string, now time.Time, nonce string) error {
	return sign(r, secret, keyID, func() time.Time { return now }, nonce, DefaultMaxRequestBytes)
}

func sign(r *http.Request, secret []byte, keyID string, clock func() time.Time, nonce string, maxBytes int64) error {
	if len(secret) != 64 || !ValidID(keyID) {
		return ErrInvalidSignature
	}
	n, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(n) != 24 || base64.RawURLEncoding.EncodeToString(n) != nonce {
		return ErrInvalidSignature
	}
	if _, err := SingleHeader(r.Header, "X-Api-Key"); err != nil {
		return err
	}
	if transformations(r) {
		return ErrInvalidSignature
	}
	if _, err := ValidateTarget(r.URL.String()); err != nil {
		return err
	}
	if len(r.Header.Values("Content-Type")) == 0 {
		r.Header.Set("Content-Type", "application/octet-stream")
	}
	digest, cleanup, err := Spool(r, maxBytes)
	if err != nil {
		return err
	}
	now := clock()
	m := Metadata{Created: now.Unix(), Expires: now.Unix() + 30, Nonce: nonce, KeyID: keyID}
	r.Header.Set("Content-Digest", digest)
	base, err := signatureBase(r, r.URL.String(), m)
	if err != nil {
		cleanup()
		return err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(base))
	r.Header.Set("Signature-Input", "sig1="+params(m))
	r.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(mac.Sum(nil))+":")
	return nil
}

// VerifyHeaders authenticates the MAC and freshness before reading any body.
func VerifyHeaders(r *http.Request, target string, secret []byte, keyID string, now time.Time) (Metadata, error) {
	m, err := ParseMetadata(r)
	if err != nil || len(secret) != 64 || m.KeyID != keyID || !now.Before(time.Unix(m.Expires, 0)) || m.Created > now.Unix()+5 {
		return Metadata{}, ErrInvalidSignature
	}
	base, err := signatureBase(r, target, m)
	if err != nil {
		return Metadata{}, err
	}
	signature, err := SingleHeader(r.Header, "Signature")
	if err != nil || !strings.HasPrefix(signature, "sig1=:") || !strings.HasSuffix(signature, ":") {
		return Metadata{}, ErrInvalidSignature
	}
	given, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(signature, "sig1=:"), ":"))
	if err != nil || len(given) != sha256.Size || signature != "sig1=:"+base64.StdEncoding.EncodeToString(given)+":" {
		return Metadata{}, ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(base))
	if !hmac.Equal(mac.Sum(nil), given) {
		return Metadata{}, ErrInvalidSignature
	}
	m.Digest = r.Header.Get("Content-Digest")
	return m, nil
}

// Verify is the bounded complete-request convenience verifier. Server ingress
// uses VerifyHeaders before spooling and admitting a nonce itself.
func Verify(r *http.Request, target string, secret []byte, keyID string, now time.Time) (Metadata, error) {
	m, err := VerifyHeaders(r, target, secret, keyID, now)
	if err != nil {
		return Metadata{}, err
	}
	digest, cleanup, err := Spool(r, DefaultMaxRequestBytes)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return Metadata{}, err
	}
	if !hmac.Equal([]byte(digest), []byte(m.Digest)) {
		return Metadata{}, ErrInvalidSignature
	}
	return m, nil
}
