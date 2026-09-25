package api

import (
	"bytes"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestVisualSearchRequestMediaTypes(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	var upload bytes.Buffer
	form := multipart.NewWriter(&upload)
	file, err := form.CreateFormFile("image", "query.png")
	require.NoError(err)
	require.NoError(png.Encode(file, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	// An invalid filter proves the upload reached image decoding and form
	// parsing without requiring an embedding provider or a populated index.
	require.NoError(form.WriteField("limit", "invalid"))
	require.NoError(form.Close())

	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		status      int
		code        string
	}{
		{"multipart image", form.FormDataContentType(), upload.String(), http.StatusBadRequest, "invalid_limit"},
		{"JSON text", "application/json; charset=utf-8", `{"text":"a tree","source_id":-1}`, http.StatusBadRequest, "invalid_visual_filter"},
		{"undeclared type", "text/plain", `{"text":"a tree"}`, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"missing type", "", `{"text":"a tree"}`, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"malformed type", "multipart/form-data; boundary", upload.String(), http.StatusUnsupportedMediaType, "unsupported_media_type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newTestServerWithMockStore(t)
			server.SetVisualSearch(&visual.SearchService{})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/search/attachments/visual", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)

			assert.Equal(t, tc.status, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"error":"`+tc.code+`"`)
		})
	}
}
