package query_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/verification"
)

func TestVerificationQueryFragments(t *testing.T) {
	assertions := assert.New(t)

	msg := &query.MessageDetail{BodyText: "Your verification code is 482913. Magic link: https://example.com/login?token=abcdefghijklmnop"}
	for _, input := range []string{"482913", "482", "subject:913", "filename:482913.txt", "ordinary | 482913", "abcdefghijklmnop"} {
		assertions.True(query.VerificationQueryMatches(msg, query.VerificationQueryTokens(search.ParseOR(input))), input)
	}
	for _, input := range []string{"glacier", "conversation_id:482913", "account:482913"} {
		assertions.False(query.VerificationQueryMatches(msg, query.VerificationQueryTokens(search.ParseOR(input))), input)
	}
	assertions.False(query.VerificationQueryMatches(&query.MessageDetail{BodyText: "Invoice number 482913"}, []string{"482913"}))
}

func TestVerificationQueryDecodedLinkPayload(t *testing.T) {
	for _, link := range []string{
		"https://example.com/login?code=%34%38%32%39%31%33",
		"https://example.com/login/%34%38%32%39%31%33",
		"https://example.com/#token=%34%38%32%39%31%33",
		"https://example.com/#/login?code=%34%38%32%39%31%33",
		"https://example.com/#token=%2534%2538%2532%2539%2531%2533",
	} {
		t.Run(link, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			body := "Sign in: " + link + strings.Repeat(" ordinary context", 30) + " 482913"
			msg := &query.MessageDetail{BodyText: body}
			finding := verification.Detect(query.VerificationInput(msg))
			require.NotNil(finding)
			require.Len(finding.Values, 1, "the repeated digits are outside the detector cue window")
			for _, input := range []string{"482913", "subject:482", "filename:913.txt"} {
				assert.True(query.VerificationQueryMatches(msg, query.VerificationQueryTokens(search.ParseOR(input))), input)
			}
			assert.False(query.VerificationQueryMatches(msg, query.VerificationQueryTokens(search.ParseOR("glacier"))))
		})
	}
}

func TestVerificationArrivalsScopedScan(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "synthetic-source")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("sms", "other-source")
	requirements.NoError(err)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	put := func(sourceID int64, key, body string, sentAt time.Time) int64 {
		conv, err := st.EnsureConversationWithType(sourceID, "synthetic-chat", "direct_chat", "Synthetic")
		requirements.NoError(err)
		id, err := st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: conv, SourceMessageID: key, MessageType: "beeper", SentAt: sql.NullTime{Time: sentAt, Valid: true}})
		requirements.NoError(err)
		requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		return id
	}
	id := put(src.ID, "provider-message", "Your verification code is 482913. Expires in 10 minutes.", now.Add(-5*time.Minute))
	put(src.ID, "expired", "Your verification code is 572914. Expires in 1 minute.", now.Add(-30*time.Minute))
	put(src.ID, "old", "Your verification code is 692734.", now.Add(-25*time.Hour))
	put(other.ID, "outside", "Your verification code is 791245.", now.Add(-time.Minute))
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	filter := query.MessageFilter{SourceIDs: []int64{src.ID}}
	page, err := query.ScanVerifications(t.Context(), engine, filter, verification.ListOptions{}, now, nil)
	requirements.NoError(err)
	requirements.Len(page.Data, 1)
	assertions.Equal(id, page.Data[0].MessageID)
	assertions.Equal(src.ID, page.Data[0].SourceID)
	assertions.Equal("provider-message", page.Data[0].SourceMessageID)
	assertions.True(page.Data[0].Verification.Masked)
	assertions.Empty(page.Data[0].Values)
	encoded, err := json.Marshal(page)
	requirements.NoError(err)
	assertions.NotContains(string(encoded), "482913")
	assertions.NotContains(string(encoded), "791245")
	page, err = query.ScanVerifications(t.Context(), engine, filter, verification.ListOptions{IncludeExpired: true, Reveal: true}, now, nil)
	requirements.NoError(err)
	requirements.Len(page.Data, 2)
	assertions.Equal("482913", page.Data[0].Values[0].Value)
	assertions.True(page.Data[1].Verification.Expired)
	page, err = query.ScanVerifications(t.Context(), engine, filter, verification.ListOptions{Reveal: true}, now, func(sourceID int64) (bool, error) {
		assertions.Equal(src.ID, sourceID)
		return false, nil
	})
	requirements.NoError(err)
	assertions.Empty(page.Data[0].Values, "a delegated decision overrides the owner request")
	assertions.True(page.Data[0].Verification.Masked)
	page, err = query.ScanVerifications(t.Context(), engine, filter, verification.ListOptions{Since: now.Add(-48 * time.Hour), Kind: "magic_link"}, now, nil)
	requirements.NoError(err)
	assertions.Equal(now.Add(-24*time.Hour), page.Since)
	assertions.Empty(page.Data)
}

func TestVerificationArrivalsBoundedWork(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "synthetic-source")
	requirements.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "synthetic-chat", "direct_chat", "Synthetic")
	requirements.NoError(err)
	now := time.Now().UTC()
	for i := range 1005 {
		body := "Ordinary context " + strings.Repeat("data ", 10)
		if i%10 == 0 {
			body += "Your verification code is 482913"
		}
		id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: fmt.Sprint(i), MessageType: "beeper", SentAt: sql.NullTime{Time: now.Add(-time.Duration(i+1) * time.Second), Valid: true}})
		requirements.NoError(err)
		requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	}
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	started := time.Now()
	page, err := query.ScanVerifications(t.Context(), engine, query.MessageFilter{}, verification.ListOptions{}, now, nil)
	requirements.NoError(err)
	assertions.Len(page.Data, 20)
	assertions.Equal(191, page.Scanned)
	assertions.True(page.ScanTruncated)
	t.Logf("read-time classification: %d candidates in %s", page.Scanned, time.Since(started))
	page, err = query.ScanVerifications(t.Context(), engine, query.MessageFilter{}, verification.ListOptions{Kind: "magic_link"}, now, nil)
	requirements.NoError(err)
	assertions.Equal(1000, page.Scanned)
	assertions.True(page.ScanTruncated)
	assertions.Empty(page.Data)
	_, err = query.ScanVerifications(t.Context(), engine, query.MessageFilter{}, verification.ListOptions{Limit: 101}, now, nil)
	requirements.ErrorContains(err, "limit")
}
