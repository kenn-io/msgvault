package carddav

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newFixtureClient(t *testing.T, rawOrigin, username, password string) *Client {
	t.Helper()
	origin, err := url.Parse(rawOrigin)
	require.NoError(t, err)
	client, err := NewClient(ClientOptions{
		CredentialOrigin:         origin,
		Username:                 username,
		Password:                 password,
		AllowInsecureCredentials: true,
	})
	require.NoError(t, err)
	client.allowPrivateOrigin = true
	return client
}

func TestClientRejectsCredentialsOverHTTPByDefault(t *testing.T) {
	origin, err := url.Parse("http://contacts.example/dav")
	require.NoError(t, err)

	_, err = NewClient(ClientOptions{
		CredentialOrigin: origin, Username: "alice", Password: "app-password",
	})
	require.ErrorIs(t, err, ErrUnsafeTarget)
}

func TestClientNeverSendsBasicAuthAcrossOrigin(t *testing.T) {
	var redirectedAuth string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusMultiStatus)
	}))
	t.Cleanup(destination.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	t.Cleanup(source.Close)

	client := newFixtureClient(t, source.URL, "alice", "app-password")
	_, err := client.Do(t.Context(), Request{Method: "PROPFIND", URL: source.URL})
	require.ErrorIs(t, err, ErrUnsafeRedirect)
	assert.Empty(t, redirectedAuth)
}

func TestClientSetsDAVPreconditionsAndTypedStatusErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Basic YWxpY2U6YXBwLXBhc3N3b3Jk", r.Header.Get("Authorization"))
		assert.Equal(t, "1", r.Header.Get("Depth"))
		assert.Equal(t, "\"prior\"", r.Header.Get("If-Match"))
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)

	depth := 1
	client := newFixtureClient(t, server.URL, "alice", "app-password")
	_, err := client.Do(t.Context(), Request{
		Method: "PROPFIND", URL: server.URL, Depth: &depth, ETag: "\"prior\"",
	})
	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusTooManyRequests, statusErr.StatusCode)
	assert.Equal(t, 90, int(statusErr.RetryAfter.Seconds()))
}

func TestClientValidateChildHrefRejectsOriginAndCollectionEscapes(t *testing.T) {
	require := require.New(t)

	base, err := url.Parse("https://contacts.example/dav/books/personal/")
	require.NoError(err)
	resolver, _ := newFixtureResolver(t, netip.MustParseAddr("203.0.113.9"))
	client, err := NewClient(ClientOptions{CredentialOrigin: base, Resolver: resolver})
	require.NoError(err)

	valid, err := client.ValidateChildHref(t.Context(), base, "alice.vcf")
	require.NoError(err)
	assert.Equal(t, "https://contacts.example/dav/books/personal/alice.vcf", valid.String())

	for _, href := range []string{
		"https://elsewhere.example/card.vcf",
		"/dav/books/other/card.vcf",
		"../card.vcf",
		"alice.vcf#one",
	} {
		_, err = client.ValidateChildHref(t.Context(), base, href)
		require.ErrorIsf(err, ErrUnsafeHref, "%q", href)
	}
}

func TestClientValidateChildHrefTreatsExplicitDefaultPortAsSameOrigin(t *testing.T) {
	require := require.New(t)

	base, err := url.Parse("https://contacts.example/dav/books/personal/")
	require.NoError(err)
	resolver, _ := newFixtureResolver(t, netip.MustParseAddr("203.0.113.9"))
	client, err := NewClient(ClientOptions{CredentialOrigin: base, Resolver: resolver})
	require.NoError(err)

	resolved, err := client.ValidateChildHref(t.Context(), base, "https://contacts.example:443/dav/books/personal/alice.vcf")
	require.NoError(err)
	assert.Equal(t, "https://contacts.example:443/dav/books/personal/alice.vcf", resolved.String())
}

