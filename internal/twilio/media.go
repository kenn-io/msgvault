package twilio

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
)

var ErrMediaSizeLimit = errors.New("twilio: recording exceeds byte limit")
var ErrNotAudio = errors.New("twilio: recording response is not audio")

// DiscoverCalls includes calls without recordings when Relay discovery is enabled.
func (c *Client) DiscoverCalls() bool { return c.options.RelayDiscovery }
func (c *Client) allowedExternal(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, host := range c.options.ExternalMediaHosts {
		if strings.EqualFold(u.Hostname(), host) {
			return true
		}
	}
	return false
}
func (c *Client) mediaClient(origin *url.URL, external bool) *http.Client {
	copyClient := *c.http
	// API requests keep their short whole-request timeout. Media reads are
	// bounded by the importer and request context, so long recordings can stream.
	copyClient.Timeout = 0
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("twilio: too many recording redirects")
		}
		if req.URL.User != nil || req.URL.Fragment != "" {
			return errors.New("twilio: invalid recording redirect")
		}
		if sameOrigin(origin, req.URL) {
			return nil
		}
		// Signed CDN redirects are GET-only and never receive account credentials.
		if req.URL.Scheme != "https" {
			return errors.New("twilio: insecure recording redirect")
		}
		trustedCDN := !external && (strings.EqualFold(req.URL.Hostname(), "media.twiliocdn.com") || strings.HasSuffix(strings.ToLower(req.URL.Hostname()), ".twiliocdn.com"))
		if !trustedCDN && !c.allowedExternal(req.URL) {
			return errors.New("twilio: recording redirect host is not allowed")
		}
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		return nil
	}
	return &copyClient
}

