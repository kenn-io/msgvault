package query

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/verification"
)

func TestVerificationCopiesContentAndPreservesStructure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	sent := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	msg := &MessageDetail{
		ID: 482913, SourceID: 7, SourceMessageID: "provider-482913", ConversationID: 482913,
		Subject: "Your verification code is 482913", Snippet: "482913", MessageType: "beeper", SentAt: sent,
		BodyText:          "Your verification code is 482913. 482 913.",
		BodyHTML:          `<p>Your verification code is 482913</p>`,
		ConversationTitle: "482913", From: []Address{{Email: "482913@example.com", Name: "482913"}},
		To: []Address{{Email: "482913@example.org"}}, Labels: []string{"482913"},
		Attachments: []AttachmentInfo{{ID: 482913, Filename: "482913.txt", URL: "https://example.com/482913", ContentHash: "482913"}},
		Calendar:    &store.CalendarProjection{},
	}
	beforeFrom := slices.Clone(msg.From)
	beforeTo := slices.Clone(msg.To)
	beforeLabels := slices.Clone(msg.Labels)
	beforeAttachments := slices.Clone(msg.Attachments)
	beforeCalendar := *msg.Calendar
	masked := PrepareVerification(msg, false, sent)
	requirements.NotNil(masked.Verification)
	assertions.True(masked.Verification.Masked)
	assertions.Contains(masked.Verification.MaskedFields, "body_text")
	assertions.Equal(int64(482913), masked.ID)
	assertions.Equal("provider-482913", masked.SourceMessageID)
	assertions.Equal(msg.SentAt, masked.SentAt)
	assertions.Equal(msg.Attachments[0].ContentHash, masked.Attachments[0].ContentHash)
	assertions.Equal(verification.CodePlaceholder+".txt", masked.Attachments[0].Filename)
	assertions.Nil(masked.Calendar)
	for _, content := range []string{masked.Subject, masked.Snippet, masked.BodyText, masked.BodyHTML, masked.From[0].Email, masked.From[0].Name, masked.To[0].Email, masked.Labels[0], masked.ConversationTitle} {
		assertions.NotContains(content, "482913")
		assertions.NotContains(content, "482 913")
	}
	assertions.Equal("Your verification code is 482913", msg.Subject)
	assertions.Equal("482913", msg.Snippet)
	assertions.Equal("Your verification code is 482913. 482 913.", msg.BodyText)
	assertions.Equal(`<p>Your verification code is 482913</p>`, msg.BodyHTML)
	assertions.Equal("482913", msg.ConversationTitle)
	assertions.Equal(beforeFrom, msg.From)
	assertions.Equal(beforeTo, msg.To)
	assertions.Equal(beforeLabels, msg.Labels)
	assertions.Equal(beforeAttachments, msg.Attachments)
	assertions.Equal(beforeCalendar, *msg.Calendar)
	revealed := PrepareVerification(msg, true, sent)
	assertions.Equal(msg.BodyText, revealed.BodyText)
	assertions.False(revealed.Verification.Masked)
	assertions.Equal(masked, PrepareVerification(masked, false, sent))
}

func TestVerificationWithholdsFragmentedHTML(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	markup := "<p>Your verification code is <b>AB4</b><span>C9D</span></p>"
	msg := &MessageDetail{ID: 17, SourceID: 3, BodyHTML: markup}
	masked := PrepareVerification(msg, false, time.Time{})
	requirements.NotNil(masked.Verification)
	assertions.Equal(verification.ContentPlaceholder, masked.BodyHTML)
	assertions.Contains(masked.Verification.MaskedFields, "body_html")
	assertions.Equal(markup, msg.BodyHTML)
	assertions.Equal(markup, PrepareVerification(msg, true, time.Time{}).BodyHTML)
}

func TestVerificationSummaryHydrationContext(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	msg := &MessageDetail{ID: 17, SourceID: 3, Subject: "482913", Snippet: "482913", BodyText: "Your verification code is 482913"}
	row := MessageSummary{ID: 17, SourceID: 3, Subject: "482913", Snippet: "482913", MatchSnippet: "482", BodyContextSnippets: []string{"913"}, Labels: []string{"482913"}}
	masked := PrepareSummaryVerification(row, msg, false, time.Time{})
	assertions.NotContains(masked.Subject, "482913")
	assertions.NotContains(masked.Snippet, "482913")
	assertions.Empty(masked.MatchSnippet)
	assertions.Empty(masked.BodyContextSnippets)
	assertions.Empty(masked.BodyText, "a search read must not acquire the canonical body")
	assertions.Equal("482913", row.Subject)
	assertions.Equal([]string{"482913"}, row.Labels)
	requirements.NotNil(masked.Verification)
	assertions.True(masked.Verification.Masked)
	assertions.Contains(masked.Verification.MaskedFields, "labels")
	assertions.Equal(row.ID, masked.ID)
}

