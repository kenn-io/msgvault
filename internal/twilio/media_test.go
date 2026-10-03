package twilio

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testWAV = []byte("RIFF\x10\x00\x00\x00WAVEfmt \x00synthetic")

func TestRecordingDualFallbackAndBounds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	requests := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal("/2010-04-01/Accounts/"+testAC+"/Recordings/"+testRE+".wav", r.URL.Path)
		if r.URL.Query().Get("RequestedChannels") == "2" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, err := w.Write(testWAV)
		assert.NoError(err)
	}, nil)
	reader, err := c.OpenRecording(context.Background(), Recording{SID: testRE, AccountSID: testAC, Status: "completed", Channels: 2}, 100)
	require.NoError(err)
	defer func(reader io.ReadCloser) { require.NoError(reader.Close()) }(reader)
	data, err := io.ReadAll(reader)
	require.NoError(err)
	assert.Equal(testWAV, data)
	assert.Equal(2, requests)
	reader, err = c.OpenRecording(context.Background(), Recording{SID: testRE, Status: "completed"}, 12)
	if err == nil {
		defer func(reader io.ReadCloser) { require.NoError(reader.Close()) }(reader)
		_, err = io.ReadAll(reader)
	}
	require.Error(err)
}

func TestRecordingBodyCanOutlastAPIClientTimeout(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBody := func() { releaseOnce.Do(func() { close(release) }) }
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("ID3"))
		assertions.NoError(err)
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			close(started)
			return
		}
		flusher.Flush()
		close(started)
		<-release
		_, err = w.Write([]byte("synthetic audio body"))
		assertions.NoError(err)
	}, nil)
	t.Cleanup(releaseBody)
	apiTimeout := 40 * time.Millisecond
	c.http.Timeout = apiTimeout
	result := make(chan struct {
		reader io.ReadCloser
		err    error
	}, 1)
	go func() {
		reader, err := c.OpenRecording(context.Background(), Recording{SID: testRE, Status: "completed"}, 100)
		result <- struct {
			reader io.ReadCloser
			err    error
		}{reader, err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		requirements.FailNow("media request did not reach its response body")
	}
	select {
	case outcome := <-result:
		releaseBody()
		if outcome.reader != nil {
			requirements.NoError(outcome.reader.Close())
		}
		requirements.NoError(outcome.err, "media body should outlast the API request timeout")
	case <-time.After(4 * apiTimeout):
	}
	releaseBody()
	var outcome struct {
		reader io.ReadCloser
		err    error
	}
	select {
	case outcome = <-result:
	case <-time.After(time.Second):
		requirements.FailNow("media request did not finish after the body was released")
	}
	requirements.NoError(outcome.err)
	requirements.NotNil(outcome.reader)
	data, err := io.ReadAll(outcome.reader)
	requirements.NoError(err)
	requirements.NoError(outcome.reader.Close())
	assertions.Equal("ID3synthetic audio body", string(data))
}

func TestRecordingRejectsErrorBodyAndForeignRedirect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{{"JSON", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, err := w.Write([]byte(`{"error":"not audio"}`))
		assert.NoError(t, err)
	}}, {"redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/audio.wav", http.StatusFound)
	}}} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, tc.handler, nil)
			reader, err := c.OpenRecording(context.Background(), Recording{SID: testRE, Status: "completed"}, 100)
			if reader != nil {
				require.NoError(t, reader.Close())
			}
			require.Error(t, err)
		})
	}
}
func TestRecordingDecryptsAndAuthenticates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(err)
	path := filepath.Join(t.TempDir(), "recording.pem")
	require.NoError(os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600))
	cek := make([]byte, 32)
	_, err = rand.Read(cek)
	require.NoError(err)
	iv := make([]byte, 12)
	_, err = rand.Read(iv)
	require.NoError(err)
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &key.PublicKey, cek, nil)
	require.NoError(err)
	block, err := aes.NewCipher(cek)
	require.NoError(err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(err)
	encrypted := gcm.Seal(nil, iv, testWAV, nil)
	envelope := &EncryptionDetails{Type: "rsa-aes", PublicKeySID: "CRaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", EncryptedCEK: base64.StdEncoding.EncodeToString(wrapped), IV: base64.StdEncoding.EncodeToString(iv)}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { _, err := w.Write(encrypted); assert.NoError(err) }, func(options *Options) { options.RecordingKeys = map[string]string{envelope.PublicKeySID: path} })
	recording := Recording{SID: testRE, Status: "completed", EncryptionDetails: envelope}
	reader, err := c.OpenRecording(context.Background(), recording, 100)
	require.NoError(err)
	data, err := io.ReadAll(reader)
	require.NoError(err)
	require.NoError(reader.Close())
	assert.Equal(testWAV, data)
	encrypted[len(encrypted)-1] ^= 1
	reader, err = c.OpenRecording(context.Background(), recording, 100)
	require.Error(err)
	assert.Nil(reader)
	delete(c.options.RecordingKeys, envelope.PublicKeySID)
	reader, err = c.OpenRecording(context.Background(), recording, 100)
	require.Error(err)
	assert.Nil(reader)
}
func TestExternalMediaRequiresMappingAndDoesNotSendCredentials(t *testing.T) {
	require := require.New(t)

	external := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		_, err := w.Write(testWAV)
		assert.NoError(t, err)
	}))
	defer external.Close()
	u, err := url.Parse(external.URL)
	require.NoError(err)
	c, err := NewClient(Options{AccountSID: testAC, AuthToken: "test-secret", HTTPClient: external.Client(), ExternalMedia: map[string]string{testRE: external.URL + "/audio.wav"}, ExternalMediaHosts: []string{u.Hostname()}})
	require.NoError(err)
	reader, err := c.OpenRecording(context.Background(), Recording{SID: testRE, Status: "completed"}, 100)
	require.NoError(err)
	defer func(reader io.ReadCloser) { require.NoError(reader.Close()) }(reader)
	data, err := io.ReadAll(reader)
	require.NoError(err)
	assert.Equal(t, testWAV, data)
	_, err = NewClient(Options{AccountSID: testAC, AuthToken: "test-secret", ExternalMedia: map[string]string{testRE: external.URL}})
	require.Error(err)
}