// OpenRecording returns bounded audio. Encrypted bytes are authenticated in full
// before any plaintext is returned. External retrieval requires explicit mapping.
func (c *Client) OpenRecording(ctx context.Context, recording Recording, maxBytes int64) (io.ReadCloser, error) {
	if !validSID(recording.SID, "RE") {
		return nil, errors.New("twilio: invalid recording SID")
	}
	if err := c.validateAccount(recording.AccountSID); err != nil {
		return nil, err
	}
	if !strings.EqualFold(recording.Status, "completed") {
		return nil, errors.New("twilio: recording is not completed")
	}
	if maxBytes <= 0 || maxBytes > math.MaxInt64-32 {
		return nil, errors.New("twilio: recording byte limit must be positive")
	}
	var key *rsa.PrivateKey
	var cek, iv []byte
	var err error
	if envelope := recording.EncryptionDetails; envelope != nil {
		if envelope.Type != "rsa-aes" {
			return nil, errors.New("twilio: unsupported recording encryption type")
		}
		key, err = c.recordingKey(envelope.PublicKeySID)
		if err != nil {
			return nil, err
		}
		wrapped, err := base64.StdEncoding.DecodeString(envelope.EncryptedCEK)
		if err != nil {
			return nil, errors.New("twilio: invalid encrypted recording key")
		}
		cek, err = rsa.DecryptOAEP(sha256.New(), rand.Reader, key, wrapped, nil)
		if err != nil || len(cek) != 32 {
			return nil, errors.New("twilio: could not decrypt recording key")
		}
		iv, err = base64.StdEncoding.DecodeString(envelope.IV)
		if err != nil || len(iv) != 12 {
			return nil, errors.New("twilio: invalid recording encryption IV")
		}
	}
	target := c.endpoint("voice", c.accountPath()+"/Recordings/"+recording.SID+".wav", nil)
	external := false
	if mapping := c.options.ExternalMedia[recording.SID]; mapping != "" {
		target, err = url.Parse(mapping)
		if err != nil || !c.allowedExternal(target) {
			return nil, errors.New("twilio: external recording URL is not allowed")
		}
		external = true
	} else if recording.MediaURL != "" {
		providerURL, err := url.Parse(recording.MediaURL)
		if err != nil {
			return nil, errors.New("twilio: invalid provider recording media URL")
		}
		expected := c.accountPath() + "/Recordings/" + recording.SID
		if providerURL.User != nil || providerURL.Fragment != "" {
			return nil, errors.New("twilio: invalid provider recording media URL")
		}
		providerHost := providerURL.Host == c.origins["voice"].Host
		providerPath := providerURL.Path == expected || providerURL.Path == expected+".wav" || providerURL.Path == expected+".mp3"
		if providerURL.IsAbs() && !providerHost {
			return nil, errors.New("twilio: external recording requires explicit media mapping")
		}
		if !providerPath || (providerURL.Scheme != "" && providerURL.Scheme != "http" && providerURL.Scheme != "https") {
			return nil, errors.New("twilio: provider recording media URL does not match its account and recording")
		}
		// media_url is metadata; reconstruct media retrieval on the pinned secure origin.
	}
	q := target.Query()
	if !external && recording.Channels == 2 {
		q.Set("RequestedChannels", "2")
		target.RawQuery = q.Encode()
	}
	client := c.mediaClient(target, external)
	response, err := c.get(ctx, client, "recording media", target, !external)
	if err != nil && !external && q.Get("RequestedChannels") == "2" {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest {
			q.Set("RequestedChannels", "1")
			target.RawQuery = q.Encode()
			response, err = c.get(ctx, client, "recording media", target, true)
		}
	}
	if err != nil {
		return nil, err
	}
	mediaLimit := maxBytes
	if key != nil {
		mediaLimit += 16
	}
	if response.ContentLength > mediaLimit {
		return nil, errors.Join(ErrMediaSizeLimit, response.Body.Close())
	}
	bounded := &mediaReader{ctx: ctx, reader: response.Body, closer: response.Body, remaining: mediaLimit}
	if key != nil {
		encrypted, err := io.ReadAll(bounded)
		closeErr := bounded.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		block, err := aes.NewCipher(cek)
		if err != nil {
			return nil, errors.New("twilio: invalid recording encryption key")
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, errors.New("twilio: invalid recording encryption cipher")
		}
		plaintext, err := gcm.Open(nil, iv, encrypted, nil)
		if err != nil {
			return nil, errors.New("twilio: recording authentication failed")
		}
		if int64(len(plaintext)) > maxBytes {
			return nil, ErrMediaSizeLimit
		}
		format, ok := detectAudioFormat(plaintext)
		if !ok {
			return nil, ErrNotAudio
		}
		return &recordingMedia{ReadCloser: io.NopCloser(bytes.NewReader(plaintext)), format: format}, nil
	}
	buffered := bufio.NewReader(bounded)
	prefix, err := buffered.Peek(12)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.Join(fmt.Errorf("twilio: inspect recording audio: %w", err), bounded.Close())
	}
	format, ok := detectAudioFormat(prefix)
	if !ok {
		return nil, errors.Join(ErrNotAudio, bounded.Close())
	}
	return &recordingMedia{ReadCloser: &recordingReader{Reader: buffered, Closer: bounded}, format: format}, nil
}
func (c *Client) recordingKey(id string) (key *rsa.PrivateKey, retErr error) {
	path := c.options.RecordingKeys[id]
	if path == "" {
		return nil, errors.New("twilio: no local private key configured for recording public key SID")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("twilio: could not open recording private key")
	}
	defer func() {
		if err := file.Close(); err != nil {
			key = nil
			retErr = errors.Join(retErr, errors.New("twilio: could not close recording private key"))
		}
	}()
	data, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return nil, errors.New("twilio: could not read recording private key")
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("twilio: recording private key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("twilio: invalid recording private key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("twilio: recording private key must be RSA")
	}
	return key, nil
}

type audioFormat struct {
	extension string
	mimeType  string
}

type recordingMedia struct {
	io.ReadCloser

	format audioFormat
}

func detectAudioFormat(data []byte) (audioFormat, bool) {
	switch {
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return audioFormat{extension: "wav", mimeType: "audio/wav"}, true
	case len(data) >= 3 && string(data[:3]) == "ID3", len(data) >= 2 && data[0] == 0xff && data[1]&0xe0 == 0xe0:
		return audioFormat{extension: "mp3", mimeType: "audio/mpeg"}, true
	case len(data) >= 4 && string(data[:4]) == "fLaC":
		return audioFormat{extension: "flac", mimeType: "audio/flac"}, true
	case len(data) >= 4 && string(data[:4]) == "OggS":
		return audioFormat{extension: "ogg", mimeType: "audio/ogg"}, true
	default:
		return audioFormat{}, false
	}
}

type recordingReader struct {
	io.Reader
	io.Closer
}
type mediaReader struct {
	ctx       context.Context
	reader    io.Reader
	closer    io.Closer
	remaining int64
	overflow  bool
}

func (r *mediaReader) Close() error { return r.closer.Close() }
func (r *mediaReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.overflow {
		return 0, ErrMediaSizeLimit
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n > 0 {
			r.overflow = true
			return 0, ErrMediaSizeLimit
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if n < 0 {
		return 0, errors.New("twilio: invalid recording read")
	}
	return n, err
}
