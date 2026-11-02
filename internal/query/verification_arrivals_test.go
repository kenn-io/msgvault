package query_test

import (
	"context"
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

type insertAfterFirstVerificationBatchEngine struct {
	query.Engine

	afterFirstPage func() error
	calls          int
}

func (e *insertAfterFirstVerificationBatchEngine) ListMessages(ctx context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
	rows, err := e.Engine.ListMessages(ctx, filter)
	if err != nil {
		return nil, err
	}
	e.calls++
	if e.calls == 1 && e.afterFirstPage != nil {
		if err := e.afterFirstPage(); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

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

func TestVerificationQueryIgnoresPublicLinkComponents(t *testing.T) {
	tests := []struct {
		name  string
		link  string
		query string
	}{
		{name: "sender address host", link: "https://example.com/login?token=abcdefghijklmnop", query: "from:sender@example.com"},
		{name: "endpoint", link: "https://example.com/login?token=abcdefghijklmnop", query: "login"},
		{name: "sign-in route", link: "https://example.com/sign-in?token=abcdefghijklmnop", query: "sign-in"},
		{name: "long public endpoint email term", link: "https://example.com/email-verification?token=abcdefghijklmnop", query: "email"},
		{name: "long public endpoint verification term", link: "https://example.com/email-verification?token=abcdefghijklmnop", query: "verification"},
		{name: "encoded long public endpoint", link: "https://example.com/email%2Dverification?token=abcdefghijklmnop", query: "verification"},
		{name: "query key", link: "https://example.com/login?token=abcdefghijklmnop", query: "token"},
		{name: "hostname", link: "https://example.com/login?token=abcdefghijklmnop", query: "example.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msg := &query.MessageDetail{BodyText: "Sign in: " + test.link}
			require.NotNil(t, verification.Detect(query.VerificationInput(msg)))
			assert.False(t, query.VerificationQueryMatches(msg, query.VerificationQueryTokens(search.ParseOR(test.query))), test.query)
			assert.True(t, query.VerificationQueryMatches(msg, query.VerificationQueryTokens(search.ParseOR("abcdefghijklmnop"))))
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

func TestVerificationArrivalsUseReceivedAtAndSentAtFallback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "received-source")
	requirements.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "received-chat", "direct_chat", "Synthetic")
	requirements.NoError(err)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	body := "Your verification code is 482913. Expires in 10 minutes."
	put := func(key string, sentAt time.Time, receivedAt sql.NullTime) int64 {
		id, err := st.UpsertMessage(&store.Message{
			SourceID: src.ID, ConversationID: conv, SourceMessageID: key, MessageType: "beeper",
			SentAt: sql.NullTime{Time: sentAt, Valid: true}, ReceivedAt: receivedAt,
		})
		requirements.NoError(err)
		requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		return id
	}

	delayedSentAt := now.Add(-2 * time.Hour)
	delayedReceivedAt := now.Add(-15 * time.Minute)
	delayedID := put("delayed", delayedSentAt, sql.NullTime{Time: delayedReceivedAt, Valid: true})
	put("received-too-old", now.Add(-15*time.Minute), sql.NullTime{Time: now.Add(-2 * time.Hour), Valid: true})
	fallbackSentAt := now.Add(-20 * time.Minute)
	fallbackID := put("sent-time-fallback", fallbackSentAt, sql.NullTime{})

	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	page, err := query.ScanVerifications(t.Context(), engine,
		query.MessageFilter{SourceIDs: []int64{src.ID}},
		verification.ListOptions{Since: now.Add(-time.Hour), Limit: 10, IncludeExpired: true}, now, nil)
	requirements.NoError(err)
	requirements.Len(page.Data, 2)
	assertions.Equal(delayedID, page.Data[0].MessageID, "arrival order follows receipt time")
	assertions.Equal(fallbackID, page.Data[1].MessageID, "missing receipt time falls back to sent time")
	assertions.Equal(delayedReceivedAt, page.Data[0].Verification.ReceivedAt)
	assertions.Equal(fallbackSentAt, page.Data[1].Verification.ReceivedAt)
	assertions.Equal(2, page.Scanned)
}

func TestVerificationArrivalsOrderMixedOffsetReceivedAtBeforeLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "mixed-offset-source")
	requirements.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "mixed-offset-chat", "direct_chat", "Synthetic")
	requirements.NoError(err)
	now := time.Date(2026, 1, 2, 14, 0, 0, 0, time.UTC)
	put := func(key, receivedAt, body string) int64 {
		id, err := st.UpsertMessage(&store.Message{
			SourceID: src.ID, ConversationID: conv, SourceMessageID: key, MessageType: "beeper",
			SentAt: sql.NullTime{Time: now, Valid: true}, ReceivedAt: sql.NullTime{Time: now, Valid: true},
		})
		requirements.NoError(err)
		requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET received_at = ? WHERE id = ?`, receivedAt, id)
		requirements.NoError(err)
		return id
	}
	put("older", "2026-01-02 12:30:00+02:00", "Your verification code is 482913.")
	newerID := put("newer", "2026-01-02 11:00:00-02:00", "Your verification code is 572914.")

	engine := query.NewSQLiteEngine(st.DB())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	page, err := query.ScanVerifications(t.Context(), engine,
		query.MessageFilter{SourceIDs: []int64{src.ID}}, verification.ListOptions{
			Since: now.Add(-24 * time.Hour), Limit: 1,
		}, now, nil)
	requirements.NoError(err)
	requirements.Len(page.Data, 1)
	assertions.Equal(newerID, page.Data[0].MessageID, "limited scan returns the chronologically newest verification")
}

func TestVerificationArrivalsReadSnapshotPreventsOffsetDuplicates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "snapshot-source")
	requirements.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "snapshot-chat", "direct_chat", "Synthetic")
	requirements.NoError(err)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var targetID int64
	for i := range 101 {
		body := "Ordinary archived message"
		if i == 99 {
			body = "Your verification code is 482913. Expires in 10 minutes."
		}
		id, err := st.UpsertMessage(&store.Message{
			SourceID: src.ID, ConversationID: conv, SourceMessageID: fmt.Sprintf("original-%03d", i),
			MessageType: "beeper", SentAt: sql.NullTime{Time: now.Add(-time.Minute - time.Duration(i)*time.Second), Valid: true},
		})
		requirements.NoError(err)
		requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		if i == 99 {
			targetID = id
		}
	}
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	inserted := false
	mutating := &insertAfterFirstVerificationBatchEngine{
		Engine: engine,
		afterFirstPage: func() error {
			id, err := st.UpsertMessage(&store.Message{
				SourceID: src.ID, ConversationID: conv, SourceMessageID: "newer-message",
				MessageType: "beeper", SentAt: sql.NullTime{Time: now.Add(-30 * time.Second), Valid: true},
			})
			if err != nil {
				return err
			}
			if err := st.UpsertMessageBody(id, sql.NullString{String: "ordinary concurrent insert", Valid: true}, sql.NullString{}); err != nil {
				return err
			}
			inserted = true
			return nil
		},
	}
	page, err := query.ScanVerifications(ctx, mutating, query.MessageFilter{SourceIDs: []int64{src.ID}}, verification.ListOptions{
		Since: now.Add(-time.Hour), Limit: 20,
	}, now, nil)
	requirements.NoError(err)
	assertions.True(inserted)
	requirements.Len(page.Data, 1)
	assertions.Equal(targetID, page.Data[0].MessageID)
	assertions.Equal(101, page.Scanned)
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
