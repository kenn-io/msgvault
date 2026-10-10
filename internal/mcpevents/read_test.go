package mcpevents

import (
	"encoding/json"
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
)

func TestRetainedEventAndMessageReadsRecheckAuthorization(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	id := f.CreateMessage("synthetic-readable-message")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET subject='Synthetic current subject' WHERE id=?`), id)
	require.NoError(err)
	appendReceipt(t, s, f, 1)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_log SET message_id=?,message_reference_seq=1 WHERE seq=1`), id)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_message_refs(message_id,reference_seq) VALUES(?,1)`), id)
	require.NoError(err)
	eventID := encodeEventID(s.key, result.ID, 1)
	envelope, err := s.GetEvent(t.Context(), s.principal, eventID)
	require.NoError(err)
	assert.Equal(eventID, envelope.EventID)
	assert.JSONEq(`{"kind":"message","from_me":false}`, string(envelope.Data))
	detail, err := s.GetMessage(t.Context(), s.principal, eventID, id)
	require.NoError(err)
	assert.Equal("Synthetic current subject", detail.Subject)
	assert.Equal(f.Source.ID, detail.SourceID)
	_, err = s.GetMessage(t.Context(), s.principal, eventID, id+1)
	require.Error(err)
	_, err = s.GetEvent(t.Context(), Principal("different-synthetic-owner"), eventID)
	require.Error(err)
	_, err = s.GetMessage(t.Context(), Principal("different-synthetic-owner"), eventID, id)
	require.Error(err)
	require.NoError(s.Unsubscribe(t.Context(), s.principal, UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: req.Delivery}))
	_, err = s.GetEvent(t.Context(), s.principal, eventID)
	require.Error(err)
	_, err = s.GetMessage(t.Context(), s.principal, eventID, id)
	require.Error(err)
}

func TestCalendarSourceRecipeUsesActualRuntimeGate(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, _ := eventService(t)
	source, err := f.Store.GetOrCreateSource("gcal", "synthetic-calendar@example.net")
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE sources SET display_name=?,sync_config=? WHERE id=?`), "Synthetic Calendar", `{"account_email":"owner@example.net"}`, source.ID)
	require.NoError(err)
	_, err = f.Store.GetOrCreateSource("caldav", "https://calendar.example.net/synthetic")
	require.NoError(err)
	sources, err := s.CalendarSources(t.Context(), s.principal)
	require.NoError(err)
	assert.Equal([]CalendarSource{{SourceID: strconv.FormatInt(source.ID, 10), Summary: "Synthetic Calendar", Account: "owner@example.net"}}, sources)
	args, err := canonicalArguments(calendarFamily, map[string]any{"calendar_source_id": strconv.FormatInt(source.ID, 10)})
	require.NoError(err)
	_, gotID, err := s.st.ValidateMCPEventScope(t.Context(), calendarFamily, args.scopeKind, args.scopeID, nil)
	require.NoError(err)
	assert.Equal(source.ID, gotID)
	var canonical map[string]any
	require.NoError(json.Unmarshal(args.bytes, &canonical))
	assert.Equal(map[string]any{"calendar_source_id": strconv.FormatInt(source.ID, 10)}, canonical)
}
