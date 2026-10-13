package mcpevents

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
)

func Principal(ownerKey string) string {
	sum := sha256.Sum256([]byte(ownerKey))
	return "owner:" + hex.EncodeToString(sum[:8])
}
func subscriptionID(principal, name string, args []byte, callback string) string {
	h := sha256.New()
	for _, field := range [][]byte{[]byte(principal), []byte(name), args, []byte(callback)} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		h.Write(length[:])
		h.Write(field)
	}
	return "sub_" + hex.EncodeToString(h.Sum(nil))
}
func tokenMAC(key []byte, input string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(input))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:16])
}
func encodeCursor(key []byte, id string, epoch, seq int64) string {
	prefix := "c1." + strconv.FormatInt(epoch, 10) + "." + strconv.FormatInt(seq, 10)
	return prefix + "." + tokenMAC(key, "c1."+id+"."+strconv.FormatInt(epoch, 10)+"."+strconv.FormatInt(seq, 10))
}
func encodeEventID(key []byte, id string, seq int64) string {
	prefix := "evt1." + id + "." + strconv.FormatInt(seq, 10)
	return prefix + "." + tokenMAC(key, prefix)
}
func decimal(s string) (int64, error) {
	if s == "" || len(s) > 19 || (len(s) > 1 && s[0] == '0') {
		return 0, invalid("invalid_identifier")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, invalid("invalid_identifier")
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, invalid("invalid_identifier")
	}
	return v, nil
}
func validSubscriptionID(id string) bool {
	if len(id) != 68 || !strings.HasPrefix(id, "sub_") {
		return false
	}
	for _, r := range id[4:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
func validMAC(got, want string) bool {
	if len(got) != 22 {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(got)
	return err == nil && len(b) == 16 && hmac.Equal([]byte(got), []byte(want))
}
func decodeCursor(key []byte, id, token string) (int64, int64, error) {
	if len(token) > 69 || !validSubscriptionID(id) {
		return 0, 0, invalid("invalid_cursor")
	}
	p := strings.Split(token, ".")
	if len(p) != 4 || p[0] != "c1" {
		return 0, 0, invalid("invalid_cursor")
	}
	e, err := decimal(p[1])
	if err != nil {
		return 0, 0, invalid("invalid_cursor")
	}
	n, err := decimal(p[2])
	if err != nil {
		return 0, 0, invalid("invalid_cursor")
	}
	if !validMAC(p[3], tokenMAC(key, "c1."+id+"."+p[1]+"."+p[2])) {
		return 0, 0, invalid("invalid_cursor")
	}
	return e, n, nil
}
func decodeEventID(key []byte, token string) (string, int64, error) {
	if len(token) > 119 {
		return "", 0, invalid("invalid_event_id")
	}
	p := strings.Split(token, ".")
	if len(p) != 4 || p[0] != "evt1" || !validSubscriptionID(p[1]) {
		return "", 0, invalid("invalid_event_id")
	}
	n, err := decimal(p[2])
	if err != nil || !validMAC(p[3], tokenMAC(key, strings.Join(p[:3], "."))) {
		return "", 0, invalid("invalid_event_id")
	}
	return p[1], n, nil
}
func decodeSecret(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, "whsec_") || len(secret) > 94 {
		return nil, invalid("invalid_secret")
	}
	b, err := base64.StdEncoding.Strict().DecodeString(secret[6:])
	if err != nil || len(b) < 24 || len(b) > 64 || base64.StdEncoding.EncodeToString(b) != secret[6:] {
		return nil, invalid("invalid_secret")
	}
	return b, nil
}
func webhookSignature(secret []byte, id, timestamp string, body []byte) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(id + "." + timestamp + "."))
	h.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(h.Sum(nil))
}
func secretCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, &Error{Code: -32015, Reason: "events_key_unavailable"}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, &Error{Code: -32015, Reason: "events_key_unavailable"}
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, &Error{Code: -32015, Reason: "events_key_unavailable"}
	}
	return aead, nil
}
func encryptSecret(key []byte, id, role string, plain []byte) ([]byte, error) {
	a, err := secretCipher(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, &Error{Code: -32015, Reason: "events_key_unavailable"}
	}
	return a.Seal(nonce, nonce, plain, []byte(id+"."+role)), nil
}
func decryptSecret(key []byte, id, role string, enc []byte) ([]byte, error) {
	a, err := secretCipher(key)
	if err != nil {
		return nil, err
	}
	if len(enc) < a.NonceSize()+a.Overhead() {
		return nil, &Error{Code: -32015, Reason: "events_key_unavailable"}
	}
	plain, err := a.Open(nil, enc[:a.NonceSize()], enc[a.NonceSize():], []byte(id+"."+role))
	if err != nil {
		return nil, &Error{Code: -32015, Reason: "events_key_unavailable"}
	}
	return plain, nil
}
