package linkedin

import (
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
			f := csvFile{kind: "comments.csv", open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(tc.input)), nil }}
			var got []row
			require.NoError(t, readRows(t.Context(), f, func(r row, _ int) error { got = append(got, r); return nil }))
			require.Len(t, got, 1)
			assert.Equal(t, tc.want, got[0]["message"])
		})
	}
}

func TestDateUTC(t *testing.T) {
	for _, value := range []string{"2026-10-02 12:00:00 UTC", "2026-10-02 12:00:00", "2026-10-02T14:00:00+02:00"} {
		got, err := date(value)
		require.NoError(t, err)
		assert.Equal(t, time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), got)
	}
	_, err := date("2026-10-02 12:00:00 PST")
	require.Error(t, err)
}

func TestReadRowsRejectsMalformed(t *testing.T) {
	for _, input := range []string{"Date,Date\n", "Date,Message\n2026-10-02,x,excess\n", "Date,Message\n\"unterminated"} {
		f := csvFile{kind: "comments.csv", open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(input)), nil }}
		require.Error(t, readRows(t.Context(), f, func(row, int) error { return nil }))
	}
}

func TestDiscoverSkipsDirectorySymlinks(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(external, "messages.csv"), []byte("DATE,CONTENT\n"), 0600))
	if err := os.Symlink(external, filepath.Join(root, "messages")); err != nil {
		t.Skip("symlinks unavailable")
	}
	_, _, err := discover(root)
	require.ErrorContains(t, err, "no supported CSV")
}
