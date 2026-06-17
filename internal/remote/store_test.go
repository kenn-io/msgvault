package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

func TestNew_RejectsHTTPWithoutAllowInsecure(t *testing.T) {
	_, err := New(Config{
		URL:    "http://nas:8080",
		APIKey: "key",
	})
	requirepkg.Error(t, err, "New() should reject http:// without AllowInsecure")
}

func TestNew_AllowsHTTPWithAllowInsecure(t *testing.T) {
	s, err := New(Config{
		URL:           "http://nas:8080",
		APIKey:        "key",
		AllowInsecure: true,
	})
	requirepkg.NoError(t, err, "New()")
	requirepkg.NotNil(t, s, "New() returned nil store")
}

func TestNew_AllowsHTTPS(t *testing.T) {
	s, err := New(Config{
		URL:    "https://nas:8080",
		APIKey: "key",
	})
	requirepkg.NoError(t, err, "New()")
	requirepkg.NotNil(t, s, "New() returned nil store")
}

func TestNew_RejectsEmptyURL(t *testing.T) {
	_, err := New(Config{APIKey: "key"})
	requirepkg.Error(t, err, "New() should reject empty URL")
}

func TestNew_RejectsInvalidScheme(t *testing.T) {
	_, err := New(Config{
		URL:    "ftp://nas:8080",
		APIKey: "key",
	})
	requirepkg.Error(t, err, "New() should reject ftp:// scheme")
	assertpkg.ErrorContains(t, err, "http or https")
}

func TestNew_RejectsEmptyHost(t *testing.T) {
	_, err := New(Config{
		URL:           "http://",
		APIKey:        "key",
		AllowInsecure: true,
	})
	requirepkg.Error(t, err, "New() should reject URL with empty host")
	assertpkg.ErrorContains(t, err, "must include a host")
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	s, err := New(Config{
		URL:           "http://nas:8080/",
		APIKey:        "key",
		AllowInsecure: true,
	})
	requirepkg.NoError(t, err, "New()")
	assertpkg.Equal(t, "http://nas:8080", s.baseURL, "baseURL should have trailing slash trimmed")
}

func TestNew_DefaultTimeout(t *testing.T) {
	s, err := New(Config{
		URL:    "https://nas:8080",
		APIKey: "key",
	})
	requirepkg.NoError(t, err, "New()")
	assertpkg.NotZero(t, s.httpClient.Timeout, "httpClient.Timeout should have a default")
}

// newTestStore creates a Store pointing at the given httptest server.
func newTestStore(srv *httptest.Server, apiKey string) *Store {
	return &Store{
		baseURL:    srv.URL,
		apiKey:     apiKey,
		httpClient: srv.Client(),
	}
}

func TestDoRequest_SetsAuthHeader(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertpkg.Equal(t, "secret-key", r.Header.Get("X-Api-Key"), "X-API-Key")
		assertpkg.Equal(t, "application/json", r.Header.Get("Accept"), "Accept")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newTestStore(srv, "secret-key")
	resp, err := s.doRequest("/test")
	requirepkg.NoError(t, err, "doRequest error")
	_ = resp.Body.Close()
}

func TestDoRequest_OmitsAuthHeaderWhenEmpty(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertpkg.Empty(t, r.Header.Get("X-Api-Key"), "X-API-Key should be empty")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newTestStore(srv, "")
	resp, err := s.doRequest("/test")
	requirepkg.NoError(t, err, "doRequest error")
	_ = resp.Body.Close()
}

func TestHandleErrorResponse_JSONBody(t *testing.T) {
	body := `{"error":"not_found","message":"Message 42 not found"}`
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       http.NoBody,
	}
	// Use a real body
	resp.Body = readCloser(body)

	err := handleErrorResponse(resp)
	requirepkg.Error(t, err, "handleErrorResponse should return error")
	requirepkg.ErrorContains(t, err, "404", "error should contain status code")
	assertpkg.ErrorContains(t, err, "Message 42 not found", "error should contain API message")
}

func TestHandleErrorResponse_PlainTextBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       readCloser("internal server error"),
	}

	err := handleErrorResponse(resp)
	requirepkg.Error(t, err, "handleErrorResponse should return error")
	requirepkg.ErrorContains(t, err, "500", "error should contain status code")
	assertpkg.ErrorContains(t, err, "internal server error", "error should contain body text")
}

func TestGetStats_ErrorResponse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"db_error","message":"database locked"}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	_, err := s.GetStats()
	requirepkg.Error(t, err, "GetStats should return error on 500")
	assertpkg.ErrorContains(t, err, "database locked")
}

func TestGetStats_Success(t *testing.T) {
	assert := assertpkg.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(statsResponse{
			TotalMessages: 100,
			TotalThreads:  50,
			TotalAccounts: 2,
			TotalLabels:   10,
			TotalAttach:   5,
			DatabaseSize:  1024,
		})
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	stats, err := s.GetStats()
	requirepkg.NoError(t, err, "GetStats error")
	assert.Equal(int64(100), stats.MessageCount, "MessageCount")
	assert.Equal(int64(50), stats.ThreadCount, "ThreadCount")
	assert.Equal(int64(2), stats.SourceCount, "SourceCount")
}