func TestClientValidateChildHrefRejectsPrivateLiteralAndDNSDestination(t *testing.T) {
	t.Run("literal", func(t *testing.T) {
		origin, err := url.Parse("http://127.0.0.1/dav/books/personal/")
		require.NoError(t, err)
		client, err := NewClient(ClientOptions{CredentialOrigin: origin})
		require.NoError(t, err)

		_, err = client.ValidateChildHref(t.Context(), origin, "alice.vcf")
		require.ErrorIs(t, err, ErrUnsafeHref)
	})

	t.Run("DNS", func(t *testing.T) {
		origin, err := url.Parse("http://contacts.example/dav/books/personal/")
		require.NoError(t, err)
		resolver, _ := newFixtureResolver(t, netip.MustParseAddr("10.0.0.8"))
		client, err := NewClient(ClientOptions{CredentialOrigin: origin, Resolver: resolver})
		require.NoError(t, err)

		_, err = client.ValidateChildHref(t.Context(), origin, "alice.vcf")
		require.ErrorIs(t, err, ErrUnsafeHref)
	})
}

func TestClientPinsValidatedAddressAndRevalidatesSameOriginRedirect(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/after", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(err)
	resolver, queries := newFixtureResolver(t, netip.MustParseAddr("203.0.113.9"))
	var dialed []string
	dialer := net.Dialer{}
	origin, err := url.Parse("http://contacts.example:" + serverURL.Port())
	require.NoError(err)
	client, err := NewClient(ClientOptions{
		CredentialOrigin: origin,
		Resolver:         resolver,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return dialer.DialContext(ctx, network, serverURL.Host)
		},
	})
	require.NoError(err)

	response, err := client.Do(t.Context(), Request{Method: "PROPFIND", URL: origin.String() + "/redirect"})
	require.NoError(err)
	assert.Equal(http.StatusMultiStatus, response.StatusCode)
	assert.Equal([]string{"/redirect", "/after"}, paths)
	require.Len(dialed, 2)
	assert.Equal(net.JoinHostPort("203.0.113.9", serverURL.Port()), dialed[0])
	assert.Equal(net.JoinHostPort("203.0.113.9", serverURL.Port()), dialed[1])
	assert.GreaterOrEqual(queries.Load(), int32(2))
}

func TestClientTriesEveryValidatedAddressWithoutResolvingAgain(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(err)
	resolver, queries := newFixtureResolver(t,
		netip.MustParseAddr("203.0.113.8"),
		netip.MustParseAddr("203.0.113.9"),
	)
	origin, err := url.Parse("http://contacts.example:" + serverURL.Port())
	require.NoError(err)
	var dialed []string
	dialer := net.Dialer{}
	client, err := NewClient(ClientOptions{
		CredentialOrigin: origin,
		Resolver:         resolver,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			if len(dialed) == 1 {
				return nil, errors.New("fixture address is unreachable")
			}
			return dialer.DialContext(ctx, network, serverURL.Host)
		},
	})
	require.NoError(err)

	response, err := client.Do(t.Context(), Request{Method: "PROPFIND", URL: origin.String() + "/dav"})
	require.NoError(err)
	assert.Equal(http.StatusMultiStatus, response.StatusCode)
	assert.ElementsMatch([]string{
		net.JoinHostPort("203.0.113.8", serverURL.Port()),
		net.JoinHostPort("203.0.113.9", serverURL.Port()),
	}, dialed)
	assert.Positive(queries.Load())
	assert.LessOrEqual(queries.Load(), int32(2), "one LookupNetIP may issue one A and one AAAA query")
}

func TestClientNeverReplaysDAVMutationAcrossAmbiguousRedirect(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			t.Run(http.StatusText(status)+"/"+method, func(t *testing.T) {
				var startRequests, redirectedRequests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/start":
						startRequests.Add(1)
						w.Header().Set("Location", "/redirected")
						w.WriteHeader(status)
					case "/redirected":
						redirectedRequests.Add(1)
						w.WriteHeader(http.StatusNoContent)
					}
				}))
				t.Cleanup(server.Close)

				client := newFixtureClient(t, server.URL, "", "")
				_, err := client.Do(t.Context(), Request{Method: method, URL: server.URL + "/start", Body: []byte("synthetic-body")})
				require.ErrorIs(t, err, ErrUnsafeRedirect)
				assert.Equal(t, int32(1), startRequests.Load())
				assert.Equal(t, int32(0), redirectedRequests.Load())
			})
		}
	}
}

