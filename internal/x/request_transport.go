package x

import (
	"crypto/tls"
	"net/http"
	"time"
)

// singleRequestHTTPClient prevents net/http from silently replaying an admitted
// request after losing a response on a reused connection. A fresh HTTP/1
// connection is required for each request; HTTP/2 has its own transparent retry
// paths. Clone native transports so TLS trust, proxy and dial policy survive
// without changing a caller's shared transport.
//
// Opaque custom RoundTrippers must independently honor the single-request
// contract. This helper cannot inspect or unwrap their retry behavior. Apply it
// to the underlying native transport before adding a bearer wrapper.
func singleRequestHTTPClient(base *http.Client) *http.Client {
	client := http.Client{}
	if base != nil {
		client = *base
	}
	if client.Timeout <= 0 {
		client.Timeout = 60 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if native, ok := transport.(*http.Transport); ok && native != nil {
		single := native.Clone()
		single.DisableKeepAlives = true
		single.ForceAttemptHTTP2 = false
		single.Protocols = new(http.Protocols)
		single.Protocols.SetHTTP1(true)
		// Clone may inherit an already configured HTTP/2 protocol handler. Disable
		// it and ALPN advertising as well as setting the explicit protocol policy.
		single.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		if single.TLSClientConfig != nil {
			single.TLSClientConfig.NextProtos = []string{"http/1.1"}
		}
		transport = single
	}
	client.Transport = transport
	return &client
}