func TestGetMessage_NotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	msg, err := s.GetMessage(999)
	requirepkg.ErrorIs(t, err, store.ErrMessageNotFound, "GetMessage(999) should report not found")
	assertpkg.Nil(t, msg, "GetMessage(999) should return nil for not found")
}

func TestGetMessage_Success(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/42", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messageDetailResponse{
			messageResponse: messageResponse{
				ID:      42,
				Subject: "Test Subject",
				From:    "sender@example.com",
				To:      []string{"receiver@example.com"},
				SentAt:  "2024-01-15T10:30:00Z",
			},
			Body: "Hello, world!",
			Attachments: []attachmentResponse{
				{Filename: "doc.pdf", MimeType: "application/pdf", Size: 1024},
			},
		})
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	msg, err := s.GetMessage(42)
	require.NoError(err, "GetMessage error")
	require.NotNil(msg, "GetMessage returned nil")
	assert.Equal("Test Subject", msg.Subject, "Subject")
	assert.Equal("Hello, world!", msg.Body, "Body")
	require.Len(msg.Attachments, 1, "len(Attachments)")
	assert.Equal("doc.pdf", msg.Attachments[0].Filename, "Attachments[0].Filename")
}

func TestListMessages_ZeroLimit(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps := r.URL.Query().Get("page_size")
		assertpkg.NotEqual(t, "0", ps, "page_size should not be 0")
		resp := listMessagesResponse{
			Total:    0,
			Page:     1,
			PageSize: 20,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	s := newTestStore(srv, "test")

	// This previously panicked with divide-by-zero
	msgs, total, err := s.ListMessages(0, 0)
	requirepkg.NoError(t, err, "ListMessages(0, 0) error")
	assertpkg.Equal(t, int64(0), total, "total")
	assertpkg.Empty(t, msgs, "len(msgs)")
}

func TestListMessages_NegativeLimit(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps := r.URL.Query().Get("page_size")
		assertpkg.Equal(t, "20", ps, "page_size should default to 20")
		resp := listMessagesResponse{Total: 0, Page: 1, PageSize: 20}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	s := newTestStore(srv, "test")

	_, _, err := s.ListMessages(0, -5)
	requirepkg.NoError(t, err, "ListMessages(0, -5) error")
}

func TestSearchMessages_ZeroLimit(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps := r.URL.Query().Get("page_size")
		assertpkg.NotEqual(t, "0", ps, "page_size should not be 0")
		resp := searchResponse{
			Query:    "test",
			Total:    0,
			Page:     1,
			PageSize: 20,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	s := newTestStore(srv, "test")

	// This previously panicked with divide-by-zero
	msgs, total, err := s.SearchMessages("test", 0, 0)
	requirepkg.NoError(t, err, "SearchMessages(test, 0, 0) error")
	assertpkg.Equal(t, int64(0), total, "total")
	assertpkg.Empty(t, msgs, "len(msgs)")
}

func TestSearchMessages_QueryEncoding(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		assertpkg.Equal(t, "hello world", q, "q")
		resp := searchResponse{Total: 0, Page: 1, PageSize: 20}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	s := newTestStore(srv, "test")
	_, _, err := s.SearchMessages("hello world", 0, 20)
	requirepkg.NoError(t, err, "SearchMessages error")
}

func TestListMessages_PageCalculation(t *testing.T) {
	tests := []struct {
		name     string
		offset   int
		limit    int
		wantPage string
		wantSize string
	}{
		{"first page", 0, 20, "1", "20"},
		{"second page", 20, 20, "2", "20"},
		{"third page", 40, 20, "3", "20"},
		{"small pages", 10, 10, "2", "10"},
		{"zero limit defaults", 0, 0, "1", "20"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				ps := r.URL.Query().Get("page_size")
				assertpkg.Equal(t, tt.wantPage, page, "page")
				assertpkg.Equal(t, tt.wantSize, ps, "page_size")
				resp := listMessagesResponse{Total: 0, Page: 1, PageSize: 20}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			s := newTestStore(srv, "test")

			_, _, err := s.ListMessages(tt.offset, tt.limit)
			requirepkg.NoError(t, err, "ListMessages(%d, %d) error", tt.offset, tt.limit)
		})
	}
}

func TestListAccounts_Success(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertpkg.Equal(t, "/api/v1/accounts", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(accountsResponse{
			Accounts: []AccountInfo{
				{Email: "user@gmail.com", Enabled: true, Schedule: "0 2 * * *"},
			},
		})
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	accounts, err := s.ListAccounts()
	requirepkg.NoError(t, err, "ListAccounts error")
	requirepkg.Len(t, accounts, 1, "len(accounts)")
	assertpkg.Equal(t, "user@gmail.com", accounts[0].Email, "Email")
}

// readCloser wraps a string in an io.ReadCloser. The embedded *strings.Reader
// promotes Read so io.EOF propagates verbatim to callers.
func readCloser(s string) *readCloserImpl {
	return &readCloserImpl{Reader: strings.NewReader(s)}
}

type readCloserImpl struct {
	*strings.Reader
}

func (rc *readCloserImpl) Close() error {
	return nil
}
