package mcpevents

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// The callback transports are replaced only to observe whether real admission
// reaches networking. Both DNS and TCP remain forbidden for a stale reference.
func TestImportExactSourceRejectsStaleReferenceBeforeCallback(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := storetest.New(t)
	var lookups, dials atomic.Int64
	svc, err := New(t.Context(), f.Store, Options{
		Enabled: true, ImportNotifications: true, OwnerKey: "synthetic-import-reference-callback-owner",
		KeyPath: filepath.Join(t.TempDir(), "events.key"),
		LookupIP: func(context.Context, string) ([]netip.Addr, error) {
			lookups.Add(1)
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("synthetic callback destination unavailable")
		},
	})
	require.NoError(err)
	original, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sources SET identifier=? WHERE id=?`), "synthetic-replacement@example.net", f.Source.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sources SET identifier=? WHERE id=?`), f.Source.Identifier, f.Source.ID)
	require.NoError(err)
	req := SubscribeRequest{Name: importFamily, Arguments: map[string]any{"source_id": strconv.FormatInt(f.Source.ID, 10), "source_ref": original.Reference}, Delivery: Delivery{Mode: "webhook", URL: "https://receiver.example.net/imports", Secret: "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))}}
	_, err = svc.Subscribe(t.Context(), svc.principal, req)
	require.Error(err)
	assert.Zero(lookups.Load(), "stale source references fail before DNS")
	assert.Zero(dials.Load(), "stale source references fail before callback dialing")
	lookups.Store(0)
	dials.Store(0)
	current, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	assert.NotEqual(original.Reference, current.Reference)
	req.Arguments["source_ref"] = current.Reference
	_, err = svc.Subscribe(t.Context(), svc.principal, req)
	require.Error(err, "positive admission reaches the deliberately unavailable synthetic destination")
	assert.Positive(lookups.Load(), "a current source reference reaches callback verification")
	assert.Positive(dials.Load(), "positive control exercises the dial boundary")
}
