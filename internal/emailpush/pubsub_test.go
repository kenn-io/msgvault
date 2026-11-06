package emailpush

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

const subscription = "projects/example/subscriptions/mail"
const topic = "projects/example/topics/mail"

// Rewrites only the remote address; exercises actual HTTP serialization/parsing.
type rewriteTransport struct{ base string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(rt.base, "http://")
	return http.DefaultTransport.RoundTrip(req)
}
func testPubSub(tb testing.TB, handler http.HandlerFunc) (*PubSub, func()) {
	tb.Helper()
	server := httptest.NewServer(handler)
	client, err := NewPubSub(&http.Client{Transport: rewriteTransport{server.URL}}, subscription, topic)
	require.NoError(tb, err)
	return client, func() { server.CloseClientConnections(); server.Close() }
}
func activeSubscription(tb testing.TB, w http.ResponseWriter, fields map[string]any) {
	tb.Helper()
	body := map[string]any{"name": subscription, "topic": topic, "state": "ACTIVE", "pushConfig": map[string]any{}}
	maps.Copy(body, fields)
	if !assert.NoError(tb, json.MarshalWrite(w, body)) {
		return
	}
}

func TestPubSubPullAndAcknowledge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	payload := encodedNotification(t, "synthetic@example.com", "42")
	var paths []string
	var pathsMu sync.Mutex
	ps, closeServer := testPubSub(t, func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		if r.Method == http.MethodGet {
			activeSubscription(t, w, nil)
			return
		}
		var body map[string]any
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		switch r.URL.Path {
		case "/v1/" + subscription + ":pull":
			assert.Equal(t, map[string]any{"maxMessages": float64(10)}, body)
			if !assert.NoError(t, json.MarshalWrite(w, map[string]any{"receivedMessages": []any{map[string]any{"ackId": "ack-synthetic", "message": map[string]string{"data": payload}}}})) {
				return
			}
		case "/v1/" + subscription + ":acknowledge":
			assert.Equal(t, map[string]any{"ackIds": []any{"ack-synthetic"}}, body)
			_, err := io.WriteString(w, `{}`)
			assert.NoError(t, err)
		default:
			assert.Fail(t, "unexpected route")
		}
	})
	defer closeServer()
	_, err := ps.Pull(context.Background())
	requirements.ErrorIs(err, ErrNotValidated)
	requirements.ErrorIs(ps.Acknowledge(context.Background(), []string{"ack-synthetic"}), ErrNotValidated)
	pathsMu.Lock()
	assertions.Empty(paths)
	pathsMu.Unlock()
	requirements.NoError(ps.Validate(context.Background()))
	deliveries, err := ps.Pull(context.Background())
	requirements.NoError(err)
	assertions.Equal([]Delivery{{AckID: "ack-synthetic", Data: payload}}, deliveries)
	requirements.NoError(ps.Acknowledge(context.Background(), []string{deliveries[0].AckID}))
	pathsMu.Lock()
	assertions.Equal([]string{"/v1/" + subscription, "/v1/" + subscription + ":pull", "/v1/" + subscription + ":acknowledge"}, paths)
	pathsMu.Unlock()
}

func TestPubSubRejectsUnsupportedSubscription(t *testing.T) {
	for field, value := range map[string]any{
		"name": "projects/example/subscriptions/other", "topic": "projects/example/topics/other",
		"state": "RESOURCE_ERROR", "enableExactlyOnceDelivery": true, "filter": "attributes.x = \"y\"",
		"pushConfig":     map[string]string{"pushEndpoint": "https://example.com/push"},
		"bigqueryConfig": map[string]any{}, "cloudStorageConfig": map[string]any{}, "bigtableConfig": map[string]any{},
		"deadLetterPolicy": map[string]any{}, "messageTransforms": []any{map[string]any{}}, "detached": true,
	} {
		t.Run(field, func(t *testing.T) {
			requirements := require.New(t)
			var requests atomic.Int32
			ps, closeServer := testPubSub(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				activeSubscription(t, w, map[string]any{field: value})
			})
			defer closeServer()
			err := ps.Validate(context.Background())
			var validation *ValidationError
			requirements.ErrorAs(err, &validation)
			_, err = ps.Pull(context.Background())
			requirements.ErrorIs(err, ErrNotValidated)
			requirements.ErrorIs(ps.Acknowledge(context.Background(), []string{"ack"}), ErrNotValidated)
			assert.Equal(t, int32(1), requests.Load())
		})
	}
}

func TestPubSubValidSubscriptionResourceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))
		sub := fmt.Sprintf("projects/example/subscriptions/mail-%d", n)
		top := fmt.Sprintf("projects/example/topics/mail-%d", n)
		retention := fmt.Sprintf("%ds", hegel.Draw(ht, hegel.Integers(600, 604800)))
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(ht, "/v1/"+sub, r.URL.Path)
			if !assert.NoError(ht, json.MarshalWrite(w, map[string]any{"name": sub, "topic": top, "state": "ACTIVE", "messageRetentionDuration": retention, "enableExactlyOnceDelivery": false, "pushConfig": map[string]any{}})) {
				return
			}
		}))
		defer server.Close()
		ps, err := NewPubSub(&http.Client{Transport: rewriteTransport{server.URL}}, sub, top)
		require.NoError(ht, err)
		require.NoError(ht, ps.Validate(context.Background()))
	})
}

func TestPubSubRejectsFilterProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text().MaxSize(200))
		ps, closeServer := testPubSub(ht, func(w http.ResponseWriter, r *http.Request) {
			activeSubscription(ht, w, map[string]any{"filter": "x" + text})
		})
		defer closeServer()
		var validation *ValidationError
		require.ErrorAs(ht, ps.Validate(context.Background()), &validation)
	})
}

func TestPubSubRevalidationFailureRevokesAdmission(t *testing.T) {
	var fail atomic.Bool
	ps, closeServer := testPubSub(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		activeSubscription(t, w, nil)
	})
	defer closeServer()
	require.NoError(t, ps.Validate(context.Background()))
	fail.Store(true)
	require.Error(t, ps.Validate(context.Background()))
	_, err := ps.Pull(context.Background())
	require.ErrorIs(t, err, ErrNotValidated)
}

