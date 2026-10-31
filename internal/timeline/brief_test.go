package timeline

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestBriefLimits(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	f := newTimelineFixture(t)
	for i := range 8 {
		f.PersistEmail(storetest.EmailSpec{Thread: fmt.Sprintf("owed%d", i), From: fmt.Sprintf("peer%d@example.com", i), To: []string{storetest.OwnerAddress}, Subject: "Question", Body: "Can you send the contract?", SentAt: f.now.Add(-48 * time.Hour)})
	}
	f.add(f.now.AddDate(0, 0, -3), false)
	f.add(f.now, false)
	for _, hour := range []int{23, 25} {
		at := f.now.Add(time.Duration(hour) * time.Hour)
		f.calendar(strconv.Itoa(hour), fmt.Sprintf(`{"id":%q,"summary":"Upcoming","start":{"dateTime":%q},"end":{"dateTime":%q}}`, strconv.Itoa(hour), at.Format(time.RFC3339), at.Add(time.Hour).Format(time.RFC3339)))
	}
	f.project()
	req := Request{Brief: true, Days: 10, Scope: Scope{Owner: true}, Format: "markdown"}
	got, err := Build(t.Context(), f.src, req)
	require.NoError(err)
	assert.LessOrEqual(len(Markdown(got)), 6144)
	require.NotNil(got.OpenLoops)
	assert.Len(got.OpenLoops.Items, 5)
	assert.True(got.OpenLoops.Truncated)
	require.Len(got.Upcoming, 1)
	assert.Less(got.Upcoming[0].Time.Sub(f.now), 24*time.Hour)
	assert.Equal("2026-10-21", got.Window.From)
	for _, day := range got.Days {
		assert.Nil(day.Summary)
	}
}