func TestClientPreservesDAVMutationOnlyAcrossSameOrigin307And308(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests []struct {
				path, method, body string
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				requests = append(requests, struct{ path, method, body string }{r.URL.Path, r.Method, string(body)})
				if r.URL.Path == "/start" {
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(server.Close)

			client := newFixtureClient(t, server.URL, "", "")
			response, err := client.Do(t.Context(), Request{Method: http.MethodPut, URL: server.URL + "/start", Body: []byte("synthetic-body")})
			require.NoError(t, err)
			assert.Equal(t, http.StatusNoContent, response.StatusCode)
			assert.Equal(t, []struct{ path, method, body string }{
				{path: "/start", method: http.MethodPut, body: "synthetic-body"},
				{path: "/redirected", method: http.MethodPut, body: "synthetic-body"},
			}, requests)
		})
	}
}

func TestClientEnforcesResponseAndOperationByteBudgets(t *testing.T) {
	t.Run("response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("abc"))
		}))
		t.Cleanup(server.Close)
		client := newFixtureClient(t, server.URL, "", "")
		client.responseBytes = 2
		client.operationBytes = 10

		response, err := client.Do(t.Context(), Request{Method: "PROPFIND", URL: server.URL})
		require.ErrorIs(t, err, ErrResponseLimit)
		require.NotNil(t, response)
		assert.Equal(t, []byte("abc"), response.Body)
	})

	t.Run("operation across redirect", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				w.Header().Set("Location", "/done")
				w.WriteHeader(http.StatusFound)
				_, _ = w.Write([]byte("12"))
				return
			}
			_, _ = w.Write([]byte("345"))
		}))
		t.Cleanup(server.Close)
		client := newFixtureClient(t, server.URL, "", "")
		client.responseBytes = 10
		client.operationBytes = 4

		_, err := client.Do(t.Context(), Request{Method: "PROPFIND", URL: server.URL + "/start"})
		require.ErrorIs(t, err, ErrOperationLimit)
	})
}

func TestClientHonorsRequestAndOperationTimeouts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusMultiStatus)
	}))
	t.Cleanup(server.Close)

	for name, configure := range map[string]func(*Client){
		"request":   func(client *Client) { client.requestTimeout = 5 * time.Millisecond },
		"operation": func(client *Client) { client.operationTimeout = 5 * time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			client := newFixtureClient(t, server.URL, "", "")
			configure(client)
			_, err := client.Do(t.Context(), Request{Method: "PROPFIND", URL: server.URL})
			require.Error(t, err)
		})
	}
}

func newFixtureResolver(t *testing.T, addresses ...netip.Addr) (*net.Resolver, *atomic.Int32) {
	t.Helper()
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	queries := new(atomic.Int32)
	go func() {
		buffer := make([]byte, 512)
		for {
			n, remote, readErr := listener.ReadFrom(buffer)
			if readErr != nil {
				return
			}
			_, _ = listener.WriteTo(fixtureDNSResponse(buffer[:n], addresses...), remote)
		}
	}()
	dialer := net.Dialer{}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			queries.Add(1)
			return dialer.DialContext(ctx, network, listener.LocalAddr().String())
		},
	}, queries
}

func fixtureDNSResponse(request []byte, addresses ...netip.Addr) []byte {
	questionEnd := 12
	for questionEnd < len(request) && request[questionEnd] != 0 {
		questionEnd += int(request[questionEnd]) + 1
	}
	questionEnd += 5
	queryType := uint16(0)
	if questionEnd >= 4 {
		queryType = uint16(request[questionEnd-4])<<8 | uint16(request[questionEnd-3])
	}
	answers := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if (queryType == 1 && address.Is4()) || (queryType == 28 && address.Is6()) {
			answers = append(answers, address)
		}
	}
	response := append([]byte{}, request[:2]...)
	response = append(response, 0x81, 0x80, 0x00, 0x01, byte(len(answers)>>8), byte(len(answers)), 0x00, 0x00, 0x00, 0x00)
	response = append(response, request[12:questionEnd]...)
	for _, address := range answers {
		response = append(response, 0xc0, 0x0c, byte(queryType>>8), byte(queryType), 0x00, 0x01, 0x00, 0x00, 0x00, 0x3c,
			0x00, byte(len(address.AsSlice())))
		response = append(response, address.AsSlice()...)
	}
	return response
}