func TestVerificationWithholdsCroppedSnippet(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	canonical := &MessageDetail{ID: 17, SourceID: 3, Snippet: "AB4C...", BodyText: "Your verification code is AB4C9D"}
	detail := PrepareVerification(canonical, false, time.Time{})
	requirements.NotNil(detail.Verification)
	assertions.Equal(verification.ContentPlaceholder, detail.Snippet)
	assertions.Contains(detail.Verification.MaskedFields, "snippet")
	row := MessageSummary{ID: 17, SourceID: 3, Snippet: "AB4C..."}
	summary := PrepareSummaryVerification(row, canonical, false, time.Time{})
	requirements.NotNil(summary.Verification)
	assertions.Equal(verification.ContentPlaceholder, summary.Snippet)
	assertions.Contains(summary.Verification.MaskedFields, "snippet")
	assertions.Equal("AB4C...", canonical.Snippet)
	assertions.Equal("AB4C...", PrepareVerification(canonical, true, time.Time{}).Snippet)
	assertions.Equal(row.Snippet, PrepareSummaryVerification(row, canonical, true, time.Time{}).Snippet)
}

func TestVerificationRefreshesStaleSummaryContent(t *testing.T) {
	assertions := assert.New(t)
	canonical := &MessageDetail{
		ID: 17, SourceID: 3, Subject: "Updated status", Snippet: "The deployment finished",
		BodyText: "The deployment finished successfully",
	}
	row := MessageSummary{
		ID: 17, SourceID: 3, Subject: "Your verification code is 482913",
		Snippet: "Your verification code is 482913", MatchSnippet: "verification code is 482913",
		BodyContextSnippets: []string{"verification code is 482913"}, BodyContextSnippetsTruncated: true,
	}

	got := PrepareSummaryVerification(row, canonical, false, time.Time{})

	assertions.Equal(canonical.Subject, got.Subject)
	assertions.Equal(canonical.Snippet, got.Snippet)
	assertions.Empty(got.MatchSnippet)
	assertions.Empty(got.BodyContextSnippets)
	assertions.True(got.BodyContextSnippetsTruncated)
	assertions.Nil(got.Verification)
}

func TestVerificationPreservesCanonicalBodyContexts(t *testing.T) {
	assertions := assert.New(t)
	canonical := &MessageDetail{
		ID: 17, Subject: "Routine notice", Snippet: "The picnic starts Monday",
		BodyText: "The team picnic starts Monday at noon.",
	}
	contexts := []string{"picnic starts Monday at noon"}
	row := MessageSummary{
		ID: 17, Subject: canonical.Subject, Snippet: canonical.Snippet,
		BodyContextSnippets: contexts,
	}

	got := PrepareSummaryVerification(row, canonical, false, time.Time{})

	assertions.Equal(contexts, got.BodyContextSnippets)
	assertions.False(got.BodyContextSnippetsTruncated)
	assertions.Nil(got.Verification)
	assertions.Equal(contexts, row.BodyContextSnippets, "verification must not mutate the input row")
}

func TestVerificationMarksWithheldStaleBodyContextsTruncated(t *testing.T) {
	assertions := assert.New(t)
	canonical := &MessageDetail{
		ID: 17, Subject: "Routine notice", Snippet: "The picnic starts Monday",
		BodyText: "The team picnic starts Monday at noon.",
	}
	row := MessageSummary{
		ID: 17, Subject: canonical.Subject, Snippet: canonical.Snippet,
		BodyContextSnippets: []string{"Your verification code is 482913"},
	}

	got := PrepareSummaryVerification(row, canonical, false, time.Time{})

	assertions.Empty(got.BodyContextSnippets)
	assertions.True(got.BodyContextSnippetsTruncated)
	assertions.Nil(got.Verification)
}

func TestVerificationWithholdsUnverifiableSummaryExcerpts(t *testing.T) {
	assertions := assert.New(t)
	canonical := &MessageDetail{
		ID: 17, SourceID: 3, Subject: "Routine notice", Snippet: "The picnic starts Monday",
		BodyText: "The team picnic starts Monday at noon.",
	}
	row := MessageSummary{
		ID: 17, SourceID: 3, Subject: canonical.Subject, Snippet: canonical.Snippet,
		MatchSnippet:                 "Your verification code is 482913",
		BodyContextSnippets:          []string{"Your verification code is 482913. This old body excerpt is stale."},
		BodyContextSnippetsTruncated: true,
	}

	got := PrepareSummaryVerification(row, canonical, false, time.Time{})

	assertions.Equal(canonical.Subject, got.Subject)
	assertions.Equal(canonical.Snippet, got.Snippet)
	assertions.Empty(got.MatchSnippet)
	assertions.Empty(got.BodyContextSnippets)
	assertions.True(got.BodyContextSnippetsTruncated)
	assertions.Empty(got.BodyText, "a search read must not acquire the canonical body")
	assertions.Nil(got.Verification, "the edited canonical body no longer contains a finding")
	assertions.Equal("Your verification code is 482913", row.MatchSnippet)
	assertions.Equal([]string{"Your verification code is 482913. This old body excerpt is stale."}, row.BodyContextSnippets)
}
