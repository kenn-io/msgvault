package emailfamily

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"testing/quick"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

func workMember(id, source int64, rawHeaders string) Member {
	return Member{MessageID: id, SourceID: source, SourceType: "gmail", Linkage: mime.ExtractEmailLinkage([]byte(rawHeaders + "\r\n\r\n[QA] body"))}
}

func workActivity(id, source int64, header string, incoming bool, second int) Activity {
	return Activity{Member: workMember(id, source, header), OccurredAt: time.Date(2026, 1, 1, 0, 0, second, 0, time.UTC), Incoming: incoming, AttributionComplete: true}
}

func completedReply(id int64, target string, second int) Activity {
	outgoing := workActivity(id, 2, fmt.Sprintf("Message-ID: <sent-%d@example.test>\r\nIn-Reply-To: <%s>", id, target), false, second)
	outgoing.ConfirmedSourceSender = true
	outgoing.PositiveSent = true
	outgoing.ExternalRecipient = true
	return outgoing
}

func TestEmailWorkHandlingDirectReplyClosesAncestorsNotSiblingsOrNewMail(t *testing.T) {
	assertions := assert.New(t)
	root := workActivity(1, 1, "Message-ID: <root@example.test>", true, 1)
	branch := workActivity(2, 1, "Message-ID: <branch@example.test>\r\nIn-Reply-To: <root@example.test>", true, 2)
	sibling := workActivity(3, 3, "Message-ID: <sibling@example.test>\r\nIn-Reply-To: <root@example.test>", true, 3)
	outgoing := completedReply(4, "branch@example.test", 4)
	got := Evaluate([]Activity{root, branch, sibling, outgoing})
	assertions.Equal("actionable", got.State)
	assertions.Equal([]int64{3}, got.OutstandingEventIDs)
	assertions.Equal([]int64{4}, got.OutboundEvidenceIDs)
	handled := Evaluate([]Activity{root, branch, outgoing})
	assertions.Equal("handled", handled.State)
	assertions.Empty(handled.OutstandingEventIDs)
	newer := workActivity(5, 1, "Message-ID: <newer@example.test>\r\nIn-Reply-To: <sent-4@example.test>", true, 5)
	reopened := Evaluate([]Activity{root, branch, outgoing, newer})
	assertions.Equal("actionable", reopened.State)
	assertions.Equal([]int64{5}, reopened.OutstandingEventIDs)
}

func TestEmailWorkHandlingForwardAndDuplicateCopiesRemainOneReceivedEvent(t *testing.T) {
	assertions := assert.New(t)
	incoming := workActivity(1, 1, "Message-ID: <original@example.test>", true, 1)
	otherCopy := incoming
	otherCopy.MessageID = 2
	otherCopy.SourceID = 3
	outgoing := completedReply(3, "unused@example.test", 3)
	outgoing.Linkage.Replies = nil
	outgoing.Linkage.AttachedOriginals = []string{"original@example.test"}
	before := Evaluate([]Activity{otherCopy, incoming})
	assertions.Equal([]int64{1}, before.OutstandingEventIDs)
	got := Evaluate([]Activity{otherCopy, outgoing, incoming})
	assertions.Equal("handled", got.State)
	assertions.Equal([]int64{3}, got.OutboundEvidenceIDs)
}

