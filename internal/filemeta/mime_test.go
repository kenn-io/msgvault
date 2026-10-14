package filemeta_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/filemeta"
	"go.kenn.io/msgvault/internal/testutil"
)

// The extraction must preserve the current analytical classifier, including
// its untrimmed parameters, nullable input and office/archive exceptions.
func TestMIMEFamilySQL(t *testing.T) {
	s := testutil.NewTestStore(t)
	for _, tc := range []struct {
		mime any
		want string
	}{
		{nil, "other"}, {"", "other"}, {"application/octet-stream", "other"},
		{"image/png", "image"}, {"IMAGE/JPEG", "image"}, {"image/svg+xml", "image"},
		{"application/pdf", "pdf"}, {"APPLICATION/PDF", "pdf"},
		{"audio/mpeg", "audio"}, {"AUDIO/WAV", "audio"},
		{"video/mp4", "video"}, {"VIDEO/WEBM", "video"},
		{"text/plain", "text"}, {"text/html", "text"}, {"text/rtf", "text"},
		{"TEXT/PLAIN; charset=utf-8", "text"},
		{"application/msword", "document"}, {"application/rtf", "document"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "document"},
		{"application/vnd.oasis.opendocument.text", "document"},
		{"application/zip", "archive"}, {"application/gzip", "archive"},
		{"application/x-tar", "archive"}, {"application/x-7z-compressed", "archive"},
		{"application/x-rar-compressed", "archive"},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "other"},
		{"application/vnd.ms-powerpoint", "other"},
		{"application/pdf; charset=utf-8", "other"}, {" application/pdf", "other"},
	} {
		var family string
		query := `SELECT ` + filemeta.MIMEFamilySQL("value") + ` FROM (SELECT CAST(? AS TEXT) AS value) AS synthetic_mime`
		require.NoError(t, s.DB().QueryRow(s.Rebind(query), tc.mime).Scan(&family))
		assert.Equal(t, tc.want, family, "%v", tc.mime)
	}
}
