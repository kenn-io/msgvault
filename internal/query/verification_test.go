package query

import (
	"encoding/json"
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
	before, err := json.Marshal(msg)
	requirements.NoError(err)
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
	after, err := json.Marshal(msg)
	requirements.NoError(err)
	assertions.Equal(before, after)
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

	msg := &MessageDetail{
		ID: 17, SourceID: 3, BodyText: "Your verification code is 482913",
		Labels: []string{"482913"},
	}
	row := MessageSummary{ID: 17, SourceID: 3, Subject: "482913", Snippet: "482913", MatchSnippet: "482", BodyContextSnippets: []string{"913"}, Labels: []string{"482913"}}
	masked := PrepareSummaryVerification(row, msg, false, time.Time{})
	assertions.NotContains(masked.Subject, "482913")
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

func TestVerificationSummaryRefreshesStaleCachedContent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	row := MessageSummary{
		ID: 17, SourceID: 3, Subject: "Your verification code is 482913", Snippet: "482913",
		MatchSnippet: "code 482913", BodyContextSnippets: []string{"Use 482913"},
		BodyText: "Your verification code is 482913", FromEmail: "old@example.test",
		FromName: "Old 482913", ConversationTitle: "Code 482913",
		WebURL: "https://example.test/messages/17?code=482913", Labels: []string{"482913"},
	}
	canonical := &MessageDetail{
		ID: 17, SourceID: 3, Subject: "Re: Project update", Snippet: "A routine project update",
		BodyText: "Canonical body", ConversationTitle: "Project updates",
		WebURL: "https://example.test/messages/17", From: []Address{{Email: "new@example.test", Name: "New sender"}},
		Labels: []string{"project"},
	}

	got := PrepareSummaryVerification(row, canonical, false, time.Time{})
	requirements.Nil(got.Verification)
	assertions.Equal(canonical.Subject, got.Subject)
	assertions.Equal(canonical.Snippet, got.Snippet)
	assertions.Equal("new@example.test", got.FromEmail)
	assertions.Equal("New sender", got.FromName)
	assertions.Equal(canonical.ConversationTitle, got.ConversationTitle)
	assertions.Equal(canonical.WebURL, got.WebURL)
	assertions.Equal([]string{"project"}, got.Labels)
	assertions.Empty(got.MatchSnippet)
	assertions.Empty(got.BodyContextSnippets)
	assertions.Empty(got.BodyText)
	assertions.Equal("Your verification code is 482913", row.Subject,
		"refreshing a cached summary must not mutate the caller's row")
}

func TestVerificationSummaryWithholdsStaleCachedExcerpts(t *testing.T) {
	assertions := assert.New(t)
	row := MessageSummary{
		ID: 81, SourceID: 3, Subject: "Routine status", Snippet: "Routine update",
		MatchSnippet: "Old code … 482913", BodyContextSnippets: []string{"Use the old code 482913"},
	}
	canonical := &MessageDetail{
		ID: 81, SourceID: 3, Subject: row.Subject, Snippet: row.Snippet,
		BodyText: "The current message is a routine project update.",
	}

	got := PrepareSummaryVerification(row, canonical, false, time.Time{})

	assertions.Empty(got.MatchSnippet)
	assertions.Empty(got.BodyContextSnippets)
	assertions.True(got.BodyContextSnippetsTruncated)
	assertions.Empty(got.BodyText)
	assertions.False(row.BodyContextSnippetsTruncated, "refreshing a summary must not mutate the caller's row")
}

func TestVerificationSummaryPreservesCanonicalBodyContext(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	body := "An ordinary message contains statusneedle for the body search."
	context := "contains statusneedle for the body search"
	row := MessageSummary{
		ID: 81, SourceID: 3, Subject: "Routine status",
		BodyContextSnippets: []string{context}, BodyContextSnippetsTruncated: true,
	}
	canonical := &MessageDetail{ID: 81, SourceID: 3, Subject: row.Subject, BodyText: body}

	got := PrepareSummaryVerification(row, canonical, false, time.Time{})

	requirements.Nil(got.Verification)
	assertions.Equal([]string{context}, got.BodyContextSnippets)
	assertions.True(got.BodyContextSnippetsTruncated)
	assertions.Empty(got.BodyText)
}

func TestVerificationSummaryValidatesPhoneAgainstCanonicalSender(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rowPhone  string
		wantPhone string
	}{
		{name: "preserve canonical phone when another field is stale", rowPhone: "+15551234567", wantPhone: "+15551234567"},
		{name: "clear a phone that differs from canonical sender", rowPhone: "+15557654321"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			row := MessageSummary{
				ID: 81, SourceID: 3, Subject: "Cached subject",
				FromEmail: "+15551234567", FromPhone: tc.rowPhone,
			}
			canonical := &MessageDetail{
				ID: 81, SourceID: 3, Subject: "Canonical subject",
				From: []Address{{Email: "+15551234567"}},
			}

			got := PrepareSummaryVerification(row, canonical, false, time.Time{})

			assertions.Equal("Canonical subject", got.Subject)
			assertions.Equal(tc.wantPhone, got.FromPhone)
		})
	}
}