func TestPubSubBoundedMalformedResponses(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{`, `{"name":"x","name":"y"}`, strings.Repeat("x", MaxResponseBytes+1)} {
		t.Run(fmt.Sprintf("length-%d", len(body)), func(t *testing.T) {
			ps, closeServer := testPubSub(t, func(w http.ResponseWriter, r *http.Request) {
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			})
			defer closeServer()
			require.Error(t, ps.Validate(context.Background()))
		})
	}
}

func TestPubSubPullBoundaries(t *testing.T) {
	for name, body := range map[string]string{
		"missing-ack":   `{"receivedMessages":[{"message":{"data":"e30="}}]}`,
		"null-delivery": `{"receivedMessages":[null]}`,
		"oversize-ack":  `{"receivedMessages":[{"ackId":"` + strings.Repeat("a", MaxAckIDBytes+1) + `"}]}`,
		"too-many":      `{"receivedMessages":[` + strings.TrimSuffix(strings.Repeat(`{"ackId":"ack"},`, 11), ",") + `]}`,
		"body":          strings.Repeat("x", MaxResponseBytes+1),
		"duplicate":     `{"receivedMessages":[],"receivedMessages":[]}`,
		"null":          `null`,
	} {
		t.Run(name, func(t *testing.T) {
			ps, closeServer := testPubSub(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					activeSubscription(t, w, nil)
					return
				}
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			})
			defer closeServer()
			require.NoError(t, ps.Validate(context.Background()))
			got, err := ps.Pull(context.Background())
			require.Error(t, err)
			assert.Nil(t, got)
		})
	}
}

func TestPubSubEmptyPullAndMalformedPayload(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var response atomic.Value
	response.Store(`{}`)
	ps, closeServer := testPubSub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			activeSubscription(t, w, nil)
			return
		}
		body, ok := response.Load().(string)
		if !assert.True(t, ok) {
			return
		}
		_, err := io.WriteString(w, body)
		assert.NoError(t, err)
	})
	defer closeServer()
	requirements.NoError(ps.Validate(context.Background()))
	got, err := ps.Pull(context.Background())
	requirements.NoError(err)
	assertions.Empty(got)
	// An ACKable poison notification must reach the lifecycle classifier unchanged.
	response.Store(`{"receivedMessages":[{"ackId":"ack","message":{"data":"!malformed"}}]}`)
	got, err = ps.Pull(context.Background())
	requirements.NoError(err)
	assertions.Equal([]Delivery{{AckID: "ack", Data: "!malformed"}}, got)
}

func TestPubSubAcknowledgeBounds(t *testing.T) {
	requirements := require.New(t)
	var requests atomic.Int32
	ps, closeServer := testPubSub(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); activeSubscription(t, w, nil) })
	defer closeServer()
	requirements.NoError(ps.Validate(context.Background()))
	for _, ids := range [][]string{{""}, {strings.Repeat("a", MaxAckIDBytes+1)}, make([]string, 11)} {
		requirements.Error(ps.Acknowledge(context.Background(), ids))
	}
	requirements.NoError(ps.Acknowledge(context.Background(), nil))
	assert.Equal(t, int32(1), requests.Load())
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestPubSubDeadlineCancellationAndRedaction(t *testing.T) {
	requirements := require.New(t)
	ps, err := NewPubSub(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		assert.LessOrEqual(t, time.Until(deadline), 30*time.Second)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}, subscription, topic)
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requirements.ErrorIs(ps.Validate(ctx), context.Canceled)
	ps, err = NewPubSub(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New("synthetic-secret-value") })}, subscription, topic)
	requirements.NoError(err)
	err = ps.Validate(context.Background())
	requirements.Error(err)
	assert.NotContains(t, err.Error(), "synthetic-secret-value")
}

func TestPubSubExplicitCredentialsOnly(t *testing.T) {
	requirements := require.New(t)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "must-not-read.json"))
	_, err := NewPubSubFromCredentials(context.Background(), "", subscription, topic)
	requirements.Error(err)
	path := filepath.Join(t.TempDir(), "subscriber.json")
	requirements.NoError(os.WriteFile(path, []byte(`{"type":"unexpected"}`), 0600))
	_, err = NewPubSubFromCredentials(context.Background(), path, subscription, topic)
	requirements.Error(err)
	requirements.NoError(os.WriteFile(path, []byte(strings.Repeat("x", MaxCredentialsBytes+1)), 0600))
	_, err = NewPubSubFromCredentials(context.Background(), path, subscription, topic)
	requirements.Error(err)
}

func TestPubSubConstructorRejectsResourceInjection(t *testing.T) {
	for _, bad := range []string{"", "mail", subscription + "?q=1", subscription + "/other", subscription + "%2fother", subscription + "#fragment"} {
		_, err := NewPubSub(http.DefaultClient, bad, topic)
		require.Error(t, err)
	}
	_, err := NewPubSub(nil, subscription, topic)
	require.Error(t, err)
	_, err = NewPubSub(http.DefaultClient, subscription, subscription)
	require.Error(t, err)
}

func TestPubSubExplicitAuthorizedUserCredentials(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "subscriber.json")
	requirements.NoError(os.WriteFile(path, []byte(`{"type":"authorized_user","client_id":"synthetic","client_secret":"synthetic","refresh_token":"synthetic"}`), 0600))
	// Parsing explicit credentials never contacts a provider or starts authentication.
	ps, err := NewPubSubFromCredentials(context.Background(), path, subscription, topic)
	requirements.NoError(err)
	requirements.NotNil(ps)
	_, err = ps.Pull(context.Background())
	requirements.ErrorIs(err, ErrNotValidated)
}

func TestPubSubHTTPFailureDoesNotExposeBody(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var status atomic.Int32
	status.Store(http.StatusOK)
	ps, closeServer := testPubSub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			activeSubscription(t, w, nil)
			return
		}
		w.WriteHeader(int(status.Load()))
		_, err := io.WriteString(w, `synthetic-private-body`)
		assert.NoError(t, err)
	})
	defer closeServer()
	requirements.NoError(ps.Validate(context.Background()))
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		status.Store(int32(code))
		_, err := ps.Pull(context.Background())
		requirements.Error(err)
		assertions.NotContains(err.Error(), "synthetic-private-body")
		err = ps.Acknowledge(context.Background(), []string{"ack"})
		requirements.Error(err)
		assertions.NotContains(err.Error(), "synthetic-private-body")
	}
}

func TestPubSubResponseReadStopsAtLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	reader := &countedBody{remaining: MaxResponseBytes * 10}
	ps, err := NewPubSub(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: reader, Header: make(http.Header)}, nil
	})}, subscription, topic)
	requirements.NoError(err)
	requirements.Error(ps.Validate(context.Background()))
	assertions.Equal(MaxResponseBytes+1, reader.read)
	assertions.True(reader.closed)
}

type countedBody struct {
	remaining, read int
	closed          bool
}

func (b *countedBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.remaining)
	clear(p[:n])
	b.remaining -= n
	b.read += n
	return n, nil
}
func (b *countedBody) Close() error { b.closed = true; return nil }

func TestPubSubRejectsInactiveStateProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		state := "INACTIVE_" + hegel.Draw(ht, hegel.Text().MaxSize(200))
		ps, closeServer := testPubSub(ht, func(w http.ResponseWriter, r *http.Request) {
			activeSubscription(ht, w, map[string]any{"state": state})
		})
		defer closeServer()
		var validation *ValidationError
		require.ErrorAs(ht, ps.Validate(context.Background()), &validation)
	})
}

func TestPubSubBoundedPullBatchProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(0, 10))
		deliveries := make([]Delivery, count)
		envelope := make([]any, count)
		for i := range count {
			id := fmt.Sprintf("ack-%d", i)
			data := encodedNotification(ht, "synthetic@example.com", strconv.Itoa(i+1))
			deliveries[i] = Delivery{AckID: id, Data: data}
			envelope[i] = map[string]any{"ackId": id, "message": map[string]string{"data": data}}
		}
		ps, closeServer := testPubSub(ht, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				activeSubscription(ht, w, nil)
				return
			}
			if !assert.NoError(ht, json.MarshalWrite(w, map[string]any{"receivedMessages": envelope})) {
				return
			}
		})
		defer closeServer()
		require.NoError(ht, ps.Validate(context.Background()))
		got, err := ps.Pull(context.Background())
		require.NoError(ht, err)
		assert.Equal(ht, deliveries, got)
	})
}

func TestPubSubAcknowledgeBatchProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(1, 10))
		seed := hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))
		ids := make([]string, count)
		for i := range count {
			ids[i] = fmt.Sprintf("opaque-%d-%d", seed, i)
		}
		var acknowledged atomic.Bool
		ps, closeServer := testPubSub(ht, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				activeSubscription(ht, w, nil)
				return
			}
			assert.Equal(ht, "/v1/"+subscription+":acknowledge", r.URL.Path)
			var body struct {
				AckIDs []string `json:"ackIds"`
			}
			if !assert.NoError(ht, json.UnmarshalRead(r.Body, &body)) {
				return
			}
			assert.Equal(ht, ids, body.AckIDs)
			acknowledged.Store(true)
			_, err := io.WriteString(w, `{}`)
			assert.NoError(ht, err)
		})
		defer closeServer()
		require.NoError(ht, ps.Validate(context.Background()))
		require.NoError(ht, ps.Acknowledge(context.Background(), ids))
		assert.True(ht, acknowledged.Load())
	})
}

func TestPubSubCanceledCalls(t *testing.T) {
	for _, method := range []string{"Validate", "Pull", "Acknowledge"} {
		t.Run(method, func(t *testing.T) {
			ps, closeServer := testPubSub(t, func(w http.ResponseWriter, r *http.Request) { activeSubscription(t, w, nil) })
			defer closeServer()
			require.NoError(t, ps.Validate(context.Background()))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var err error
			switch method {
			case "Validate":
				err = ps.Validate(ctx)
			case "Pull":
				_, err = ps.Pull(ctx)
			case "Acknowledge":
				err = ps.Acknowledge(ctx, []string{"ack"})
			}
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestPubSubConcurrentValidationCompletionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		failureLast := hegel.Draw(ht, hegel.Booleans())
		failureKind := hegel.Draw(ht, hegel.SampledFrom([]string{"http", "json", "topic", "name", "state", "exactly-once", "filter", "transforms", "dead-letter", "cancel"}))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		failureCtx, cancelFailure := context.WithCancel(ctx)
		defer cancelFailure()
		successEntered, failureEntered := make(chan struct{}), make(chan struct{})
		releaseSuccess, releaseFailure := make(chan struct{}), make(chan struct{})
		var validations atomic.Int32
		ps, closeServer := testPubSub(ht, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				_, err := io.WriteString(w, `{}`)
				assert.NoError(ht, err)
				return
			}
			switch validations.Add(1) {
			case 1:
				close(successEntered)
				select {
				case <-releaseSuccess:
					activeSubscription(ht, w, nil)
				case <-r.Context().Done():
				}
			case 2:
				close(failureEntered)
				select {
				case <-releaseFailure:
					switch failureKind {
					case "http":
						w.WriteHeader(http.StatusForbidden)
					case "json":
						_, writeErr := w.Write([]byte(`{`))
						assert.NoError(ht, writeErr)
					case "topic":
						activeSubscription(ht, w, map[string]any{"topic": "projects/example/topics/other"})
					case "name":
						activeSubscription(ht, w, map[string]any{"name": "projects/example/subscriptions/other"})
					case "state":
						activeSubscription(ht, w, map[string]any{"state": "RESOURCE_ERROR"})
					case "exactly-once":
						activeSubscription(ht, w, map[string]any{"enableExactlyOnceDelivery": true})
					case "filter":
						activeSubscription(ht, w, map[string]any{"filter": "attributes.x = x"})
					case "transforms":
						activeSubscription(ht, w, map[string]any{"messageTransforms": []any{map[string]any{}}})
					case "dead-letter":
						activeSubscription(ht, w, map[string]any{"deadLetterPolicy": map[string]any{}})
					}

				case <-r.Context().Done():
				}
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
		})
		defer closeServer()
		// Cancel before closing the server even if an assertion fails at a barrier.
		defer cancel()
		successResult, failureResult := make(chan error, 1), make(chan error, 1)
		go func() { successResult <- ps.Validate(ctx) }()
		select {
		case <-successEntered:
		case <-ctx.Done():
			require.NoError(ht, ctx.Err())
		}
		go func() { failureResult <- ps.Validate(failureCtx) }()
		select {
		case <-failureEntered:
		case <-ctx.Done():
			require.NoError(ht, ctx.Err())
		}
		if failureLast {
			close(releaseSuccess)
			require.NoError(ht, <-successResult)
			if failureKind == "cancel" {
				cancelFailure()
			} else {
				close(releaseFailure)
			}
			require.Error(ht, <-failureResult)
		} else {
			if failureKind == "cancel" {
				cancelFailure()
			} else {
				close(releaseFailure)
			}
			require.Error(ht, <-failureResult)
			close(releaseSuccess)
			require.NoError(ht, <-successResult)
		}
		_, pullErr := ps.Pull(ctx)
		ackErr := ps.Acknowledge(ctx, []string{"synthetic-ack"})
		if failureLast {
			require.ErrorIs(ht, pullErr, ErrNotValidated)
			require.ErrorIs(ht, ackErr, ErrNotValidated)
		} else {
			require.NoError(ht, pullErr)
			require.NoError(ht, ackErr)
		}
	})
}

func TestPubSubServiceAccountCredentials(t *testing.T) {
	requirements := require.New(t)
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	requirements.NoError(err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	requirements.NoError(err)
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	body, err := json.Marshal(map[string]string{"type": "service_account", "project_id": "example", "private_key_id": "synthetic", "private_key": string(privateKey), "client_email": "subscriber@example.com", "client_id": "123", "token_uri": "https://oauth2.googleapis.com/token"})
	requirements.NoError(err)
	path := filepath.Join(t.TempDir(), "subscriber.json")
	requirements.NoError(os.WriteFile(path, body, 0600))
	ps, err := NewPubSubFromCredentials(context.Background(), path, subscription, topic)
	requirements.NoError(err)
	requirements.NotNil(ps)
	_, err = ps.Pull(context.Background())
	requirements.ErrorIs(err, ErrNotValidated)
}

func TestPubSubUnsupportedCredentialTypeProperty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subscriber.json")
	hegel.Test(t, func(ht *hegel.T) {
		kind := "unsupported_" + hegel.Draw(ht, hegel.Text().MaxSize(60))
		body, err := json.Marshal(map[string]string{"type": kind, "private_key": "synthetic-private-marker"})
		require.NoError(ht, err)
		require.NoError(ht, os.WriteFile(path, body, 0600))
		ps, err := NewPubSubFromCredentials(context.Background(), path, subscription, topic)
		require.Error(ht, err)
		assert.Nil(ht, ps)
		assert.NotContains(ht, err.Error(), "synthetic-private-marker")
	})
}
