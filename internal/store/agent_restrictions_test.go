package store_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAgentRestrictionsStagedConfiguration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("imap", "restriction@example.test")
	require.NoError(err)
	taint, err := st.AgentRestrictionTaint(ctx)
	require.NoError(err)
	assert.True(taint)
	require.NoError(st.SetAgentRestrictionTaint(ctx, false))
	taint, err = st.AgentRestrictionTaint(ctx)
	require.NoError(err)
	assert.False(taint)
	rule, err := st.AddAgentRestriction(ctx, store.AgentRestriction{Kind: "source", SourceID: source.ID, Reveal: "headers"})
	require.NoError(err)
	assert.Positive(rule.ID)
	_, err = st.AddAgentRestriction(ctx, store.AgentRestriction{Kind: "source", SourceID: -1, Reveal: "headers"})
	require.Error(err)
	_, err = st.AddAgentRestriction(ctx, store.AgentRestriction{Kind: "source", SourceID: source.ID, Reveal: "invalid"})
	require.Error(err)
	rules, err := st.ListAgentRestrictions(ctx)
	require.NoError(err)
	require.Len(rules, 1)
	assert.Equal(rule.ID, rules[0].ID)
	require.NoError(st.RemoveAgentRestriction(ctx, rule.ID))
	require.Error(st.RemoveAgentRestriction(ctx, rule.ID))
}

func TestAgentRestrictionLabelIdentityAndDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("imap", "labels@example.test")
	require.NoError(err)
	ids, err := st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{"provider-1": {Name: "Private", Type: "user"}})
	require.NoError(err)
	labelID := ids["provider-1"]
	matched, err := st.ResolveRestrictionLabels(ctx, "private", source.ID)
	require.NoError(err)
	assert.Equal([]int64{labelID}, matched)
	rule, err := st.AddAgentRestriction(ctx, store.AgentRestriction{Kind: "label", LabelID: labelID})
	require.NoError(err)
	renamed, err := st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{"provider-1": {Name: "Renamed", Type: "user"}})
	require.NoError(err)
	assert.Equal(labelID, renamed["provider-1"])
	_, err = st.ResolveRestrictionLabels(ctx, "Private", source.ID)
	require.Error(err)
	matched, err = st.ResolveRestrictionLabels(ctx, "Renamed", source.ID)
	require.NoError(err)
	assert.Equal([]int64{labelID}, matched)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM labels WHERE id=?`), labelID)
	require.Error(err)
	require.NoError(st.RemoveAgentRestriction(ctx, rule.ID))
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM labels WHERE id=?`), labelID)
	require.NoError(err)
}

func TestAgentRestrictionCheckSelectorsAndTaint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("imap", "check@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Thread")
	require.NoError(err)
	participant, err := st.EnsureParticipant("sender@example.test", "Sender", "example.test")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	labels, err := st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{"private": {Name: "Private", Type: "user"}})
	require.NoError(err)
	var first, second int64
	err = st.DB().QueryRowContext(ctx, st.Rebind(`INSERT INTO messages(conversation_id,source_id,message_type,sender_id) VALUES(?,?,?,?) RETURNING id`), conv, source.ID, "email", participant).Scan(&first)
	require.NoError(err)
	err = st.DB().QueryRowContext(ctx, st.Rebind(`INSERT INTO messages(conversation_id,source_id,message_type) VALUES(?,?,?) RETURNING id`), conv, source.ID, "email").Scan(&second)
	require.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO message_labels(message_id,label_id) VALUES(?,?)`), first, labels["private"])
	require.NoError(err)
	for _, rule := range []store.AgentRestriction{{Kind: "label", LabelID: labels["private"]}, {Kind: "person", PersonID: person.ID}, {Kind: "source", SourceID: source.ID}} {
		_, err = st.AddAgentRestriction(ctx, rule)
		require.NoError(err)
	}
	rules, err := st.CheckAgentRestrictions(ctx, first)
	require.NoError(err)
	assert.Len(rules, 3)
	rules, err = st.CheckAgentRestrictions(ctx, second)
	require.NoError(err)
	assert.Len(rules, 3)
	require.NoError(st.SetAgentRestrictionTaint(ctx, false))
	rules, err = st.CheckAgentRestrictions(ctx, second)
	require.NoError(err)
	require.Len(rules, 1)
	assert.Equal("source", rules[0].Kind)
	_, err = st.CheckAgentRestrictions(ctx, 999999)
	require.Error(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM persons WHERE id=?`), person.ID)
	require.Error(err)
}

func TestAgentRestrictionsFollowLabelMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("imap", "rename@example.test")
	require.NoError(err)
	ids, err := st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{"keep": {Name: "Before", Type: "user"}, "conflict": {Name: "After", Type: "user"}})
	require.NoError(err)
	for _, id := range ids {
		_, err = st.AddAgentRestriction(ctx, store.AgentRestriction{Kind: "label", LabelID: id})
		require.NoError(err)
	}
	renamed, err := st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{"keep": {Name: "After", Type: "user"}})
	require.NoError(err)
	assert.Equal(ids["keep"], renamed["keep"])
	rules, err := st.ListAgentRestrictions(ctx)
	require.NoError(err)
	require.Len(rules, 2)
	for _, rule := range rules {
		assert.Equal(ids["keep"], rule.LabelID)
	}
}

func TestAgentRestrictionsFollowPersonMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	first, err := st.EnsureParticipant("kept@example.test", "Kept", "example.test")
	require.NoError(err)
	second, err := st.EnsureParticipant("absorbed@example.test", "Absorbed", "example.test")
	require.NoError(err)
	kept, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(second)
	require.NoError(err)
	for _, id := range []int64{kept.ID, absorbed.ID} {
		_, err = st.AddAgentRestriction(ctx, store.AgentRestriction{Kind: "person", PersonID: id})
		require.NoError(err)
	}
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: kept.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: kept.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "restriction-merge", Actor: "test"})
	require.NoError(err)
	rules, err := st.ListAgentRestrictions(ctx)
	require.NoError(err)
	require.Len(rules, 2)
	for _, rule := range rules {
		assert.Equal(kept.ID, rule.PersonID)
	}

	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: kept.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{second},
		ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "restriction-split", Actor: "test",
	})
	require.NoError(err)
	rules, err = st.ListAgentRestrictions(ctx)
	require.NoError(err)
	require.Len(rules, 4)
	counts := map[int64]int{}
	for _, rule := range rules {
		counts[rule.PersonID]++
	}
	assert.Equal(map[int64]int{kept.ID: 2, split.NewPerson.ID: 2}, counts)
}

func TestAgentRestrictionsFollowSourceMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	from, err := st.GetOrCreateSource("imap", "history@example.test")
	require.NoError(err)
	into, err := st.GetOrCreateSource("imap", "live@example.test")
	require.NoError(err)
	fromLabels, err := st.EnsureLabelsBatch(from.ID, map[string]store.LabelInfo{"private": {Name: "Private", Type: "user"}})
	require.NoError(err)
	intoLabels, err := st.EnsureLabelsBatch(into.ID, map[string]store.LabelInfo{"private": {Name: "Private", Type: "user"}})
	require.NoError(err)
	for _, rule := range []store.AgentRestriction{{Kind: "source", SourceID: from.ID, Reveal: "minimal"}, {Kind: "label", LabelID: fromLabels["private"]}} {
		_, err = st.AddAgentRestriction(ctx, rule)
		require.NoError(err)
	}
	_, err = st.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)
	rules, err := st.ListAgentRestrictions(ctx)
	require.NoError(err)
	require.Len(rules, 4)
	var sources, labels []int64
	for _, rule := range rules {
		if rule.Kind == "source" {
			sources = append(sources, rule.SourceID)
			assert.Equal("minimal", rule.Reveal)
		}
		if rule.Kind == "label" {
			labels = append(labels, rule.LabelID)
		}
	}
	assert.ElementsMatch([]int64{from.ID, into.ID}, sources)
	assert.ElementsMatch([]int64{fromLabels["private"], intoLabels["private"]}, labels)
}

