package telegram

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportAndAPIKeysAgreeAcrossPeerKinds(t *testing.T) {
	tests := []struct {
		kind string
		peer tg.PeerClass
		want int64
	}{
		{"personal_chat", &tg.PeerUser{UserID: 123}, 123},
		{"private_group", &tg.PeerChat{ChatID: 123}, -123},
		{"public_supergroup", &tg.PeerChannel{ChannelID: 123}, -1000000000123},
		{"public_channel", &tg.PeerChannel{ChannelID: 123}, -1000000000123},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			exportID, err := CanonicalExportChatID(tt.kind, 123)
			require.NoError(err)
			apiID, err := MarkedPeerID(tt.peer)
			require.NoError(err)
			assert.Equal(tt.want, exportID)
			assert.Equal(exportID, apiID)
		})
	}
}

func TestCanonicalExportIDPreservesMarkedGroups(t *testing.T) {
	got, err := CanonicalExportChatID("public_supergroup", -1009999999999)
	require.NoError(t, err)
	assert.Equal(t, int64(-1009999999999), got)
}

func TestPeerIDsRejectInvalidAndAmbiguousValues(t *testing.T) {
	for _, tc := range []struct {
		kind string
		id   int64
	}{
		{"private_group", 0}, {"private_group", 1000000000000},
		{"public_channel", -123}, {"public_channel", 1000000000000},
		{"personal_chat", -123}, {"unknown", 123},
	} {
		_, err := CanonicalExportChatID(tc.kind, tc.id)
		assert.Error(t, err)
	}
	var missing *tg.PeerChannel
	_, err := MarkedPeerID(missing)
	assert.Error(t, err)
}

func TestAPIPeerRejectsMarkedIDs(t *testing.T) {
	for _, peer := range []tg.PeerClass{&tg.PeerChat{ChatID: -123}, &tg.PeerChannel{ChannelID: -1000000000123}} {
		_, err := MarkedPeerID(peer)
		assert.Error(t, err)
	}
}

func TestPersonalExportMonoforumRangeRequiresPeerEvidence(t *testing.T) {
	const ambiguous = int64(1002147483649)
	_, err := CanonicalExportChatID("personal_chat", ambiguous)
	require.ErrorContains(t, err, "ambiguous")
	apiID, err := MarkedPeerID(&tg.PeerUser{UserID: ambiguous})
	require.NoError(t, err)
	assert.Equal(t, ambiguous, apiID)
}

func TestSpecialUserDialogsKeepUserIDs(t *testing.T) {
	for _, kind := range []string{"replies", "verification_codes"} {
		id, err := CanonicalExportChatID(kind, 123)
		require.NoError(t, err)
		assert.EqualValues(t, 123, id)
	}
}

func TestNormalizedExportRejectsNegativeUserPeer(t *testing.T) {
	for _, kind := range []string{"personal_chat", "bot_chat", "saved_messages", "replies", "verification_codes"} {
		_, err := normalizedExportID(&Chat{ID: -123, Type: kind})
		require.Error(t, err, kind)
	}
}

func TestExportReplyPeerIDsUseTypedNamespaces(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int64
	}{{"user123", 123}, {"chat123", -123}, {"channel123", -1000000000123}} {
		got, err := ExportReplyPeerID(tc.raw)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
	for _, raw := range []string{"user-123", "chat0123", "channel0", "unknown123"} {
		_, err := ExportReplyPeerID(raw)
		require.Error(t, err, raw)
	}
}
