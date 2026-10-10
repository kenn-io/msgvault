package mcpevents_test

import (
	"encoding/base64"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsRejectsClosedArgumentsBeforeCallback(t *testing.T) {
	cases := []struct {
		name      string
		arguments func(string) map[string]any
	}{
		{"missing_scope", func(string) map[string]any { return map[string]any{} }},
		{"numeric_scope", func(string) map[string]any { return map[string]any{"conversation_id": 1} }},
		{"leading_zero_scope", func(id string) map[string]any { return map[string]any{"conversation_id": "0" + id} }},
		{"signed_scope", func(id string) map[string]any { return map[string]any{"conversation_id": "+" + id} }},
		{"overflow_scope", func(string) map[string]any { return map[string]any{"conversation_id": "9223372036854775808"} }},
		{"account_scope", func(id string) map[string]any { return map[string]any{"conversation_id": id, "source_id": "1"} }},
		{"string_boolean", func(id string) map[string]any {
			return map[string]any{"conversation_id": id, "include_from_me": "true"}
		}},
		{"null_boolean", func(id string) map[string]any { return map[string]any{"conversation_id": id, "include_reactions": nil} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			ownerKey := "synthetic-owner-key"
			svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Retention: 7 * 24 * time.Hour, Sources: []string{"gmail"}, KeyPath: filepath.Join(t.TempDir(), "mcp-events.key"), OwnerKey: ownerKey})
			require.NoError(err)
			_, err = svc.Subscribe(t.Context(), mcpevents.Principal(ownerKey), mcpevents.SubscribeRequest{Name: "msgvault.message_archived", Arguments: tc.arguments(strconv.FormatInt(f.ConvID, 10)), Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://receiver.example.net/events", Secret: "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))}})
			require.Error(err)
			var rpcErr *mcpevents.Error
			require.ErrorAs(err, &rpcErr)
			assert.Equal(-32602, rpcErr.Code)
			rows, err := svc.Status(t.Context(), mcpevents.Principal(ownerKey))
			require.NoError(err)
			assert.Empty(rows)
		})
	}
}

func TestMCPEventsRejectsUnsupportedDeliveryAndTTL(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	ownerKey := "synthetic-owner-key"
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Retention: 7 * 24 * time.Hour, Sources: []string{"gmail"}, KeyPath: filepath.Join(t.TempDir(), "mcp-events.key"), OwnerKey: ownerKey})
	require.NoError(err)
	request := mcpevents.SubscribeRequest{Name: "msgvault.message_archived", Arguments: map[string]any{"conversation_id": strconv.FormatInt(f.ConvID, 10)}, Delivery: mcpevents.Delivery{Mode: "sse", URL: "https://receiver.example.net/events", Secret: "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))}}
	_, err = svc.Subscribe(t.Context(), mcpevents.Principal(ownerKey), request)
	require.Error(err)
	var rpcErr *mcpevents.Error
	require.ErrorAs(err, &rpcErr)
	assert.Equal(-32014, rpcErr.Code)
	request.Delivery.Mode = "webhook"
	for _, ttl := range []int64{-1, 0, 24*60*60*1000 + 1} {
		request.TTLMS = &ttl
		_, err = svc.Subscribe(t.Context(), mcpevents.Principal(ownerKey), request)
		require.Error(err)
		require.ErrorAs(err, &rpcErr)
		assert.Equal(-32602, rpcErr.Code)
	}
}

func TestMCPEventsRejectsUnsafeCallbackURLs(t *testing.T) {
	f := storetest.New(t)
	ownerKey := "synthetic-owner-key"
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Retention: 7 * 24 * time.Hour, Sources: []string{"gmail"}, KeyPath: filepath.Join(t.TempDir(), "mcp-events.key"), OwnerKey: ownerKey})
	Require.NoError(t, err)
	for _, callback := range []string{
		"http://receiver.example.net/events",
		"https://receiver.example.net:9443/events",
		"https://user:password@receiver.example.net/events",
		"https://receiver.example.net/events#fragment",
		"https://localhost/events",
		"https://127.0.0.1/events",
		"https://10.0.0.1/events",
		"https://169.254.169.254/events",
		"https://[::1]/events",
		"https://[::ffff:127.0.0.1]/events",
		"https://[fc00::1]/events",
	} {
		t.Run(callback, func(t *testing.T) {
			_, err := svc.Subscribe(t.Context(), mcpevents.Principal(ownerKey), mcpevents.SubscribeRequest{Name: "msgvault.message_archived", Arguments: map[string]any{"conversation_id": strconv.FormatInt(f.ConvID, 10)}, Delivery: mcpevents.Delivery{Mode: "webhook", URL: callback, Secret: "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))}})
			Require.Error(t, err)
			Assert.NotContains(t, err.Error(), callback)
		})
	}
	rows, err := svc.Status(t.Context(), mcpevents.Principal(ownerKey))
	Require.NoError(t, err)
	Assert.Empty(t, rows)
}

func TestMCPEventsRejectsMalformedWebhookSecrets(t *testing.T) {
	f := storetest.New(t)
	ownerKey := "synthetic-owner-key"
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Retention: 7 * 24 * time.Hour, Sources: []string{"gmail"}, KeyPath: filepath.Join(t.TempDir(), "mcp-events.key"), OwnerKey: ownerKey})
	Require.NoError(t, err)
	for _, secret := range []string{"", "plain-secret", "whsec_!!!", "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 23)), "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 65))} {
		_, err := svc.Subscribe(t.Context(), mcpevents.Principal(ownerKey), mcpevents.SubscribeRequest{Name: "msgvault.message_archived", Arguments: map[string]any{"conversation_id": strconv.FormatInt(f.ConvID, 10)}, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://receiver.example.net/events", Secret: secret}})
		Require.Error(t, err)
		if secret != "" {
			Assert.NotContains(t, err.Error(), secret)
		}
	}
}
