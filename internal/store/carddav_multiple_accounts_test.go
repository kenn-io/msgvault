package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCardDAVMultipleAccountDiscoveryAndFences(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			st := cardDAVMultiBackendStore(t, backend)
			input := cardDAVConcurrentInput("personal@example.com", "shared")
			input.Books[0].CanCreate = new(true)
			personal, personalBooks, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			input.ConnectionName, input.Username = "work", "work@example.com"
			work, workBooks, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			assertions.Equal(int64(1), personal.ID)
			assertions.Greater(work.ID, int64(1))
			assertions.NotEqual(personalBooks[0].ID, workBooks[0].ID)
			assertions.Equal(personalBooks[0].CanonicalURL, workBooks[0].CanonicalURL)
			assertions.True(personalBooks[0].IsWriteTarget)
			assertions.False(workBooks[0].IsWriteTarget)
			assertions.True(workBooks[0].IsLookupSource)
			owner, err := st.GetCardDAVAccountForBookContext(t.Context(), workBooks[0].ID)
			require.NoError(err)
			assertions.Equal(work, owner)
			accounts, err := st.ListCardDAVAccountsContext(t.Context())
			require.NoError(err)
			assertions.Len(accounts, 2)
			plan := store.CardDAVSyncPlan{AddressBookID: workBooks[0].ID,
				ConnectionGeneration: work.ConnectionGeneration, SyncRevision: workBooks[0].SyncRevision,
				Upserts: []store.CardDAVRemoteResource{remoteResource(workBooks[0].CanonicalURL+"person.vcf", "work-person", "Work Example", "work-person@example.com", `"one"`)}}
			// Changing another connection's fence must not invalidate this plan.
			defaultInput := cardDAVConcurrentInput("personal@example.com", "shared")
			defaultInput.Books[0].CanCreate, defaultInput.CredentialsChanged = new(true), true
			_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), defaultInput)
			require.NoError(err)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), plan)
			require.NoError(err)
			books, err := st.ListCardDAVAddressBooksContext(t.Context(), work.ID)
			require.NoError(err)
			require.Len(books, 2)
			plan.SyncRevision = books[0].SyncRevision
			input.CredentialsChanged = true
			next, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			assertions.Equal(work.ConnectionGeneration+1, next.ConnectionGeneration)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), plan)
			require.ErrorIs(err, store.ErrCardDAVStalePlan)
			defaultAccount, err := st.GetCardDAVAccountByIDContext(t.Context(), store.DefaultCardDAVAccountID)
			require.NoError(err)
			assertions.Equal("personal@example.com", defaultAccount.Username)
		})
	}
}

func TestCardDAVMultipleAccountOwnershipBlockers(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			st := cardDAVMultiBackendStore(t, backend)
			input := cardDAVConcurrentInput("personal@example.com", "shared")
			input.Books[0].CanCreate = new(true)
			account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			resource := remoteResource(books[0].CanonicalURL+"person.vcf", "personal-person", "Personal Example", "personal-person@example.com", `"one"`)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: books[0].ID,
				ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision,
				Upserts: []store.CardDAVRemoteResource{resource}})
			require.NoError(err)
			mapping, err := st.GetCardDAVResourceContext(t.Context(), books[0].ID, resource.Href)
			require.NoError(err)
			require.NotNil(mapping.PersonID)
			snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *mapping.PersonID)
			require.NoError(err)
			_, err = st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
				PersonID: *mapping.PersonID, Desired: true, AddressBookID: books[0].ID, Href: resource.Href,
				OutgoingBody: resource.RemoteBody, OutgoingSemanticHash: "changed", LocalHash: snapshot.Fingerprint})
			require.NoError(err)
			input.ConnectionName, input.Username = "work", "work@example.com"
			work, workBooks, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			input.CredentialsChanged = true
			_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err, "pending default writes do not block changing work credentials")
			err = st.SetCardDAVBookRolesContext(t.Context(), workBooks[0].ID, store.CardDAVBookRoles{
				IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true})
			require.ErrorIs(err, store.ErrCardDAVRoleChangePending, "global target switch must protect the prior account")
			ids, err := st.ListCardDAVPublicationPersonIDsContext(t.Context(), work.ID)
			require.NoError(err)
			assertions.Empty(ids)
		})
	}
}

func TestCardDAVMultipleAccountRunsAndRetry(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			st := cardDAVMultiBackendStore(t, backend)
			input := cardDAVConcurrentInput("personal@example.com", "shared")
			_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			input.ConnectionName, input.Username = "work", "work@example.com"
			work, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			personalRun, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: store.DefaultCardDAVAccountID, Trigger: store.CardDAVSyncTriggerManual})
			require.NoError(err)
			workRun, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: work.ID, Trigger: store.CardDAVSyncTriggerScheduled})
			require.NoError(err)
			assertions.Equal(int64(1), personalRun.AccountID)
			assertions.Equal(work.ID, workRun.AccountID)
			_, err = st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: work.ID, Trigger: store.CardDAVSyncTriggerManual})
			require.ErrorIs(err, store.ErrCardDAVSyncActive)
			runs, err := st.ListCardDAVSyncRunsContext(t.Context(), 25, nil, work.ID)
			require.NoError(err)
			require.Len(runs, 1)
			assertions.Equal(workRun.ID, runs[0].ID)
			status, err := st.CardDAVSyncStatusContext(t.Context(), store.DefaultCardDAVAccountID)
			require.NoError(err)
			require.NotNil(status.Active)
			assertions.Equal(personalRun.ID, status.Active.ID)
			require.NoError(st.SetCardDAVRetryAfterContext(t.Context(), time.Now().Add(time.Hour), work.ID))
			assertions.NoError(st.CheckCardDAVRetryAfterContext(t.Context(), store.DefaultCardDAVAccountID))
			assertions.ErrorIs(st.CheckCardDAVRetryAfterContext(t.Context(), work.ID), store.ErrCardDAVRetryAfter)
		})
	}
}

func TestCardDAVMultipleAccountUnboundIntentRouting(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			st := cardDAVMultiBackendStore(t, backend)
			input := cardDAVConcurrentInput("personal@example.com", "shared")
			input.Books[0].CanCreate = new(true)
			_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			input.ConnectionName, input.Username = "work", "work@example.com"
			work, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			var desiredID, abandonedID int64
			for _, entry := range []struct {
				uid     string
				desired bool
				id      *int64
			}{
				{"example-desired", true, &desiredID}, {"example-abandoned", false, &abandonedID},
			} {
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id`), entry.uid).Scan(entry.id))
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_publications (person_id, desired) VALUES (?, ?)`), *entry.id, entry.desired)
				require.NoError(err)
			}
			personalIDs, err := st.ListCardDAVPublicationPersonIDsContext(t.Context(), store.DefaultCardDAVAccountID)
			require.NoError(err)
			assertions.ElementsMatch([]int64{desiredID, abandonedID}, personalIDs)
			workIDs, err := st.ListCardDAVPublicationPersonIDsContext(t.Context(), work.ID)
			require.NoError(err)
			assertions.Empty(workIDs, "an account without the global write target must not reconcile unbound publication intents")
		})
	}
}
