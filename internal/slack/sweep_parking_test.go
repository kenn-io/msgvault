package slack

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func parkedSweepTS(base time.Time, minutes int) string {
	instant := base.Add(-25 * time.Hour).Add(time.Duration(minutes) * time.Minute)
	return strconv.FormatInt(instant.Unix(), 10) + ".000100"
}

func parkedSweepFreshTS(base time.Time, seconds int) string {
	instant := base.Add(time.Duration(2+seconds) * time.Second)
	return strconv.FormatInt(instant.Unix(), 10) + ".000100"
}

func pinParkedSweepClock(imp *Importer, at time.Time) {
	imp.now = func() time.Time { return at }
}

func parkedSweepWorkspace(t *testing.T, base time.Time) (*fakeSlack, string) {
	t.Helper()
	f := newFakeSlack(t)
	f.users = []map[string]any{
		{"id": "UME", "name": "me", "profile": map[string]any{"email": "me@example.com"}},
	}
	rootTS := parkedSweepTS(base, -14400)
	f.convs = []*fakeConv{{
		ID: "C09", Name: "archive", Kind: "public", Members: []string{"UME"},
		Msgs: []fakeMsg{
			{TS: rootTS, User: "UME", Text: "ancient root",
				Replies: []fakeMsg{{TS: parkedSweepTS(base, -14390), ThreadTS: rootTS, User: "UME", Text: "ancient reply"}}},
			{TS: parkedSweepTS(base, 0), User: "UME", Text: "recent chatter"},
		},
	}}
	return f, rootTS
}

func addParkedSweepReply(f *fakeSlack, rootTS, replyTS string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{
		TS: replyTS, ThreadTS: rootTS, User: "UME", Text: "late reply",
	})
	return "C09:" + replyTS
}

func requireParkedSweepReply(t *testing.T, imp *Importer, sourceMessageID string) {
	t.Helper()
	require := require.New(t)
	var archived int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), sourceMessageID).Scan(&archived))
	require.Equal(1, archived, "repeated --limit 1 runs must archive the late reply")
}

func TestSweepConvergesFloorJustAfterMidnight(t *testing.T) {
	require := require.New(t)
	midnight := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	base := midnight.Add(3 * time.Minute)
	f, rootTS := parkedSweepWorkspace(t, base)
	imp, opts := testImporter(t, f)

	pinParkedSweepClock(imp, base)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	replyID := addParkedSweepReply(f, rootTS, parkedSweepFreshTS(base, 0))

	limited := opts
	limited.Limit = 1
	for _, offset := range []time.Duration{time.Minute, 30 * time.Minute, 2 * time.Hour} {
		pinParkedSweepClock(imp, base.Add(offset))
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	requireParkedSweepReply(t, imp, replyID)
}

func TestSweepConvergesFloorExactlyOnMidnight(t *testing.T) {
	require := require.New(t)
	midnight := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	base := midnight.Add(-time.Second)
	f, rootTS := parkedSweepWorkspace(t, base)
	imp, opts := testImporter(t, f)

	pinParkedSweepClock(imp, base)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	replyID := addParkedSweepReply(f, rootTS, parkedSweepFreshTS(base, 0))

	limited := opts
	limited.Limit = 1
	pinParkedSweepClock(imp, base.Add(time.Minute))
	_, err = imp.Import(context.Background(), limited)
	require.NoError(err)
	for _, offset := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
		pinParkedSweepClock(imp, midnight.Add(offset))
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	requireParkedSweepReply(t, imp, replyID)
}

func TestSweepLimitOneConvergesFromDayBehindBacklog(t *testing.T) {
	require := require.New(t)
	midnight := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	base := midnight.Add(-36 * time.Hour)
	f, rootTS := parkedSweepWorkspace(t, base)
	imp, opts := testImporter(t, f)

	pinParkedSweepClock(imp, base)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	replyID := addParkedSweepReply(f, rootTS, parkedSweepFreshTS(base, 25*60*60))

	limited := opts
	limited.Limit = 1
	runNow := base.Add(48 * time.Hour)
	for i := range 5 {
		pinParkedSweepClock(imp, runNow.Add(time.Duration(i)*time.Minute))
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	requireParkedSweepReply(t, imp, replyID)
}
