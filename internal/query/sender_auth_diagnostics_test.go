package query_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderAuthDiagnostics(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	e := originalEngine(f)
	ctx := context.Background()
	pid := f.EnsureParticipant("sender@example.test", "Sender", "example.test")
	for range 3 {
		id := f.NewMessage().WithSubject("must not cross diagnostic wire").Create(t, f.Store)
		must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{pid}, nil))
		must.NoError(f.Store.UpsertMessageRaw(id, []byte("From: sender@example.test\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\nPRIVATE BODY")))
	}
	report, err := e.DiagnoseSenderAuth(ctx, f.Source.ID, 2, true)
	must.NoError(err)
	checks.Equal(2, report.Scanned)
	checks.True(report.HasMore)
	checks.Zero(report.Verified)
	must.Len(report.Observations, 1)
	checks.Equal("mx.test", report.Observations[0].AuthservID)
	checks.Equal(2, report.Observations[0].Count)
	checks.Equal("untrusted_text", report.Observations[0].ContentTrust)
	e.SetSenderAuthPins(map[int64]senderauth.TrustSet{f.Source.ID: {"mx.test"}})
	report, err = e.DiagnoseSenderAuth(ctx, f.Source.ID, 10, false)
	must.NoError(err)
	checks.Equal(3, report.Verified)
	checks.False(report.HasMore)
	checks.Empty(report.Observations)
	report, err = e.DiagnoseSenderAuth(sourcescope.WithSourceIDs(ctx, []int64{f.Source.ID + 100}), f.Source.ID, 2, true)
	must.NoError(err)
	checks.Zero(report.Scanned)
	checks.Empty(report.Observations)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = e.DiagnoseSenderAuth(cancelled, f.Source.ID, 2, true)
	must.Error(err)
	_, err = e.DiagnoseSenderAuth(ctx, f.Source.ID, 1001, false)
	must.Error(err)
}

func TestSenderAuthDiagnosticsChronologicalSample(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	e := originalEngine(f)
	e.SetSenderAuthPins(map[int64]senderauth.TrustSet{f.Source.ID: {"older.test", "newer.test", "undated.test"}})
	pid := f.EnsureParticipant("sender@example.test", "Sender", "example.test")
	for _, message := range []struct {
		at           time.Time
		host, result string
	}{
		{time.Date(2026, 1, 2, 2, 0, 0, 0, time.FixedZone("offset", 9*60*60)), "older.test", "fail"},
		{time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC), "newer.test", "pass"},
		{time.Time{}, "undated.test", "fail"},
	} {
		id := f.NewMessage().WithSentAt(message.at).Create(t, f.Store)
		if message.at.IsZero() {
			_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=NULL WHERE id=?`), id)
			must.NoError(err)
		}
		must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{pid}, nil))
		raw := fmt.Sprintf("From: sender@example.test\r\nAuthentication-Results: %s; dmarc=%s header.from=example.test\r\n\r\nBody", message.host, message.result)
		must.NoError(f.Store.UpsertMessageRaw(id, []byte(raw)))
	}
	report, err := e.DiagnoseSenderAuth(t.Context(), f.Source.ID, 1, true)
	must.NoError(err)
	checks.Equal(1, report.Scanned)
	checks.True(report.HasMore)
	checks.Equal(1, report.Verified)
	must.Len(report.Observations, 1)
	checks.Equal("newer.test", report.Observations[0].AuthservID)
}
