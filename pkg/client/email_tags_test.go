package client_test

import (
	stdjson "encoding/json"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMessageTagsGeneratedClientSupportsFullUIDRange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	native := emailtags.MessageTagResult{Provider: "imap", UID: ^uint32(0), UIDValidity: ^uint32(0), Tags: []string{}, Before: []string{}, AvailableTags: []emailtags.MessageTag{}, Verified: true}
	encoded, err := json.Marshal(native)
	require.NoError(err)
	var wire generated.MessageTagResult
	require.NoError(json.Unmarshal(encoded, &wire), "the generated client must accept the complete IMAP unsigned 32-bit identity range")
	require.NotNil(wire.UID)
	require.NotNil(wire.Uidvalidity)
	assert.Equal(int64(4294967295), *wire.UID)
	assert.Equal(int64(4294967295), *wire.Uidvalidity)
}

func TestMessageTagErrorGeneratedClientPreservesReplayRequest(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	observedAt := time.Date(2026, time.October, 18, 0, 0, 0, 0, time.UTC)
	replayRequest := `{"operation":"tags","target":{"source_id":2,"source_type":"gmail","source_identifier":"owner@example.test","account_id":"owner@example.test","scope":"message","item_id":7,"provider_id":"native-7"},"tags":{"add":["Label_next"]},"expected":{"target":{"source_id":2,"source_type":"gmail","source_identifier":"owner@example.test","account_id":"owner@example.test","scope":"message","item_id":7,"provider_id":"native-7"},"tags":["INBOX"],"observed_at":"2026-10-18T00:00:00Z"},"preview_token":"signed-preview","idempotency_key":"tags-key"}`
	body := `{"error":"remote_unknown","message":"inspect receipt before retrying","result":{"message_id":7,"source_id":2,"provider":"gmail","tags":["INBOX"],"before":["INBOX"],"available_tags":[],"dry_run":false,"verified":false,"replay_request":` + replayRequest + `}}`

	var failure generated.MessageTagError
	requirements.NoError(stdjson.Unmarshal([]byte(body), &failure))
	requirements.NotNil(failure.Result)
	requirements.NotNil(failure.Result.ReplayRequest)
	encoded, err := stdjson.Marshal(failure.Result.ReplayRequest)
	requirements.NoError(err)
	var replay inboxcontrol.Request
	requirements.NoError(stdjson.Unmarshal(encoded, &replay))
	assertions.Equal(inboxcontrol.OpTags, replay.Operation)
	assertions.Equal("tags-key", replay.IdempotencyKey)
	assertions.Equal("signed-preview", replay.PreviewToken)
	requirements.NotNil(replay.Target)
	assertions.Equal(int64(2), replay.Target.SourceID)
	assertions.Equal(int64(7), replay.Target.ItemID)
	requirements.NotNil(replay.Tags)
	assertions.Equal([]string{"Label_next"}, replay.Tags.Add)
	requirements.NotNil(replay.Expected)
	assertions.Equal(observedAt, replay.Expected.ObservedAt)
	assertions.Equal([]string{"INBOX"}, replay.Expected.Tags)
}
