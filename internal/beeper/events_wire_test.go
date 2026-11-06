package beeper

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // G505: RFC 6455 requires SHA-1 for this test-only upgrade handshake.
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Raw wire frames exercise aggregate limits independently of the client library's writer.
func writeEventWire(w io.Writer, opcode byte, final bool, payload []byte) error {
	first := opcode
	if final {
		first |= 0x80
	}
	header := []byte{first}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 65535:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 127)
		header = binary.BigEndian.AppendUint64(header, uint64(len(payload)))
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
func consumeEventWire(r *bufio.Reader) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return err
	}
	n := uint64(header[1] & 127)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > 4096 {
		return errors.New("unexpected large client command")
	}
	if header[1]&128 != 0 {
		var mask [4]byte
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return err
		}
	}
	_, err := io.CopyN(io.Discard, r, int64(n))
	return err
}
func testOversizedFragmentedEvent(t *testing.T) {
	t.Helper()
	assertions := assert.New(t)
	var wakes atomic.Int32
	received := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			_, _ = w.Write([]byte(`{"endpoints":{"ws_events":"/events"}}`))
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !assertions.True(ok, "HTTP test server must support hijacking") {
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			assertions.Fail("hijack test connection", "%v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // G401: RFC 6455 mandates SHA-1 for this test-only upgrade handshake.
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
		_ = rw.Flush()
		ready := []byte(`{"type":"ready","version":1}`)
		readyMidpoint := len(ready) / 2
		if err := writeEventWire(conn, 1, false, ready[:readyMidpoint]); err != nil {
			assertions.Fail("write first ready fragment", "%v", err)
			return
		}
		if err := writeEventWire(conn, 0, true, ready[readyMidpoint:]); err != nil {
			assertions.Fail("write final ready fragment", "%v", err)
			return
		}
		if err := consumeEventWire(rw.Reader); err != nil {
			assertions.Fail("read subscription command", "%v", err)
			return
		}
		if err := writeEventWire(conn, 1, true, []byte(`{"type":"subscriptions.updated","requestID":"msgvault","chatIDs":["*"]}`)); err != nil {
			assertions.Fail("write test acknowledgement", "%v", err)
			return
		}
		payload := `{"type":"message.upserted","seq":1,"chatID":"chat-example","ids":["one"],"entries":["` + strings.Repeat("x", 270<<10) + `"]}`
		midpoint := len(payload) / 2
		if err := writeEventWire(conn, 1, false, []byte(payload[:midpoint])); err != nil {
			assertions.Fail("write first oversized event fragment", "%v", err)
			return
		}
		if err := writeEventWire(conn, 0, true, []byte(payload[midpoint:])); err != nil {
			assertions.Fail("write final oversized event fragment", "%v", err)
			return
		}
		_, _ = io.Copy(io.Discard, rw.Reader)
	}))
	defer server.Close()
	c := NewClient(server.URL, testToken, 100)
	o := fastEventOptions()
	o.minRetry = time.Hour
	o.maxRetry = time.Hour
	cancel := startTestWatch(t, c, func() { wakes.Add(1) }, func(err error) {
		if err != nil {
			received <- err
		}
	}, o)
	select {
	case err := <-received:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "oversized fragmented message accepted")
	}
	assert.EqualValues(t, 2, wakes.Load(), "subscription and loss only")
	cancel()
}
func TestEventsFragmentedMessageLimit(t *testing.T) { testOversizedFragmentedEvent(t) }
