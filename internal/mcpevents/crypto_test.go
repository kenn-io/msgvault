package mcpevents

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
)

func TestProfileKnownVectors(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	b, err := os.ReadFile("../../testdata/mcp/events-profile.json")
	require.NoError(err)
	var p struct {
		ServerKey string            `json:"server_key_hex"`
		OwnerKey  string            `json:"owner_key"`
		Principal string            `json:"principal"`
		Name      string            `json:"name"`
		Arguments map[string]any    `json:"arguments"`
		Canonical string            `json:"canonical_arguments"`
		URL       string            `json:"callback_url"`
		ID        string            `json:"subscription_id"`
		Epoch     int64             `json:"epoch"`
		Seq       int64             `json:"sequence"`
		EventID   string            `json:"event_id"`
		Cursor    string            `json:"cursor"`
		Secret    string            `json:"webhook_secret"`
		Body      string            `json:"body"`
		Headers   map[string]string `json:"headers"`
		Envelope  Envelope          `json:"envelope"`
	}
	require.NoError(json.Unmarshal(b, &p))
	key, err := hex.DecodeString(p.ServerKey)
	require.NoError(err)
	args, err := canonicalArguments(p.Name, p.Arguments)
	require.NoError(err)
	assert.Equal(p.Canonical, string(args.bytes))
	assert.Equal(p.Principal, Principal(p.OwnerKey))
	assert.Equal(p.ID, subscriptionID(p.Principal, p.Name, args.bytes, p.URL))
	assert.Equal(p.Cursor, encodeCursor(key, p.ID, p.Epoch, p.Seq))
	assert.Equal(p.EventID, encodeEventID(key, p.ID, p.Seq))
	secret, err := decodeSecret(p.Secret)
	require.NoError(err)
	assert.Equal(p.Headers["webhook-signature"], webhookSignature(secret, p.EventID, p.Headers["webhook-timestamp"], []byte(p.Body)))
	stamp, err := time.Parse(time.RFC3339, p.Envelope.Timestamp)
	require.NoError(err)
	s := Service{key: key}
	body, err := json.Marshal(s.envelope(store.MCPSubscription{ID: p.ID}, store.MCPEvent{Seq: p.Seq, Epoch: p.Epoch, Family: p.Name, OccurredAt: stamp, Data: p.Envelope.Data}))
	require.NoError(err)
	assert.Equal(p.Body, string(body))
}

func TestSecretEncryptionBindsSubscriptionAndRole(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	key := make([]byte, 32)
	first, err := encryptSecret(key, "sub_"+strings.Repeat("a", 64), "current", []byte("synthetic secret"))
	require.NoError(err)
	second, err := encryptSecret(key, "sub_"+strings.Repeat("a", 64), "current", []byte("synthetic secret"))
	require.NoError(err)
	assert.NotEqual(first, second)
	plain, err := decryptSecret(key, "sub_"+strings.Repeat("a", 64), "current", first)
	require.NoError(err)
	assert.Equal([]byte("synthetic secret"), plain)
	_, err = decryptSecret(key, "sub_"+strings.Repeat("b", 64), "current", first)
	require.Error(err)
	_, err = decryptSecret(key, "sub_"+strings.Repeat("a", 64), "previous", first)
	require.Error(err)
	key[0]++
	_, err = decryptSecret(key, "sub_"+strings.Repeat("a", 64), "current", first)
	require.Error(err)
}

func TestTokensRejectNonCanonicalAndCrossSubscriptionInputs(t *testing.T) {
	require := Require.New(t)
	key, id := make([]byte, 32), "sub_"+strings.Repeat("a", 64)
	valid := encodeCursor(key, id, 3, 42)
	for _, token := range []string{"", "c1.03.42.bad", "c1.+3.42.bad", "c1.3.-1.bad", "c1.3.9223372036854775808.bad", valid + "=", valid + ".extra", "c1.3.42.AAAAAAAAAAAAAAAAAAAAAA"} {
		_, _, err := decodeCursor(key, id, token)
		require.Error(err, token)
	}
	_, _, err := decodeCursor(key, "sub_"+strings.Repeat("b", 64), valid)
	require.Error(err)
	event := encodeEventID(key, id, 42)
	for _, token := range []string{"", event + "=", event + ".extra", strings.Replace(event, ".42.", ".042.", 1), strings.Replace(event, "sub_"+strings.Repeat("a", 64), "sub_"+strings.Repeat("b", 64), 1)} {
		_, _, err := decodeEventID(key, token)
		require.Error(err, token)
	}
	key[0] = 1
	_, _, err = decodeCursor(key, id, valid)
	require.Error(err)
	_, _, err = decodeEventID(key, event)
	require.Error(err)
	Assert.NotEqual(t, subscriptionID("ab", "c", nil, "d"), subscriptionID("a", "bc", nil, "d"), "length prefixes separate tuples")
}

func FuzzTokenRoundTrips(f *testing.F) {
	f.Add(uint64(3), uint64(42))
	f.Add(uint64(0), uint64(0))
	f.Add(uint64(1<<63-1), uint64(1<<63-1))
	f.Fuzz(func(t *testing.T, epoch, seq uint64) {
		e, n := int64(epoch&(1<<63-1)), int64(seq&(1<<63-1))
		key, id := make([]byte, 32), "sub_"+strings.Repeat("a", 64)
		gotE, gotN, err := decodeCursor(key, id, encodeCursor(key, id, e, n))
		Require.NoError(t, err)
		Assert.Equal(t, e, gotE)
		Assert.Equal(t, n, gotN)
		gotID, gotN, err := decodeEventID(key, encodeEventID(key, id, n))
		Require.NoError(t, err)
		Assert.Equal(t, id, gotID)
		Assert.Equal(t, n, gotN)
	})
}

func FuzzHostileTokensRejectNonCanonical(f *testing.F) {
	for _, s := range []string{"", "c1.03.42.bad", "c1.+3.42.bad", "c1.3.9223372036854775808.bad", "c1.3.42.AAAAAAAAAAAAAAAAAAAAAA=", "evt1.sub_ABC.42.bad", "c1.3.42.bad.extra", "\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, token string) {
		key, id := make([]byte, 32), "sub_"+strings.Repeat("a", 64)
		e, n, err := decodeCursor(key, id, token)
		if err == nil {
			Assert.Equal(t, token, encodeCursor(key, id, e, n))
		}
		gotID, n, err := decodeEventID(key, token)
		if err == nil {
			Assert.Equal(t, token, encodeEventID(key, gotID, n))
		}
	})
}
