package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

func TestMergeSourcesRebuildsAccountAttribution(t *testing.T) {
	t.Run("incoming alias changes destination mail", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		live := newAttrFixture(t, "mbox", "live")
		history := newAttrFixtureOn(t, live.st, "mbox", "history")
		history.confirm("alias@example.test")
		id := live.persist(attrMail{raw: "To: alias@example.test\r\n\r\nbody", to: []string{"alias@example.test"}})
		address, path := attribution(t, live.st, id)
		require.False(address.Valid)
		require.True(path.Valid, "mail was derived before the alias was known")

		req := store.MergeSourcesRequest{FromSourceID: history.source.ID, IntoSourceID: live.source.ID, DryRun: true}
		_, err := live.st.MergeSourcesContext(t.Context(), req)
		require.NoError(err)
		afterAddress, afterPath := attribution(t, live.st, id)
		assert.Equal(address, afterAddress)
		assert.Equal(path, afterPath)

		req.DryRun = false
		_, err = live.st.MergeSourcesContext(t.Context(), req)
		require.NoError(err)
		assert.Equal([]int64{id}, searchIDs(t, live.st, "received:alias@example.test"))

		// A repair interrupted after the merge committed leaves pending rows.
		// Retrying the merge must finish those rows without repeating the merge.
		clearAccountPath(t, live.st, id)
		req.DryRun = true
		_, err = live.st.MergeSourcesContext(t.Context(), req)
		require.NoError(err)
		_, path = attribution(t, live.st, id)
		assert.False(path.Valid, "a dry run must not repair pending rows")
		req.DryRun = false
		result, err := live.st.MergeSourcesContext(t.Context(), req)
		require.NoError(err)
		assert.True(result.AlreadyMerged)
		assert.Equal([]int64{id}, searchIDs(t, live.st, "received:alias@example.test"))
	})

	t.Run("moved mail uses the destination mailbox", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		live := newAttrFixture(t, "gmail", "live@example.test")
		history := newAttrFixtureOn(t, live.st, "gmail", "history@example.test")
		live.confirm("live@example.test")
		history.confirm("history@example.test")
		id := history.persist(attrMail{raw: "From: friend@example.test\r\nTo: list@example.test\r\n\r\nbody",
			from: []string{"friend@example.test"}, to: []string{"list@example.test"}})
		require.Equal([]int64{id}, searchIDs(t, live.st, "received:history@example.test"))

		_, err := live.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
			FromSourceID: history.source.ID, IntoSourceID: live.source.ID,
		})
		require.NoError(err)
		assert.Equal([]int64{id}, searchIDs(t, live.st, "received:live@example.test"))
		assert.Empty(searchIDs(t, live.st, "received:history@example.test"))
	})

	t.Run("incoming Sent folder changes destination mail", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		live := newAttrFixture(t, "imap", "imaps://live%40example.test@mail.example.test:993")
		history := newAttrFixtureOn(t, live.st, "imap", "imaps://history%40example.test@mail.example.test:993")
		live.confirm("live@example.test")
		id := live.persist(attrMail{raw: "From: live@example.test\r\nTo: list@example.test\r\n\r\nbody",
			from: []string{"live@example.test"}, to: []string{"list@example.test"}})
		_, path := attribution(t, live.st, id)
		require.Equal("sent", path.String)
		_, err := live.st.EnsureLabelsBatch(history.source.ID, map[string]store.LabelInfo{
			"Sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		})
		require.NoError(err)

		_, err = live.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
			FromSourceID: history.source.ID, IntoSourceID: live.source.ID,
		})
		require.NoError(err)
		assert.Equal([]int64{id}, searchIDs(t, live.st, "received:live@example.test"))
	})

	t.Run("moved calendar events use the destination account", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		live := newAttrFixture(t, "gcal", "live-calendar")
		history := newAttrFixtureOn(t, live.st, "gcal", "history-calendar")
		require.NoError(live.st.UpdateSourceSyncConfig(live.source.ID,
			`{"account_email":"live@example.test","calendar_id":"primary"}`))
		require.NoError(live.st.UpdateSourceSyncConfig(history.source.ID,
			`{"account_email":"history@example.test","calendar_id":"primary"}`))
		live.confirm("live@example.test")
		history.confirm("history@example.test")
		id, err := live.st.UpsertMessage(&store.Message{
			SourceID: history.source.ID, ConversationID: history.conv,
			SourceMessageID: "historical-event", MessageType: "calendar_event",
		})
		require.NoError(err)
		require.Equal([]int64{id}, searchIDs(t, live.st, "account:history@example.test"))

		_, err = live.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
			FromSourceID: history.source.ID, IntoSourceID: live.source.ID,
		})
		require.NoError(err)
		assert.Equal([]int64{id}, searchIDs(t, live.st, "account:live@example.test"))
		assert.Empty(searchIDs(t, live.st, "account:history@example.test"))
	})
}

