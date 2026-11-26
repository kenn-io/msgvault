package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
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
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/search/attachments/visual", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)

			assert.Equal(t, tc.status, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"error":"`+tc.code+`"`)
		})
	}
}

type imageQueryTestBackend struct{}

func (imageQueryTestBackend) PutUnpublished(context.Context, visual.VectorToken, []float32) error {
	return nil
}
func (imageQueryTestBackend) DeleteTokens(context.Context, []visual.VectorToken) error { return nil }
func (imageQueryTestBackend) Search(context.Context, visual.SearchRequest) ([]visual.Hit, error) {
	return nil, nil
}
func (imageQueryTestBackend) LoadOwnerVector(context.Context, visual.GenerationID, visual.Owner) ([]float32, error) {
	return nil, nil
}

func TestLargeVisualQueryUsesActiveProviderPolicy(t *testing.T) {
	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 4001, 4000))))
	provider, err := visual.NewKitProvider("kit", "https://embed.example.com/v1", "model", 2, "", nil, "generation")
	require.NoError(t, err)
	kit, err := visual.NewSearchService(&store.Store{}, provider, imageQueryTestBackend{}, true)
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		service *visual.SearchService
		status  int
		code    string
	}{
		{"kit", kit, http.StatusBadRequest, "invalid_limit"},
		{"voyage", &visual.SearchService{}, http.StatusUnsupportedMediaType, "unsupported_image_query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newTestServerWithMockStore(t)
			server.SetVisualSearch(tc.service)
			var upload bytes.Buffer
			form := multipart.NewWriter(&upload)
			file, err := form.CreateFormFile("image", "query.png")
			require.NoError(t, err)
			_, err = file.Write(imageBytes.Bytes())
			require.NoError(t, err)
			require.NoError(t, form.WriteField("limit", "invalid"))
			require.NoError(t, form.Close())
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/search/attachments/visual", &upload)
			request.Header.Set("Content-Type", form.FormDataContentType())
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), tc.code)
			body := fmt.Sprintf(`{"predicate":{},"visual_image_base64":%q,"limit":-1}`, base64.StdEncoding.EncodeToString(imageBytes.Bytes()))
			request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/files/search", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response = httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), tc.code)
		})
	}
}

func TestVertexQueryImageAxisHTTP(t *testing.T) {
	for _, providerName := range []string{"kit", "vertex"} {
		endpoint, model := "https://embed.example.com/v1", "model"
		if providerName == "vertex" {
			endpoint = "https://aiplatform.googleapis.com/v1/projects/project-a/locations/global/publishers/google/models"
			model = "gemini-embedding-2"
		}
		provider, err := visual.NewKitProvider(providerName, endpoint, model, 2, "", nil, "generation")
		require.NoError(t, err)
		service, err := visual.NewSearchService(&store.Store{}, provider, imageQueryTestBackend{}, true)
		require.NoError(t, err)
		for _, tc := range []struct{ width, height int }{{16384, 2}, {2, 16384}, {16385, 2}, {2, 16385}} {
			t.Run(fmt.Sprintf("%s/%dx%d", providerName, tc.width, tc.height), func(t *testing.T) {
				var img bytes.Buffer
				require.NoError(t, png.Encode(&img, image.NewRGBA(image.Rect(0, 0, tc.width, tc.height))))
				expectedStatus, expectedCode := http.StatusBadRequest, "invalid_limit"
				if providerName == "vertex" && (tc.width > 16384 || tc.height > 16384) {
					expectedStatus, expectedCode = http.StatusUnsupportedMediaType, "unsupported_image_query"
				}
				server, _ := newTestServerWithMockStore(t)
				server.SetVisualSearch(service)
				var upload bytes.Buffer
				form := multipart.NewWriter(&upload)
				file, err := form.CreateFormFile("image", "query.png")
				require.NoError(t, err)
				_, err = file.Write(img.Bytes())
				require.NoError(t, err)
				require.NoError(t, form.WriteField("limit", "invalid"))
				require.NoError(t, form.Close())
				request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/search/attachments/visual", &upload)
				request.Header.Set("Content-Type", form.FormDataContentType())
				response := httptest.NewRecorder()
				server.Router().ServeHTTP(response, request)
				assert.Equal(t, expectedStatus, response.Code, response.Body.String())
				assert.Contains(t, response.Body.String(), expectedCode)
				body := fmt.Sprintf(`{"predicate":{},"visual_image_base64":%q,"limit":-1}`, base64.StdEncoding.EncodeToString(img.Bytes()))
				request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/files/search", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response = httptest.NewRecorder()
				server.Router().ServeHTTP(response, request)
				assert.Equal(t, expectedStatus, response.Code, response.Body.String())
				assert.Contains(t, response.Body.String(), expectedCode)
			})
		}
	}
}

func TestFileSearchInvalidImageWithoutVisualService(t *testing.T) {
	server, _ := newTestServerWithMockStore(t)
	for _, tc := range []struct {
		name   string
		image  []byte
		status int
		code   string
	}{
		{"invalid", []byte("not an image"), http.StatusUnsupportedMediaType, "unsupported_image_query"},
		{"valid", validVisualQueryPNG(t), http.StatusServiceUnavailable, "visual_search_not_ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"predicate":{},"visual_image_base64":%q}`, base64.StdEncoding.EncodeToString(tc.image))
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/files/search", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), tc.code)
		})
	}
}
func validVisualQueryPNG(t *testing.T) []byte {
	t.Helper()
	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return imageBytes.Bytes()
}
