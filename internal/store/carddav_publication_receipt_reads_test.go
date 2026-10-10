package store_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

type publicationReceiptWire struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	PersonID      int64  `json:"person_id"`
	PersonUID     string `json:"person_uid"`
	AccountID     int64  `json:"account_id"`
	AddressBookID int64  `json:"address_book_id"`
	BodySHA256    string `json:"body_sha256"`
	RemoteETag    string `json:"remote_etag"`
}

func readPublicationReceiptWire(ctx context.Context, t *testing.T, st *store.Store, principal, key string, authorize store.PersonEditAuthorizer) (*publicationReceiptWire, error) {
	t.Helper()
	result, err := st.CardDAVPublicationReceiptContext(ctx, principal, key, authorize)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "BEGIN:VCARD")
	assert.NotContains(t, string(encoded), "synthetic-key")
	assert.NotContains(t, string(encoded), "synthetic-owner")
	var receipt publicationReceiptWire
	require.NoError(t, json.Unmarshal(encoded, &receipt))
	return &receipt, nil
}

func TestPublicationReceiptReadPreservesHistoryWithCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"allowed", "revoked", "nil_authorizer", "other_key", "other_principal", "recreated_person", "rebound_book", "provider_changed", "restart"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, account, book, mapping := seededCardDAVConflictMapping(t)
			require.NotNil(mapping.PersonID)
			source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), *mapping.PersonID)
			require.NoError(err)
			body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Receipt\r\nN:Example;Synthetic;;;\r\nEND:VCARD\r\n", source.Person.VCardUID))
			hash, err := carddav.SemanticHash(body)
			require.NoError(err)
			plan := store.CardDAVPublicationPlan{PersonID: *mapping.PersonID, Desired: true, AddressBookID: book.ID, Href: mapping.Href, OutgoingBody: body, OutgoingSemanticHash: hash, LocalHash: source.Snapshot.Fingerprint}
			fence := store.CardDAVCurrentReviewFence(source, body, mapping.Href)
			pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)})
			require.NoError(err)
			bound, err := st.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-key")
			require.NoError(err)
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			require.NoError(st.AdmitCardDAVPublicationReceiptContext(bound, *pending, allow))
			require.NoError(st.CommitCardDAVPublicationAuthorizedContext(bound, store.CardDAVCanonicalMutation{Publication: *pending, Remote: store.CardDAVRemoteResource{Href: pending.Href, RemoteUID: source.Person.VCardUID, RemoteETag: `"verified-receipt"`, RemoteBody: body, SemanticHash: hash}}, allow))
			original, err := readPublicationReceiptWire(t.Context(), t, st, "synthetic-owner", "synthetic-key", allow)
			require.NoError(err)
			require.NotEmpty(original.ID)
			assert.Equal("verified", original.State)
			assert.Equal(*mapping.PersonID, original.PersonID)
			assert.Equal(source.Person.VCardUID, original.PersonUID)
			assert.Equal(account.ID, original.AccountID)
			assert.Equal(book.ID, original.AddressBookID)
			assert.Equal(`"verified-receipt"`, original.RemoteETag)
			require.NotNil(pending.ApprovedBodySHA256)
			assert.Equal(*pending.ApprovedBodySHA256, original.BodySHA256)
			authorize := allow
			principal, key := "synthetic-owner", "synthetic-key"
			denied := errors.New("synthetic current receipt authority revoked")
			switch mode {
			case "revoked":
				authorize = func(context.Context, *store.IdentityGrantSelection) error { return denied }
			case "nil_authorizer":
				authorize = nil
			case "other_key":
				key = "another-key"
			case "other_principal":
				principal = "another-owner"
			case "recreated_person":
				_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE persons SET vcard_uid=? WHERE id=?"), "synthetic-replacement-person", *mapping.PersonID)
			case "rebound_book":
				_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE carddav_accounts SET username=? WHERE id=?"), "replacement@example.test", account.ID)
			case "provider_changed":
				_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE carddav_resources SET remote_etag=? WHERE id=?"), `"later-provider-version"`, mapping.ID)
			case "restart":
				if st.IsPostgreSQL() {
					t.Skip("SQLite reopen proof; PostgreSQL persistence is verified in its backend lane")
				}
				var sequence int
				var name, path string
				require.NoError(st.DB().QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&sequence, &name, &path))
				require.NotEmpty(path)
				require.NoError(st.Close())
				st, err = store.OpenForTest(path)
				require.NoError(err)
				t.Cleanup(func() { assert.NoError(st.Close()) })
			}
			require.NoError(err)
			saved, err := readPublicationReceiptWire(t.Context(), t, st, principal, key, authorize)
			if mode == "allowed" || mode == "provider_changed" || mode == "restart" {
				require.NoError(err)
				assert.Equal(original, saved, "authorized historical reads ignore later provider state and never replay publication")
			} else {
				require.Error(err)
				assert.Nil(saved)
				if mode == "revoked" {
					require.ErrorIs(err, denied)
				}
			}
		})
	}
}