func TestAgentRestrictionLabelRecreationKeepsHistoricalTarget(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			ctx := t.Context()
			source, err := st.GetOrCreateSource("gmail", "recreated@example.test")
			require.NoError(err)
			ensure := func(providerID string) int64 {
				t.Helper()
				if batch {
					ids, err := st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{providerID: {Name: "Private", Type: "user"}})
					require.NoError(err)
					return ids[providerID]
				}
				id, err := st.EnsureLabel(source.ID, providerID, "Private", "user")
				require.NoError(err)
				return id
			}
			oldID := ensure("old-provider-id")
			rule, err := st.AddAgentRestriction(ctx, store.AgentRestriction{Kind: "label", LabelID: oldID})
			require.NoError(err)
			newID := ensure("new-provider-id")
			require.NotEqual(oldID, newID)
			var oldProviderID, oldName string
			err = st.DB().QueryRowContext(ctx, st.Rebind(`SELECT source_label_id,name FROM labels WHERE id=?`), oldID).Scan(&oldProviderID, &oldName)
			require.NoError(err)
			assert.Equal("old-provider-id", oldProviderID)
			assert.Contains(oldName, "msgvault-archive:")
			matched, err := st.ResolveRestrictionLabels(ctx, "Private", source.ID)
			require.NoError(err)
			assert.Equal([]int64{newID}, matched)
			for _, labelID := range []int64{oldID, newID} {
				conversationID, err := st.EnsureConversation(source.ID, strconv.FormatInt(labelID, 10), "Thread")
				require.NoError(err)
				var messageID int64
				err = st.DB().QueryRowContext(ctx, st.Rebind(`INSERT INTO messages(source_id,conversation_id,message_type) VALUES(?,?,?) RETURNING id`), source.ID, conversationID, "email").Scan(&messageID)
				require.NoError(err)
				_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO message_labels(message_id,label_id) VALUES(?,?)`), messageID, labelID)
				require.NoError(err)
				rules, err := st.CheckAgentRestrictions(ctx, messageID)
				require.NoError(err)
				if labelID == oldID {
					assert.Equal([]store.AgentRestriction{rule}, rules)
				} else {
					assert.Empty(rules)
				}
			}
		})
	}
}

func TestAgentRestrictionCreationRespectsSourceRemovalLock(t *testing.T) {
	for _, kind := range []string{"source", "label"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			ctx := t.Context()
			_, err := st.GetOrCreateSource("matrix", "@other:example.test")
			require.NoError(err)
			source, err := st.GetOrCreateSource("matrix", "@removal:example.test")
			require.NoError(err)
			rule := store.AgentRestriction{Kind: kind}
			if kind == "source" {
				rule.SourceID = source.ID
			} else {
				rule.LabelID, err = st.EnsureLabel(source.ID, "private", "Private", "user")
				require.NoError(err)
				require.NotEqual(source.ID, rule.LabelID, "the label must resolve its source instead of using its own ID")
			}
			release, err := st.AcquireSourceRemovalLockContext(ctx, source.ID)
			require.NoError(err)
			defer func() { require.NoError(release()) }()
			_, err = st.AddAgentRestriction(ctx, rule)
			require.ErrorIs(err, store.ErrSyncAlreadyActive)
			rules, err := st.ListAgentRestrictions(ctx)
			require.NoError(err)
			assert.Empty(rules)
			require.NoError(release())
			for range 2 {
				_, err = st.AddAgentRestriction(ctx, rule)
				require.NoError(err, "successful creation must release execution ownership")
			}
		})
	}
}

func TestAgentRestrictionSourceRemovalPreflight(t *testing.T) {
	for _, kind := range []string{"source", "label"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			ctx := t.Context()
			source, err := st.GetOrCreateSource("matrix", "@restricted:example.test")
			require.NoError(err)
			rule := store.AgentRestriction{Kind: kind}
			if kind == "source" {
				rule.SourceID = source.ID
			} else {
				rule.LabelID, err = st.EnsureLabel(source.ID, "private", "Private", "user")
				require.NoError(err)
			}
			saved, err := st.AddAgentRestriction(ctx, rule)
			require.NoError(err)
			require.ErrorIs(st.CheckSourceRemovalAllowedContext(ctx, source.ID), store.ErrSourceAgentRestriction)
			release, err := st.AcquireSourceRemovalLockContext(ctx, source.ID)
			require.ErrorIs(err, store.ErrSourceAgentRestriction)
			assert.Nil(release)
			_, _, err = st.RemoveSourceSerialized(ctx, source.ID)
			require.ErrorIs(err, store.ErrSourceAgentRestriction)
			_, err = st.GetSourceByID(source.ID)
			require.NoError(err)
			require.NoError(st.RemoveAgentRestriction(ctx, saved.ID))
			require.NoError(st.CheckSourceRemovalAllowedContext(ctx, source.ID))
		})
	}
}