func TestEmailWorkHandlingRequiresEveryPositiveCompletionFact(t *testing.T) {
	incoming := workActivity(1, 1, "Message-ID: <target@example.test>", true, 2)
	good := completedReply(2, "target@example.test", 3)
	for _, tc := range []struct {
		name   string
		mutate func(*Activity)
	}{
		{"unconfirmed sender", func(m *Activity) { m.ConfirmedSourceSender = false }},
		{"no positive Sent", func(m *Activity) { m.PositiveSent = false }},
		{"draft", func(m *Activity) { m.Draft = true }},
		{"self only", func(m *Activity) { m.ExternalRecipient = false }},
		{"unknown identity", func(m *Activity) { m.AttributionComplete = false }},
		{"old outgoing", func(m *Activity) { m.OccurredAt = incoming.OccurredAt.Add(-time.Second) }},
		{"same timestamp", func(m *Activity) { m.OccurredAt = incoming.OccurredAt }},
		{"missing date", func(m *Activity) { m.OccurredAt = time.Time{} }},
		{"References only", func(m *Activity) {
			m.Linkage.Replies = nil
			m.Linkage.References = []string{"target@example.test"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			outgoing := good
			tc.mutate(&outgoing)
			got := Evaluate([]Activity{incoming, outgoing})
			want := "actionable"
			if tc.name == "unknown identity" || tc.name == "old outgoing" || tc.name == "same timestamp" || tc.name == "missing date" {
				want = "unknown"
			}
			assertions.Equal(want, got.State)
			assertions.Equal([]int64{1}, got.OutstandingEventIDs)
			assertions.Empty(got.OutboundEvidenceIDs)
		})
	}
}

func TestEmailWorkHandlingIncompleteFactsStayVisibleAndScoped(t *testing.T) {
	assertions := assert.New(t)
	incoming := workActivity(1, 1, "Message-ID: <target@example.test>", true, 1)
	outgoing := completedReply(2, "target@example.test", 2)
	scoped := Evaluate([]Activity{incoming})
	assertions.Equal("actionable", scoped.State)
	assertions.Equal([]int64{1}, scoped.OutstandingEventIDs)
	assertions.Empty(scoped.OutboundEvidenceIDs)
	incoming.Linkage.Outcome = "missing_raw"
	incomplete := Evaluate([]Activity{incoming, outgoing})
	assertions.Equal("unknown", incomplete.State)
	assertions.Contains(incomplete.Reasons, "linkage_unavailable")
	// Removing a previously valid outgoing cannot leave cached handling behind.
	incoming.Linkage.Outcome = "complete"
	recovered := Evaluate([]Activity{incoming})
	assertions.Equal("actionable", recovered.State)
	empty := Evaluate(nil)
	assertions.Equal("unknown", empty.State)
	assertions.True(slices.Contains(empty.Reasons, "no_received_item"))
}

func TestEmailWorkHandlingLongDirectAncestry(t *testing.T) {
	assertions := assert.New(t)
	const count = 5000
	members := make([]Activity, 0, count)
	for id := 1; id <= count; id++ {
		header := fmt.Sprintf("Message-ID: <m-%d@example.test>", id)
		if id > 1 {
			header += fmt.Sprintf("\r\nIn-Reply-To: <m-%d@example.test>", id-1)
		}
		member := workActivity(int64(id), 1, header, id%2 == 1, 1)
		member.OccurredAt = time.Unix(int64(id), 0)
		if id%2 == 0 {
			member.SourceID = 2
			member.ConfirmedSourceSender = true
			member.PositiveSent = true
			member.ExternalRecipient = true
		}
		members = append(members, member)
	}
	// Reversed ingestion order cannot change causal completion. This exercises
	// thousands of real parsed direct links without a throughput/time assertion.
	slices.Reverse(members)
	got := Evaluate(members)
	assertions.Equal("handled", got.State)
	assertions.Empty(got.OutstandingEventIDs)
	assertions.Len(got.OutboundEvidenceIDs, count/2)
	assertions.Equal(int64(2), got.OutboundEvidenceIDs[0])
	assertions.Equal(int64(count), got.OutboundEvidenceIDs[len(got.OutboundEvidenceIDs)-1])
}

func TestHandlingLocalDecisionCoversExactDuplicateCopies(t *testing.T) {
	assert := assert.New(t)
	a := workActivity(1, 1, "Message-ID: <same@example.test>", true, 1)
	b := a
	b.MessageID = 2
	b.SourceID = 2
	a.LocalHandled = true
	got := Evaluate([]Activity{a, b})
	assert.Equal("actionable", got.State)
	assert.Equal([]int64{1}, got.OutstandingEventIDs)
	b.LocalHandled = true
	assert.Equal("handled", Evaluate([]Activity{a, b}).State)
	b.LocalHandled = false
	b.Linkage.Outcome = "missing_raw"
	assert.Equal("unknown", Evaluate([]Activity{a, b}).State)
	b.LocalHandled = true
	assert.Equal("handled", Evaluate([]Activity{a, b}).State)
}

func TestHandlingOutboundOnlyIsUnknown(t *testing.T) {
	got := Evaluate([]Activity{completedReply(1, "missing@example.test", 2)})
	assert.Equal(t, "unknown", got.State)
	assert.Contains(t, got.Reasons, "no_received_item")
}

func TestHandlingDetectsInvertedIncomingAncestryWithoutOutbound(t *testing.T) {
	a := workActivity(1, 1, "Message-ID: <a@example.test>", true, 2)
	b := workActivity(2, 1, "Message-ID: <b@example.test>\r\nIn-Reply-To: <a@example.test>", true, 1)
	got := Evaluate([]Activity{a, b})
	assert.Equal(t, "unknown", got.State)
	assert.Contains(t, got.Reasons, "chronology_unavailable")
}

func TestHandlingLocalCoverageSurvivesUnprovableReplyChronology(t *testing.T) {
	a := workActivity(1, 1, "Message-ID: <a@example.test>", true, 1)
	a.OccurredAt = time.Time{}
	a.LocalHandled = true
	assert.Equal(t, "handled", Evaluate([]Activity{a}).State)
	got := Evaluate([]Activity{a, completedReply(2, "a@example.test", 2)})
	assert.Equal(t, "handled", got.State)
}

func TestHandlingCompletedDescendantHonorsLocallyHandledUndatedAncestor(t *testing.T) {
	assert := assert.New(t)
	a := workActivity(1, 1, "Message-ID: <a@example.test>", true, 1)
	a.OccurredAt = time.Time{}
	a.LocalHandled = true
	b := workActivity(2, 1, "Message-ID: <b@example.test>\r\nIn-Reply-To: <a@example.test>", true, 2)
	c := completedReply(3, "b@example.test", 3)

	got := Evaluate([]Activity{a, b, c})
	assert.Equal("handled", got.State)
	assert.Empty(got.OutstandingEventIDs)
	assert.Equal([]int64{3}, got.OutboundEvidenceIDs)
	assert.NotContains(got.Reasons, "chronology_unavailable")
}

func TestHandlingOutboundReplyTraversesLocallyHandledAncestor(t *testing.T) {
	root := workActivity(1, 1, "Message-ID: <root@example.test>", true, 1)
	intermediate := workActivity(2, 1, "Message-ID: <intermediate@example.test>\r\nIn-Reply-To: <root@example.test>", true, 2)
	intermediate.LocalHandled = true
	newer := workActivity(3, 1, "Message-ID: <newer@example.test>\r\nIn-Reply-To: <intermediate@example.test>", true, 3)
	outbound := completedReply(4, "newer@example.test", 4)

	got := Evaluate([]Activity{root, intermediate, newer, outbound})
	assert.Equal(t, "handled", got.State)
	assert.Empty(t, got.OutstandingEventIDs)
	assert.Equal(t, []int64{4}, got.OutboundEvidenceIDs)
}

func TestHandlingIncompleteOutboundStillExplainsNoReceivedItem(t *testing.T) {
	a := completedReply(1, "missing@example.test", 1)
	a.AttributionComplete = false
	got := Evaluate([]Activity{a})
	assert.Equal(t, "unknown", got.State)
	assert.Contains(t, got.Reasons, "no_received_item")
	assert.Contains(t, got.Reasons, "identity_unavailable")
}

func TestHandlingEverySubsetOfDuplicateLocalCoverage(t *testing.T) {
	assert := assert.New(t)
	for count := 1; count <= 8; count++ {
		for mask := range 1 << count {
			members := make([]Activity, count)
			for i := range members {
				members[i] = workActivity(int64(i+1), int64(i+1), "Message-ID: <shared@example.test>", true, 1)
				members[i].LocalHandled = mask&(1<<i) != 0
			}
			got := Evaluate(members)
			if mask == (1<<count)-1 {
				assert.Equal("handled", got.State)
				assert.Empty(got.OutstandingEventIDs)
			} else {
				assert.Equal("actionable", got.State)
				assert.Equal([]int64{1}, got.OutstandingEventIDs)
			}
		}
	}
}

func TestHandlingReferencesCannotConferAuthority(t *testing.T) {
	incoming := workActivity(1, 1, "Message-ID: <request@example.test>", true, 1)
	outgoing := completedReply(2, "other@example.test", 2)
	want := Evaluate([]Activity{incoming, outgoing})
	require.NoError(t, quick.Check(func(seed []byte) bool {
		changed := outgoing
		changed.Linkage.References = []string{"request@example.test", fmt.Sprintf("id%x@example.test", seed)}
		return reflect.DeepEqual(want, Evaluate([]Activity{incoming, changed}))
	}, &quick.Config{MaxCount: 500}))
}

func TestHandlingPermutationInvariant(t *testing.T) {
	members := []Activity{
		workActivity(1, 1, "Message-ID: <root@example.test>", true, 1),
		workActivity(2, 1, "Message-ID: <child@example.test>\r\nIn-Reply-To: <root@example.test>", true, 2),
		workActivity(3, 2, "Message-ID: <sibling@example.test>\r\nIn-Reply-To: <root@example.test>", true, 3),
		completedReply(4, "child@example.test", 4),
	}
	want := Evaluate(members)
	require.NoError(t, quick.Check(func(seed uint64) bool {
		reordered := slices.Clone(members)
		for i := len(reordered) - 1; i > 0; i-- {
			j := int(seed % uint64(i+1))
			seed /= uint64(i + 1)
			reordered[i], reordered[j] = reordered[j], reordered[i]
		}
		return reflect.DeepEqual(want, Evaluate(reordered))
	}, &quick.Config{MaxCount: 500}))
}