func TestMergeSourcesPreservesUnsentDraft(t *testing.T) {
	for _, sentEvidence := range []string{"none", "label", "origin"} {
		t.Run(sentEvidence, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			live := newAttrFixture(t, "imap", "imaps://live%40example.test@mail.example.test:993")
			history := newAttrFixtureOn(t, live.st, "imap", "imaps://history%40example.test@mail.example.test:993")
			id := history.persist(attrMail{from: []string{"history@example.test"}, to: []string{"recipient@example.test"}})
			_, err := activityExec(live.st, `UPDATE messages SET sent_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
			require.NoError(err)
			_, err = activityExec(live.st, `INSERT INTO imap_message_memberships
				(source_id, mailbox, uidvalidity, uid, message_id, flags)
				VALUES (?, 'Scratch', 7, 1, ?, ?)`, history.source.ID, id, `["\\Draft"]`)
			require.NoError(err)
			if sentEvidence == "label" {
				labels, err := live.st.EnsureLabelsBatch(history.source.ID, map[string]store.LabelInfo{
					"sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
				})
				require.NoError(err)
				require.NoError(live.st.AddMessageLabels(id, []int64{labels["sent"]}))
			}
			if sentEvidence == "origin" {
				require.NoError(live.st.SetMessageMetadata(id, sql.NullString{String: `{"imap_content_origin":"sent"}`, Valid: true}))
			}
			before, err := live.st.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
			require.NoError(err)
			require.Len(before, 1)
			require.Equal(sentEvidence == "none", before[0].Unsent)
			var originalIsSent sql.NullBool
			require.NoError(activityQueryRow(live.st, `SELECT is_sent FROM messages WHERE id = ?`, id).Scan(&originalIsSent))

			_, err = live.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
				FromSourceID: history.source.ID, IntoSourceID: live.source.ID,
			})
			require.NoError(err)
			after, err := live.st.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
			require.NoError(err)
			require.Len(after, 1)
			assert.Equal(live.source.ID, after[0].SourceID)
			assert.Equal(sentEvidence == "none", after[0].Unsent)
			assert.Equal(sentEvidence != "none", after[0].Eligible)
			var isSent sql.NullBool
			require.NoError(activityQueryRow(live.st, `SELECT is_sent FROM messages WHERE id = ?`, id).Scan(&isSent))
			assert.Equal(originalIsSent, isSent)

			labels, err := live.st.EnsureLabelsBatch(live.source.ID, map[string]store.LabelInfo{
				"sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
			})
			require.NoError(err)
			require.NoError(live.st.AddMessageLabels(id, []int64{labels["sent"]}))
			after, err = live.st.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
			require.NoError(err)
			require.Len(after, 1)
			assert.False(after[0].Unsent)
			assert.True(after[0].Eligible, "Sent evidence makes the former draft contact activity")
			require.NoError(activityQueryRow(live.st, `SELECT is_sent FROM messages WHERE id = ?`, id).Scan(&isSent))
			assert.Equal(originalIsSent, isSent)
		})
	}
}

func TestMergeSourcesPreservesLabelSystemRoles(t *testing.T) {
	for _, test := range []struct {
		name, historyLabel, destinationRole string
		occupiedArchiveNames                bool
	}{
		{name: "distinct names", historyLabel: "Historical Sent"},
		{name: "same name and role", historyLabel: "Sent", destinationRole: store.LabelSystemRoleSent},
		{name: "same name with different roles", historyLabel: "Sent"},
		{name: "generated archive name collisions", historyLabel: "Sent", occupiedArchiveNames: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			live := newAttrFixture(t, "imap", "imaps://live%40example.test@mail.example.test:993")
			history := newAttrFixtureOn(t, live.st, "imap", "imaps://history%40example.test@mail.example.test:993")
			live.confirm("live@example.test")
			history.confirm("history@example.test")
			liveLabels, err := live.st.EnsureLabelsBatch(live.source.ID, map[string]store.LabelInfo{
				"live-folder": {Name: "Sent", Type: "system", SystemRole: test.destinationRole},
				// Without another trusted Sent folder, sender fallback masks a lost role.
				"live-sent": {Name: "Sent Items", Type: "system", SystemRole: store.LabelSystemRoleSent},
			})
			require.NoError(err)
			historyLabels, err := live.st.EnsureLabelsBatch(history.source.ID, map[string]store.LabelInfo{
				"history-sent": {Name: test.historyLabel, Type: "system", SystemRole: store.LabelSystemRoleSent},
			})
			require.NoError(err)
			historyLabelID := historyLabels["history-sent"]
			archiveName := fmt.Sprintf("msgvault-archive:%d:label:%d", history.source.ID, historyLabelID)
			if test.occupiedArchiveNames {
				_, err = live.st.EnsureLabelsBatch(live.source.ID, map[string]store.LabelInfo{
					"occupied-archive": {Name: archiveName, Type: "user"},
					"occupied-suffix":  {Name: archiveName + ":archive", Type: "user"},
				})
				require.NoError(err)
			}
			liveID := live.persist(attrMail{
				raw:  "From: friend@example.test\r\nTo: list@example.test\r\n\r\nlive body",
				from: []string{"friend@example.test"}, to: []string{"list@example.test"},
				labels: []int64{liveLabels["live-folder"]},
			})
			historyID := history.persist(attrMail{
				raw:  "From: history@example.test\r\nTo: friend@example.test\r\n\r\nhistorical body",
				from: []string{"history@example.test"}, to: []string{"friend@example.test"},
				labels: []int64{historyLabelID},
			})
			addressBefore, pathBefore := attribution(t, live.st, historyID)
			require.Equal("sent", pathBefore.String)
			require.Equal("history@example.test", addressBefore.String)
			liveAddressBefore, livePathBefore := attribution(t, live.st, liveID)

			_, err = live.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
				FromSourceID: history.source.ID, IntoSourceID: live.source.ID,
			})
			require.NoError(err)
			addressAfter, pathAfter := attribution(t, live.st, historyID)
			assert.Equal("sent", pathAfter.String)
			assert.Equal("history@example.test", addressAfter.String)
			assert.NotContains(searchIDs(t, live.st, "received:live@example.test"), historyID)
			assert.Contains(searchIDs(t, live.st, "account:history@example.test"), historyID)
			liveAddressAfter, livePathAfter := attribution(t, live.st, liveID)
			assert.Equal(liveAddressBefore, liveAddressAfter)
			assert.Equal(livePathBefore, livePathAfter)
			if test.destinationRole == "" {
				assert.Contains(searchIDs(t, live.st, "received:live@example.test"), liveID)
			}

			wantLabelID := historyLabelID
			wantName := test.historyLabel
			var wantProviderID sql.NullString
			if test.historyLabel == "Sent" {
				if test.destinationRole == store.LabelSystemRoleSent {
					wantLabelID = liveLabels["live-folder"]
					wantProviderID = sql.NullString{String: "live-folder", Valid: true}
				} else {
					wantName = archiveName
					if test.occupiedArchiveNames {
						wantName += ":archive:archive"
					}
				}
			}
			labelIDs, err := live.st.MessageLabelIDsContext(t.Context(), historyID)
			require.NoError(err)
			assert.Equal([]int64{wantLabelID}, labelIDs)
			var labelSourceID int64
			var labelName, role string
			var providerID sql.NullString
			require.NoError(live.st.DB().QueryRow(live.st.Rebind(
				`SELECT source_id, name, COALESCE(system_role, ''), source_label_id FROM labels WHERE id = ?`), wantLabelID).
				Scan(&labelSourceID, &labelName, &role, &providerID))
			assert.Equal(live.source.ID, labelSourceID)
			assert.Equal(wantName, labelName)
			assert.Equal(store.LabelSystemRoleSent, role)
			assert.Equal(wantProviderID, providerID)
		})
	}
}

func TestMergeSourcesPreservesGmailLabelRoles(t *testing.T) {
	for _, test := range []struct {
		name, sourceType, providerID, incomingName string
		incomingRole, destinationRole, wantRole    string
		wantCoalesced, wantInbound, wantDraft      bool
	}{
		{name: "legacy incoming sent", providerID: "SENT", incomingName: "SENT", destinationRole: store.LabelSystemRoleSent, wantRole: store.LabelSystemRoleSent, wantCoalesced: true},
		{name: "both sent roles absent", providerID: "SENT", incomingName: "SENT", wantRole: store.LabelSystemRoleSent, wantCoalesced: true},
		{name: "both sent roles explicit", providerID: "SENT", incomingName: "SENT", incomingRole: store.LabelSystemRoleSent, destinationRole: store.LabelSystemRoleSent, wantRole: store.LabelSystemRoleSent, wantCoalesced: true},
		{name: "legacy destination sent", providerID: "SENT", incomingName: "SENT", incomingRole: store.LabelSystemRoleSent, wantRole: store.LabelSystemRoleSent, wantCoalesced: true},
		{name: "distinct legacy sent name", providerID: "SENT", incomingName: "Historical Sent", destinationRole: store.LabelSystemRoleSent, wantRole: store.LabelSystemRoleSent},
		{name: "ordinary Gmail label named SENT", providerID: "Label_ordinary", incomingName: "SENT", destinationRole: store.LabelSystemRoleSent, wantInbound: true},
		{name: "ordinary IMAP label with SENT id", sourceType: "imap", providerID: "SENT", incomingName: "SENT", destinationRole: store.LabelSystemRoleSent, wantInbound: true},
		{name: "distinct legacy draft name", providerID: "DRAFT", incomingName: "Historical Drafts", destinationRole: store.LabelSystemRoleDrafts, wantRole: store.LabelSystemRoleDrafts, wantDraft: true},
		{name: "canonical draft overrides sent role", providerID: "DRAFT", incomingName: "DRAFT", incomingRole: store.LabelSystemRoleSent, destinationRole: store.LabelSystemRoleDrafts, wantRole: store.LabelSystemRoleDrafts, wantCoalesced: true, wantDraft: true},
		{name: "draft role overrides canonical sent", providerID: "SENT", incomingName: "SENT", incomingRole: store.LabelSystemRoleDrafts, destinationRole: store.LabelSystemRoleSent, wantRole: store.LabelSystemRoleDrafts, wantDraft: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			sourceType := test.sourceType
			liveIdentifier, historyIdentifier := "live@example.test", "history@example.test"
			if sourceType == "" {
				sourceType = "gmail"
			} else {
				liveIdentifier = "imaps://live%40example.test@mail.example.test:993"
				historyIdentifier = "imaps://history%40example.test@mail.example.test:993"
			}
			live := newAttrFixture(t, sourceType, liveIdentifier)
			history := newAttrFixtureOn(t, live.st, sourceType, historyIdentifier)
			live.confirm("live@example.test")
			history.confirm("history@example.test")
			destinationID := "SENT"
			if test.providerID == "DRAFT" {
				destinationID = "DRAFT"
			}
			liveLabels, err := live.st.EnsureLabelsBatch(live.source.ID, map[string]store.LabelInfo{
				destinationID: {Name: destinationID, Type: "system", SystemRole: test.destinationRole},
			})
			require.NoError(err)
			labelID, err := live.st.EnsureLabel(history.source.ID, test.providerID, test.incomingName, "system")
			require.NoError(err)
			if test.incomingRole != "" {
				_, err = live.st.EnsureLabelsBatch(history.source.ID, map[string]store.LabelInfo{
					test.providerID: {Name: test.incomingName, Type: "system", SystemRole: test.incomingRole},
				})
				require.NoError(err)
			}
			if sourceType == "imap" {
				// Keep sender fallback from masking an ordinary label's lack of role.
				_, err = live.st.EnsureLabelsBatch(history.source.ID, map[string]store.LabelInfo{
					"other-sent": {Name: "Sent Items", Type: "system", SystemRole: store.LabelSystemRoleSent},
				})
				require.NoError(err)
			}
			var roleBefore sql.NullString
			require.NoError(live.st.DB().QueryRow(live.st.Rebind(`SELECT system_role FROM labels WHERE id = ?`), labelID).Scan(&roleBefore))
			require.Equal(test.incomingRole != "", roleBefore.Valid)
			id := history.persist(attrMail{
				raw:  "From: history@example.test\r\nTo: friend@example.test\r\n\r\nhistorical body",
				from: []string{"history@example.test"}, to: []string{"friend@example.test"}, labels: []int64{labelID},
			})
			addressBefore, pathBefore := attribution(t, live.st, id)
			require.Equal("history@example.test", addressBefore.String)
			wantPath := "sent"
			if test.wantInbound {
				wantPath = "inbound"
			}
			require.Equal(wantPath, pathBefore.String)
			var draftBefore bool
			require.NoError(live.st.DB().QueryRow(live.st.Rebind(`SELECT draft_authored FROM messages WHERE id = ?`), id).Scan(&draftBefore))
			require.Equal(test.wantDraft, draftBefore)

			_, err = live.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
				FromSourceID: history.source.ID, IntoSourceID: live.source.ID,
			})
			require.NoError(err)
			addressAfter, pathAfter := attribution(t, live.st, id)
			assert.Equal(wantPath, pathAfter.String)
			if test.wantInbound {
				assert.Equal("live@example.test", addressAfter.String)
				assert.Contains(searchIDs(t, live.st, "received:live@example.test"), id)
			} else {
				assert.Equal("history@example.test", addressAfter.String)
				assert.NotContains(searchIDs(t, live.st, "received:live@example.test"), id)
				assert.Contains(searchIDs(t, live.st, "account:history@example.test"), id)
			}
			labelIDs, err := live.st.MessageLabelIDsContext(t.Context(), id)
			require.NoError(err)
			require.Len(labelIDs, 1)
			var wantProviderID sql.NullString
			if test.wantCoalesced {
				assert.Equal(liveLabels[destinationID], labelIDs[0])
				wantProviderID = sql.NullString{String: destinationID, Valid: true}
			} else {
				assert.Equal(labelID, labelIDs[0])
			}
			var roleAfter string
			var providerAfter sql.NullString
			require.NoError(live.st.DB().QueryRow(live.st.Rebind(`SELECT COALESCE(system_role, ''), source_label_id FROM labels WHERE id = ?`), labelIDs[0]).Scan(&roleAfter, &providerAfter))
			if !test.wantCoalesced {
				assert.Equal(test.wantRole, roleAfter)
			}
			assert.Equal(wantProviderID, providerAfter)
			var draftAfter bool
			require.NoError(live.st.DB().QueryRow(live.st.Rebind(`SELECT draft_authored FROM messages WHERE id = ?`), id).Scan(&draftAfter))
			assert.Equal(test.wantDraft, draftAfter)
		})
	}
}
