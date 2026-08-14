package beeper

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParticipantDecodeKeepsIsAdminAndCapturesRawUser pins the promotion
// hazard: Participant embeds User, so a User.UnmarshalJSON is promoted to
// Participant and would decode only the User fields, silently dropping
// isAdmin. Participant must declare its own decoder.
func TestParticipantDecodeKeepsIsAdminAndCapturesRawUser(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	payload := "{\n  \"providerID\": \"alice-uuid\", \"isAdmin\": true,\n" +
		"  \"id\": \"@signal_alice-uuid:beeper.local\", \"fullName\": \"Alice Example\",\n" +
		"  \"username\": \"@Alice\", \"phoneNumber\": \"+12025550123\",\n" +
		"  \"email\": \"Alice@Example.com\", \"isSelf\": false\n}"

	var participant Participant
	require.NoError(json.Unmarshal([]byte(payload), &participant), "decode participant")
	assert.True(participant.IsAdmin, "isAdmin must survive the embedded User decoder")
	assert.Equal("@signal_alice-uuid:beeper.local", participant.ID)
	assert.Equal("+12025550123", participant.PhoneNumber)
	assert.Equal(payload, string(participant.Raw),
		"Raw must preserve the exact source bytes, including key order and whitespace")
}

func TestUserDecodeCapturesRawAndAccountUserStillDecodes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var account Account
	require.NoError(json.Unmarshal([]byte(
		`{"accountID":"signal","network":"Signal",`+
			`"user":{"id":"@me:beeper.local","isSelf":true,"providerID":"me-uuid"}}`,
	), &account), "decode account")
	assert.Equal("signal", account.AccountID)
	assert.Equal("@me:beeper.local", account.User.ID)
	assert.True(account.User.IsSelf)
	assert.Contains(string(account.User.Raw), `"providerID":"me-uuid"`)
}

func TestProviderUserIDNamespacesEveryForm(t *testing.T) {
	tests := []struct {
		name        string
		serviceSlug string
		accountID   string
		scopeKind   *string
		scopeValue  *string
		user        *User
		want        string
	}{
		{
			name:        "provider native id wins and is service namespaced",
			serviceSlug: "signal",
			accountID:   "signal",
			user: &User{
				ID:  "@signal_alice-uuid:beeper.local",
				Raw: json.RawMessage(`{"id":"@signal_alice-uuid:beeper.local","providerID":"alice-uuid"}`),
			},
			want: "provider:6:signal:7:account:6:signal:10:alice-uuid",
		},
		{
			name:        "beeper user id is the fallback anchor",
			serviceSlug: "signal",
			accountID:   "signal-account",
			user:        &User{ID: "@signal_alice-uuid:beeper.local"},
			want:        "beeper:14:signal-account:31:@signal_alice-uuid:beeper.local",
		},
		{
			name:        "unclassified service falls back rather than emitting a bare id",
			serviceSlug: "",
			accountID:   "mystery-account",
			user: &User{
				ID:  "@alice:beeper.local",
				Raw: json.RawMessage(`{"id":"@alice:beeper.local","providerID":"alice-uuid"}`),
			},
			want: "beeper:15:mystery-account:19:@alice:beeper.local",
		},
		{
			name:        "blank native id is ignored",
			serviceSlug: "signal",
			accountID:   "signal-account",
			user: &User{
				ID:  "@alice:beeper.local",
				Raw: json.RawMessage(`{"providerID":"   "}`),
			},
			want: "beeper:14:signal-account:19:@alice:beeper.local",
		},
		{
			name:        "non-string native id is ignored",
			serviceSlug: "signal",
			accountID:   "signal-account",
			user: &User{
				ID:  "@alice:beeper.local",
				Raw: json.RawMessage(`{"providerID":42}`),
			},
			want: "beeper:14:signal-account:19:@alice:beeper.local",
		},
		{name: "nil user has no anchor", serviceSlug: "signal", user: nil, want: ""},
		{name: "empty id has no anchor", serviceSlug: "signal", user: &User{}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			assert.Equal(test.want, providerUserIDScoped(
				test.serviceSlug, test.accountID, test.scopeKind, test.scopeValue, test.user))
		})
	}
}

func TestProviderUserIDUsesOnlyDocumentedProviderID(t *testing.T) {
	assert := assert.New(t)
	user := &User{
		ID:  "@slack_alice:beeper.local",
		Raw: json.RawMessage(`{"providerUserID":"wrong-key","networkUserID":"also-wrong"}`),
	}
	assert.Equal("beeper:13:slack-account:25:@slack_alice:beeper.local",
		providerUserIDScoped("slack", "slack-account", new("account"), new("slack-account"), user),
		"undocumented raw fields must never become automatic identity keys")
}

func TestProviderUserIDNamespacesNativeIDByObservedScope(t *testing.T) {
	assert := assert.New(t)
	user := &User{
		ID:  "@slack_alice:beeper.local",
		Raw: json.RawMessage(`{"providerID":"same-native-id"}`),
	}
	assert.Equal("provider:5:slack:7:account:11:workspace-a:14:same-native-id",
		providerUserIDScoped("slack", "account-a", new("account"), new("workspace-a"), user))
	assert.Equal("provider:5:slack:7:account:11:workspace-b:14:same-native-id",
		providerUserIDScoped("slack", "account-b", new("account"), new("workspace-b"), user))
}

func TestBridgePrefixAndMatrixServerFromUserID(t *testing.T) {
	tests := []struct {
		userID     string
		wantBridge string
		wantServer string
	}{
		{userID: "@signal_alice-uuid:beeper.local", wantBridge: "signal", wantServer: "beeper.local"},
		{userID: "@googlechat_alice:beeper.local", wantBridge: "googlechat", wantServer: "beeper.local"},
		{userID: "@example-bridge_alice:beeper.local", wantBridge: "example-bridge", wantServer: "beeper.local"},
		{userID: "@alice:example.org", wantBridge: "", wantServer: "example.org"},
		{userID: "@15550100001:local-whatsapp.localhost", wantBridge: "", wantServer: "local-whatsapp.localhost"},
		{userID: "@_alice:beeper.local", wantBridge: "", wantServer: "beeper.local"},
		{userID: "@Signal_alice:beeper.local", wantBridge: "", wantServer: "beeper.local"},
		{userID: "alice", wantBridge: "", wantServer: ""},
		{userID: "", wantBridge: "", wantServer: ""},
	}
	for _, test := range tests {
		t.Run(test.userID, func(t *testing.T) {
			assert := assert.New(t)
			assert.Equal(test.wantBridge, bridgePrefixFromUserID(test.userID), "bridge prefix")
			assert.Equal(test.wantServer, matrixServerFromUserID(test.userID), "matrix server")
		})
	}
}