type publicationReceiptOwnerReader interface {
	CardDAVPublicationReceiptByIDContext(ctx context.Context, id string, authorize func(context.Context) error) (*store.CardDAVPublicationReceipt, error)
}

func TestPublicationReceiptOwnerReadSurvivesResourceCleanup(t *testing.T) {
	for _, mode := range []string{"allowed", "resource_cleanup", "revoked", "nil_authorizer", "unknown_id"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, account, book, mapping := seededCardDAVConflictMapping(t)
			backend, ok := any(st).(publicationReceiptOwnerReader)
			require.True(ok, "current owner needs exact historical lookup after native resource cleanup")
			require.NotNil(mapping.PersonID)
			source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), *mapping.PersonID)
			require.NoError(err)
			body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Receipt\r\nN:Example;Synthetic;;;\r\nEND:VCARD\r\n", source.Person.VCardUID))
			hash, err := carddav.SemanticHash(body)
			require.NoError(err)
			plan := store.CardDAVPublicationPlan{PersonID: *mapping.PersonID, Desired: true, AddressBookID: book.ID, Href: mapping.Href, OutgoingBody: body, OutgoingSemanticHash: hash, LocalHash: source.Snapshot.Fingerprint}
			fence := store.CardDAVCurrentReviewFence(source, body, mapping.Href)
			pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)})
			require.NoError(err)
			bound, err := st.WithCardDAVPublicationReceipt(t.Context(), "synthetic-owner", "synthetic-key")
			require.NoError(err)
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			require.NoError(st.AdmitCardDAVPublicationReceiptContext(bound, *pending, allow))
			require.NoError(st.CommitCardDAVPublicationAuthorizedContext(bound, store.CardDAVCanonicalMutation{Publication: *pending, Remote: store.CardDAVRemoteResource{Href: pending.Href, RemoteUID: source.Person.VCardUID, RemoteETag: `"verified-receipt"`, RemoteBody: body, SemanticHash: hash}}, allow))
			original, err := st.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-key", allow)
			require.NoError(err)
			id := original.ID
			denied := errors.New("synthetic current owner authority revoked")
			authorize := func(context.Context) error { return nil }
			switch mode {
			case "resource_cleanup":
				_, err = st.DB().ExecContext(t.Context(), st.Rebind("DELETE FROM carddav_accounts WHERE id=?"), account.ID)
				require.NoError(err)
				unavailable, err := st.CardDAVPublicationReceiptContext(t.Context(), "synthetic-owner", "synthetic-key", allow)
				require.Error(err)
				assert.Nil(unavailable)
			case "revoked":
				authorize = func(context.Context) error { return denied }
			case "nil_authorizer":
				authorize = nil
			case "unknown_id":
				id = "synthetic-unknown-receipt"
			}
			saved, err := backend.CardDAVPublicationReceiptByIDContext(t.Context(), id, authorize)
			if mode == "allowed" || mode == "resource_cleanup" {
				require.NoError(err)
				assert.Equal(original, saved, "trusted current owner reads retained historical evidence without resource lookup or dispatch")
			} else {
				require.Error(err)
				assert.Nil(saved)
				if mode == "revoked" {
					require.ErrorIs(err, denied)
				}
			}
		})
	}
}
