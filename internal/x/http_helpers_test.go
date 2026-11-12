package x

import (
	"io"
	"net/http"
	"testing"
)

func xTestHTTPGet(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

func xTestHTTPPost(t *testing.T, client *http.Client, url, contentType string, body io.Reader) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", contentType)
	return client.Do(request)
}