func TestRecordingOfficialEncryptionEnvelope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var recording Recording
	require.NoError(json.Unmarshal([]byte(`{"sid":"`+testRE+`","account_sid":"`+testAC+`","call_sid":"`+testCA+`","status":"completed","custom_provider_evidence":{"kept":true},"encryption_details":{"encryption_public_key_sid":"CRaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","encryption_cek":"wrapped","encryption_iv":"nonce"}}`), &recording))
	require.NotNil(recording.EncryptionDetails)
	assert.Equal("rsa-aes", recording.EncryptionDetails.Type)
	assert.Equal("CRaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", recording.EncryptionDetails.PublicKeySID)
	assert.Equal("wrapped", recording.EncryptionDetails.EncryptedCEK)
	assert.Equal("nonce", recording.EncryptionDetails.IV)
	// Conflicting callback and REST envelopes must never pick a key by accident.
	err := json.Unmarshal([]byte(`{"type":"rsa-aes","public_key_sid":"CRaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","encryption_public_key_sid":"CRbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","encrypted_cek":"wrapped","iv":"nonce"}`), &EncryptionDetails{})
	require.Error(err)
}
func TestRecordingHTTPMetadataUsesPinnedHTTPSOrigin(t *testing.T) {
	require := require.New(t)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/2010-04-01/Accounts/"+testAC+"/Recordings/"+testRE+".wav", r.URL.Path)
		assert.NotEmpty(t, r.Header.Get("Authorization"))
		_, err := w.Write(testWAV)
		assert.NoError(t, err)
	}))
	defer server.Close()
	c, err := NewClient(Options{AccountSID: testAC, AuthToken: "test", HTTPClient: server.Client(), Endpoints: map[string]string{"voice": server.URL}})
	require.NoError(err)
	reader, err := c.OpenRecording(context.Background(), Recording{SID: testRE, Status: "completed", MediaURL: strings.Replace(server.URL, "https://", "http://", 1) + "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE}, 100)
	require.NoError(err)
	defer func(reader io.ReadCloser) { require.NoError(reader.Close()) }(reader)
	data, err := io.ReadAll(reader)
	require.NoError(err)
	assert.Equal(t, testWAV, data)
}

func TestTrustedMediaRedirectStripsTwilioCredentials(t *testing.T) {
	require := require.New(t)

	external := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		_, err := w.Write(testWAV)
		assert.NoError(t, err)
	}))
	defer external.Close()
	u, err := url.Parse(external.URL)
	require.NoError(err)
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NotEmpty(t, r.Header.Get("Authorization"))
		http.Redirect(w, r, external.URL+"/signed.wav?token=synthetic", http.StatusFound)
	}, func(options *Options) {
		options.HTTPClient = external.Client()
		options.ExternalMediaHosts = []string{u.Hostname()}
	})
	reader, err := c.OpenRecording(context.Background(), Recording{SID: testRE, Status: "completed"}, 100)
	require.NoError(err)
	defer func(reader io.ReadCloser) { require.NoError(reader.Close()) }(reader)
	data, err := io.ReadAll(reader)
	require.NoError(err)
	assert.Equal(t, testWAV, data)
}
