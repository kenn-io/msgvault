package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
)

func TestNativeInboxReconciliationUpdatesAccountAttribution(t *testing.T) {
	for _, operation := range []string{"tag snapshot", "Gmail metadata refresh", "Inbox observation", "Microsoft folder definition"} {
		t.Run(operation, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			provider := "gmail"
			if operation == "Microsoft folder definition" {
				provider = "msmail"
			}
			f := newAttrFixture(t, provider, attrSink)
			f.confirm(attrSink)
			folderID := "SENT"
			info := store.LabelInfo{Name: "SENT", Type: "system", SystemRole: store.LabelSystemRoleSent}
			if provider == "msmail" {
				folderID = "folder-1"
				info = store.LabelInfo{Name: "Inbox", Type: "system"}
			}
			labels, err := f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{folderID: info})
			requirements.NoError(err)
			mail := attrMail{raw: "Delivered-To: " + attrSink + "\r\n\r\nbody", from: []string{"stranger@example.com"}, sourceMsgKey: "native-attribution"}
			if provider == "msmail" {
				mail.labels = []int64{labels[folderID]}
			}
			id := f.persist(mail)
			address, path := attribution(t, f.st, id)
			requirements.Equal("inbound", path.String)
			requirements.Equal(attrSink, address.String)
			switch operation {
			case "tag snapshot":
				target, err := f.st.EmailTagTargetContext(t.Context(), id, "")
				requirements.NoError(err)
				requirements.NoError(f.st.SaveEmailTagsContext(t.Context(), target, &emailtags.Result{Provider: "gmail", Tags: []string{"SENT"}, Verified: true}))
			case "Gmail metadata refresh":
				_, err := f.st.RefreshGmailInboxLabelsContext(t.Context(), f.source.ID, id, mail.sourceMsgKey, []int64{labels[folderID]}, store.GmailInboxObservation{Tags: []string{"SENT"}, HistoryID: 1, ObservedAt: time.Now().UTC()})
				requirements.NoError(err)
			case "Inbox observation":
				target := inboxcontrol.Target{SourceID: f.source.ID, SourceType: "gmail", SourceIdentifier: attrSink, AccountID: attrSink, Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: mail.sourceMsgKey}
				before := inboxcontrol.State{Target: target, ObservedAt: time.Now().UTC()}
				after := inboxcontrol.State{Target: target, Tags: []string{"SENT"}, ObservedAt: before.ObservedAt.Add(time.Second)}
				requirements.NoError(f.st.ReconcileInboxProviderState(t.Context(), target, before, after))
			case "Microsoft folder definition":
				_, err := f.st.EnsureMicrosoftMailFoldersContext(t.Context(), f.source.ID, map[string]store.LabelInfo{folderID: {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent}})
				requirements.NoError(err)
			}
			address, path = attribution(t, f.st, id)
			assertions.Equal("sent", path.String)
			assertions.False(address.Valid, "sent mail from an unconfirmed sender must not acquire the delivery account")
		})
	}
}
