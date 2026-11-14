package docbankmedia

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/taskclient"
	"hegel.dev/go/hegel"
)

func checkPrivateNetworkEndpoint(t require.TestingT, endpoint string, trust, allowed bool) (*Client, *taskclient.Client) {
	client, err := NewClient(endpoint, nil, WithTrustPrivateNetwork(trust))
	if allowed {
		require.NoError(t, err, endpoint)
		require.NotNil(t, client)
	} else {
		require.Error(t, err, endpoint)
		require.Nil(t, client)
	}
	tasks, err := taskclient.New(taskclient.ClientOptions{Endpoint: endpoint, APIKey: "synthetic-key", TrustPrivateNetwork: trust})
	if allowed {
		require.NoError(t, err, endpoint)
		require.NotNil(t, tasks)
	} else {
		require.ErrorIs(t, err, taskclient.ErrInsecureEndpoint, endpoint)
		require.Nil(t, tasks)
	}
	return client, tasks
}

func TestPrivateNetworkMappedIPv4RepresentationProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(uint32(0), ^uint32(0)))
		ip := net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).String()
		trust := hegel.Draw(ht, hegel.Booleans())
		endpoints := []string{
			"http://" + ip,
			"http://[::ffff:" + ip + "]",
			fmt.Sprintf("http://[::FFFF:%X:%X]", uint16(n>>16), uint16(n)),
		}
		var wantDocbankAllowed, wantTaskAllowed bool
		var wantTaskKind taskclient.EndpointKind
		for i, endpoint := range endpoints {
			client, clientErr := NewClient(endpoint, nil, WithTrustPrivateNetwork(trust))
			tasks, taskErr := taskclient.New(taskclient.ClientOptions{Endpoint: endpoint, APIKey: "synthetic-key", TrustPrivateNetwork: trust})
			docbankAllowed, taskAllowed := clientErr == nil, taskErr == nil
			assert.Equal(ht, docbankAllowed, taskAllowed, endpoint)
			if i == 0 {
				wantDocbankAllowed, wantTaskAllowed = docbankAllowed, taskAllowed
				if tasks != nil {
					wantTaskKind = tasks.EndpointKind()
				}
			} else {
				assert.Equal(ht, wantDocbankAllowed, docbankAllowed, endpoint)
				assert.Equal(ht, wantTaskAllowed, taskAllowed, endpoint)
			}
			if client != nil {
				assert.Equal(ht, endpoint, client.baseURL)
			}
			if tasks != nil && wantTaskAllowed {
				assert.Equal(ht, wantTaskKind, tasks.EndpointKind(), endpoint)
			}
		}
	}, hegel.WithDatabase(""))
}

func TestPrivateNetworkMappedIPv4EndpointKinds(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint string
		trust          bool
		wantKind       taskclient.EndpointKind
	}{
		{"loopback", "http://[::FFFF:7F00:1]", false, taskclient.EndpointLoopbackHTTP},
		{"private", "http://[::ffff:10.2.3.4]", true, taskclient.EndpointPrivateHTTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, tasks := checkPrivateNetworkEndpoint(t, tc.endpoint, tc.trust, true)
			assert.Equal(t, tc.endpoint, client.baseURL)
			assert.Equal(t, tc.wantKind, tasks.EndpointKind())
		})
	}
}

func TestPrivateNetworkZonedLinkLocalProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(uint64(0), ^uint64(0)))
		trust := hegel.Draw(ht, hegel.Booleans())
		endpoint := fmt.Sprintf("http://[fe80::%x:%x:%x:%x%%25Net%d]:8485/base", uint16(n>>48), uint16(n>>32), uint16(n>>16), uint16(n), n)
		checkPrivateNetworkEndpoint(ht, endpoint, trust, trust)
		if trust {
			client, err := NewClient(endpoint, nil, WithTrustPrivateNetwork(true))
			require.NoError(ht, err)
			assert.Equal(ht, endpoint, client.baseURL)
		}
	}, hegel.WithDatabase(""))
}

