package timeline

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	for _, format := range []string{"json", "markdown"} {
		t.Run(format, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			req := dayRequest()
			req.Format = format
			got, err := Build(t.Context(), f.src, req)
			require.NoError(err)
			entries := allDayEntries(got)
			require.Len(entries, 1)
			if format == "json" {
				wire, err := json.Marshal(got, jsontext.AllowInvalidUTF8(true))
				require.NoError(err)
				assert.Contains(string(wire), "damaged \ufffd\ufffd")
			} else {
				assert.Contains(Markdown(got), "damaged \ufffd\ufffd")
			}
		})
	}
}
