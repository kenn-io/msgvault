package imap

import (
	"crypto/sha256"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
)

// A header scan can precede a mailbox reset and a later MIME fetch. Native
// capture must not let the old header observation mask the selected new epoch.
func TestNativeMailboxMembershipUsesLatestObservedEpoch(t *testing.T) {
	raw := []byte("Message-ID: <synthetic-epoch@example.net>\r\n\r\nsynthetic body")
	client := &Client{config: &Config{Host: "imap.example.net", Username: "owner@example.net"}, selectedUIDValidity: 41}
	client.recordMembershipLocked("Work", 1, "", "synthetic-epoch@example.net", [32]byte{}, 0, nil)
	client.selectedUIDValidity = 42
	client.recordMembershipLocked("Work", 1, "", "synthetic-epoch@example.net", sha256.Sum256(raw), int64(len(raw)), nil)
	scopes, err := client.NativeMailboxMemberships("Work|1", raw)
	Require.NoError(t, err)
	Assert.Equal(t, []NativeMailboxScope{{Mailbox: "Work", UIDValidity: 42}}, scopes, "old header evidence cannot admit the new MIME under the retired epoch")
}

func TestNativeMailboxMembershipRejectsOldFetchAfterEpochChange(t *testing.T) {
	raw := []byte("Message-ID: <synthetic-epoch@example.net>\r\n\r\nsynthetic body")
	client := &Client{config: &Config{Host: "imap.example.net", Username: "owner@example.net"}, selectedUIDValidity: 41}
	client.recordMembershipLocked("Work", 1, "", "synthetic-epoch@example.net", sha256.Sum256(raw), int64(len(raw)), nil)
	client.selectedUIDValidity = 42
	client.recordMembershipLocked("Work", 2, "", "synthetic-epoch@example.net", [32]byte{}, 0, nil)
	_, err := client.NativeMailboxMemberships("Work|1", raw)
	Assert.Error(t, err, "a newer header cannot authorize bytes fetched under a retired native epoch")
}

func TestNativeMailboxMembershipForgettingObservationDropsAuthority(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	raw := []byte("Message-ID: <synthetic-epoch@example.net>\r\n\r\nsynthetic body")
	client := &Client{config: &Config{Host: "imap.example.net", Username: "owner@example.net"}, selectedUIDValidity: 41}
	client.recordMembershipLocked("Work", 1, "", "synthetic-epoch@example.net", sha256.Sum256(raw), int64(len(raw)), nil)
	client.recordMembershipLocked("INBOX", 2, "", "synthetic-epoch@example.net", [32]byte{}, 0, nil)
	scopes, err := client.NativeMailboxMemberships("Work|1", raw)
	require.NoError(err)
	require.Len(scopes, 2)
	client.forgetMembershipLocked("INBOX", 2)
	client.recordMembershipLocked("Archive", 3, "", "different@example.net", [32]byte{}, 0, nil)
	scopes, err = client.NativeMailboxMemberships("Work|1", raw)
	require.NoError(err)
	assert.Equal([]NativeMailboxScope{{Mailbox: "Work", UIDValidity: 41}}, scopes)
	client.forgetMembershipLocked("Work", 1)
	_, err = client.NativeMailboxMemberships("Work|1", raw)
	assert.Error(err, "a forgotten native fetch cannot leave cached canonical authority")
}
