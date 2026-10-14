package query

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Freeze the existing analytical classifier on its real DuckDB runtime before
// extracting it for transactional suggestions; whitespace/parameters stay literal.
func TestFilenameMIMEFamilyParity(t *testing.T) {
	require := require.New(t)

	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	_, err = db.Exec(`SET threads=2`)
	require.NoError(err)
	for _, tc := range []struct {
		mime any
		want string
	}{
		{nil, "other"}, {"", "other"}, {"application/octet-stream", "other"},
		{"image/png", "image"}, {"IMAGE/JPEG", "image"}, {"image/svg+xml", "image"},
		{"application/pdf", "pdf"}, {"APPLICATION/PDF", "pdf"},
		{"audio/mpeg", "audio"}, {"AUDIO/WAV", "audio"},
		{"video/mp4", "video"}, {"VIDEO/WEBM", "video"},
		{"text/plain", "text"}, {"text/html", "text"}, {"text/rtf", "text"}, {"TEXT/PLAIN; charset=utf-8", "text"},
		{"application/msword", "document"}, {"application/rtf", "document"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "document"},
		{"application/vnd.oasis.opendocument.text", "document"},
		{"application/zip", "archive"}, {"application/gzip", "archive"}, {"application/x-tar", "archive"},
		{"application/x-7z-compressed", "archive"}, {"application/x-rar-compressed", "archive"},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "other"},
		{"application/vnd.ms-powerpoint", "other"},
		{"application/pdf; charset=utf-8", "other"}, {" application/pdf", "other"},
	} {
		var got string
		require.NoError(db.QueryRowContext(t.Context(), `SELECT `+sqlFileMIMEFamilyExpr()+` FROM (SELECT CAST(? AS VARCHAR) AS mime_type) AS a`, tc.mime).Scan(&got))
		assert.Equal(t, tc.want, got, "%v", tc.mime)
	}
}