func TestPrivateNetworkStrictZonedLoopbackRegression(t *testing.T) {
	requirements := require.New(t)
	// The opt-in must not change previously strict descriptor/default parsing.
	checkPrivateNetworkEndpoint(t, "http://[::1%25eth0]", false, false)
	checkPrivateNetworkEndpoint(t, "http://[::1%25eth0]", true, true)
	_, err := NewClient("http://[::1%25eth0]", nil)
	requirements.Error(err)
	requirements.Error(taskclient.ValidateEndpoint("http://[::1%25eth0]", false))
	client, err := taskclient.New(taskclient.ClientOptions{Endpoint: "http://[::1%25eth0]", APIKey: "synthetic-key", TrustPrivateNetwork: true})
	requirements.NoError(err)
	assert.Equal(t, taskclient.EndpointLoopbackHTTP, client.EndpointKind())
}

func TestPrivateNetworkEndpointRegressions(t *testing.T) {
	for _, tc := range []struct {
		host                           string
		defaultAllowed, trustedAllowed bool
	}{
		{"docbank:8485", false, true}, {"service-0.example.test:7777/api", false, true},
		{"LOCALHOST:8485", true, true}, {"127.0.0.2", true, true}, {"[::1]", true, true},
		{"192.168.2.3", false, true},
		{"[fd00::1]", false, true},
		{"169.254.1.2", false, true}, {"0.0.0.0", false, true}, {"[::]", false, true},
		{"[::ffff:10.2.3.4]", false, true}, {"[::ffff:8.8.8.8]", false, false},
		{"[fe80::1%25eth0]", false, true}, {"[2606:4700:4700::1111%25eth0]", false, false},
		{"224.0.0.1", false, false}, {"[ff02::1]", false, false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			checkPrivateNetworkEndpoint(t, "http://"+tc.host, false, tc.defaultAllowed)
			checkPrivateNetworkEndpoint(t, "http://"+tc.host, true, tc.trustedAllowed)
		})
	}
	credentialURL := (&url.URL{Scheme: "http", Host: "docbank", User: url.UserPassword("user", "pass")}).String()
	for _, endpoint := range []string{credentialURL, "http://docbank?key=value", "http://docbank#fragment", "ftp://docbank", "http://:8485"} {
		checkPrivateNetworkEndpoint(t, endpoint, true, false)
	}
	_, err := NewClient("http://docbank?", nil, WithTrustPrivateNetwork(true))
	require.Error(t, err)
	// Preserve taskclient's existing acceptance of an empty query marker.
	require.NoError(t, taskclient.ValidateEndpoint("http://localhost?", false))
	for _, endpoint := range []string{"https://EXAMPLE.test:443/base/../path/", "http://localhost:80/base/../path/"} {
		checkPrivateNetworkEndpoint(t, endpoint, false, true)
		checkPrivateNetworkEndpoint(t, endpoint, true, true)
	}
}

func TestPrivateNetworkTransportKeepsCredentialsAndRefusesRedirects(t *testing.T) {
	requirements := require.New(t)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		assert.Equal(t, "docbank:8485", r.Host)
		assert.Nil(t, r.URL.User)
		assert.Empty(t, r.URL.RawQuery)
		if strings.HasPrefix(r.URL.Path, "/api/v1/media/") {
			assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
			if strings.HasSuffix(r.URL.Path, "/redirect") {
				http.Redirect(w, r, "http://other.example.test/next", http.StatusFound)
				return
			}
			_, _ = fmt.Fprint(w, `{"vault_uid":"vault","source_id":"source","operation_state":"succeeded"}`)
			return
		}
		assert.Equal(t, "Bearer synthetic-key", r.Header.Get("Authorization"))
		http.Redirect(w, r, "http://other.example.test/next", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client, err := NewClient("http://docbank:8485", func() (string, error) { return "synthetic-key", nil }, WithTrustPrivateNetwork(true))
	requirements.NoError(err)
	client.http.Transport = transport
	_, err = client.Status(t.Context(), "source")
	requirements.NoError(err)
	_, err = client.Status(t.Context(), "redirect")
	requirements.Error(err)
	tasks, err := taskclient.New(taskclient.ClientOptions{Endpoint: "http://docbank:8485", APIKey: "synthetic-key", TrustPrivateNetwork: true, HTTPClient: &http.Client{Transport: transport}})
	requirements.NoError(err)
	_, err = tasks.Capabilities(t.Context())
	requirements.ErrorIs(err, taskclient.ErrRedirect)
	assert.Equal(t, int32(3), hits.Load())
}
