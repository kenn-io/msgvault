package timeline

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBuildInvalidUTF8Snippet(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL rejects invalid UTF-8 before a snippet can be stored")
	f := newTimelineFixture(t)
	f.add(f.now, false, func(m *store.Message) { m.Snippet = sql.NullString{String: "damaged \xff\xfe", Valid: true} })
	f.project()
	req := dayRequest()
	req.Format = "markdown"
	got, err := Build(t.Context(), f.src, req)
	require.NoError(t, err)
	require.Len(t, allDayEntries(got), 1)
	assert.Contains(t, Markdown(got), "damaged \ufffd\ufffd")
}
