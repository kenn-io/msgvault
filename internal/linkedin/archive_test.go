package linkedin

import (
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadRowsEscapesAndHeaders(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"backslash", "\ufeffMessage,EXTRA,Date\n\"A \\\"quote\\\"\\nnext\\tcolumn\\\\path\",x,2026-10-02 12:00:00 UTC\n", "A \"quote\"\nnext\tcolumn\\path"},
		{"csv", "Message,Date\n\"A \"\"quote\"\"\nnext\",2026-10-02 12:00:00 UTC\n", "A \"quote\"\nnext"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := csvFile{kind: "comments.csv", open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(tc.input)), nil }}
			var got []row
			require.NoError(readRows(t.Context(), f, func(r row, _ int) error { got = append(got, r); return nil }))
			require.Len(got, 1)
			assert.Equal(tc.want, got[0]["message"])
		})
	}
}

func TestDateUTC(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, value := range []string{"2026-10-02 12:00:00 UTC", "2026-10-02 12:00:00", "2026-10-02T14:00:00+02:00"} {
		got, err := date(value)
		require.NoError(err)
		assert.Equal(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), got)
	}
	_, err := date("2026-10-02 12:00:00 PST")
	require.Error(err)
}

func TestReadRowsRejectsMalformed(t *testing.T) {
	require := require.New(t)
	for _, input := range []string{"Date,Date\n", "Date,Message\n2026-10-02,x,excess\n", "Date,Message\n\"unterminated"} {
		f := csvFile{kind: "comments.csv", open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(input)), nil }}
		require.Error(readRows(t.Context(), f, func(row, int) error { return nil }))
	}
}

func TestDiscoverSkipsCSVFileSymlinks(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	external := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(external, "messages.csv"), []byte("DATE,CONTENT\n"), 0600))
	if err := os.Symlink(filepath.Join(external, "messages.csv"), filepath.Join(root, "messages.csv")); err != nil {
		t.Skip("symlinks unavailable")
	}
	_, _, err := discover(root)
	require.ErrorContains(err, "no supported CSV")
}

func TestReadCommentsPrefersStandardCSV(t *testing.T) {
	for _, value := range []string{`Comma, trailing backslash \`, `Backslash before \"doubled quotes\"`, `Comma, literal C:\new\thing`} {
		t.Run(value, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			var data bytes.Buffer
			writer := csv.NewWriter(&data)
			require.NoError(writer.WriteAll([][]string{{"Date", "Link", "Message"}, {"2026-10-02 12:00:00 UTC", "https://example.com/post", value}}))
			file := csvFile{kind: "comments.csv", open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data.Bytes())), nil }}
			var rows []row
			require.NoError(readRows(t.Context(), file, func(r row, _ int) error { rows = append(rows, r); return nil }))
			require.Len(rows, 1)
			assert.Equal(value, rows[0]["message"])
		})
	}
}
