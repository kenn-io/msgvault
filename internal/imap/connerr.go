package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

type connectStage uint8

const (
	stageDial connectStage = iota
	stageSetup
	stageGreeting
	stageAuth
)

var connectRetryDelays = [...]time.Duration{
	5 * time.Second,
	15 * time.Second,
	45 * time.Second,
}

type retryableConnectError struct{ err error }

func (e *retryableConnectError) Error() string { return e.err.Error() }

func (e *retryableConnectError) Unwrap() error { return e.err }

type progressSnapshot struct {
	bytesSinceRequestWrite int64
	readErr                error
	writeErr               error
	requestWritten         bool
	greetingPending        bool
	startTLSCommandSent    bool
	startTLSResponseBytes  int64
}

// progressConn keeps the transport errors associated with the request that
// caused a connection-stage operation to fail.
type progressConn struct {
	net.Conn
	mu                       sync.Mutex
	bytesSinceRequestWrite   int64
	readErr                  error
	writeErr                 error
	requestWritten           bool
	greetingSeen             bool
	greetingTrackingSkipped  bool
	greetingLine             []byte
	greetingBytesSinceWrite  int64
	startTLSCommandSent      bool
	startTLSResponseBytes    int64
	startTLSResponseLine     []byte
	startTLSResponseComplete bool
}

func (c *progressConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.bytesSinceRequestWrite += int64(n)
	if c.startTLSCommandSent && !c.startTLSResponseComplete {
		c.startTLSResponseBytes += int64(n)
		c.startTLSResponseLine = append(c.startTLSResponseLine, p[:n]...)
		c.completeSTARTTLSResponse()
	}
	if !c.greetingSeen && !c.greetingTrackingSkipped && n > 0 {
		if len(c.greetingLine) == 0 && p[0] != '*' && p[0] != '+' {
			c.greetingTrackingSkipped = true
		}
		if !c.greetingTrackingSkipped {
			previousGreetingBytes := len(c.greetingLine)
			c.greetingLine = append(c.greetingLine, p[:n]...)
			if end := bytes.Index(c.greetingLine, []byte("\r\n")); end >= 0 {
				line := bytes.TrimSpace(c.greetingLine[:end])
				if len(line) == 0 || line[0] == '*' || line[0] == '+' {
					greetingBytes := int64(end + 2 - previousGreetingBytes)
					if greetingBytes < 0 {
						greetingBytes = 0
					}
					c.greetingBytesSinceWrite += greetingBytes
					c.greetingSeen = true
					c.bytesSinceRequestWrite -= c.greetingBytesSinceWrite
					if c.bytesSinceRequestWrite < 0 {
						c.bytesSinceRequestWrite = 0
					}
					c.startTLSResponseBytes -= c.greetingBytesSinceWrite
					if c.startTLSResponseBytes < 0 {
						c.startTLSResponseBytes = 0
					}
					if c.startTLSCommandSent && !c.startTLSResponseComplete &&
						int64(len(c.startTLSResponseLine)) >= greetingBytes {
						c.startTLSResponseLine = c.startTLSResponseLine[greetingBytes:]
						c.completeSTARTTLSResponse()
					}
					c.greetingLine = nil
					c.greetingBytesSinceWrite = 0
				} else {
					c.greetingBytesSinceWrite += int64(n)
				}
			} else {
				c.greetingBytesSinceWrite += int64(n)
			}
		}
	}
	if err != nil && c.readErr == nil {
		c.readErr = err
	}
	c.mu.Unlock()
	return n, err
}

func (c *progressConn) completeSTARTTLSResponse() {
	if c.startTLSResponseComplete {
		return
	}
	end := bytes.Index(c.startTLSResponseLine, []byte("\r\n"))
	if end < 0 {
		return
	}
	line := bytes.Fields(c.startTLSResponseLine[:end])
	if len(line) >= 2 && line[0][0] != '*' && line[0][0] != '+' &&
		bytes.Equal(line[1], []byte("OK")) {
		c.bytesSinceRequestWrite = 0
		c.startTLSResponseBytes = 0
		c.startTLSResponseLine = nil
		c.startTLSResponseComplete = true
	}
}

func (c *progressConn) Write(p []byte) (int, error) {
	fields := bytes.Fields(bytes.ToUpper(p))
	startTLS := len(fields) > 1 && bytes.Equal(fields[1], []byte("STARTTLS"))
	c.mu.Lock()
	c.bytesSinceRequestWrite = 0
	c.readErr = nil
	c.writeErr = nil
	c.requestWritten = true
	c.greetingBytesSinceWrite = 0
	if startTLS {
		c.startTLSCommandSent = true
		c.startTLSResponseBytes = 0
		c.startTLSResponseLine = nil
		c.startTLSResponseComplete = false
	}
	c.mu.Unlock()

	n, err := c.Conn.Write(p)
	if err != nil {
		c.mu.Lock()
		if c.writeErr == nil {
			c.writeErr = err
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *progressConn) snapshot() progressSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return progressSnapshot{
		bytesSinceRequestWrite: c.bytesSinceRequestWrite,
		readErr:                c.readErr,
		writeErr:               c.writeErr,
		requestWritten:         c.requestWritten,
		greetingPending:        !c.greetingSeen && !c.greetingTrackingSkipped && len(c.greetingLine) > 0,
		startTLSCommandSent:    c.startTLSCommandSent,
		startTLSResponseBytes:  c.startTLSResponseBytes,
	}
}

func isTransientSocketError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	// Windows exposes WSA transport codes through syscall.Errno.
	return errno == syscall.Errno(109) ||
		errno == syscall.Errno(64) ||
		errno == syscall.Errno(10053) ||
		errno == syscall.Errno(10054)
}

func isTimeoutError(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func isCertificateError(err error) bool {
	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return true
	}

	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		return true
	}
	var invalidPtr *x509.CertificateInvalidError
	if errors.As(err, &invalidPtr) {
		return true
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return true
	}
	var hostnamePtr *x509.HostnameError
	if errors.As(err, &hostnamePtr) {
		return true
	}
	var authority x509.UnknownAuthorityError
	if errors.As(err, &authority) {
		return true
	}
	var authorityPtr *x509.UnknownAuthorityError
	if errors.As(err, &authorityPtr) {
		return true
	}
	var roots x509.SystemRootsError
	if errors.As(err, &roots) {
		return true
	}
	var rootsPtr *x509.SystemRootsError
	return errors.As(err, &rootsPtr)
}

func isConnectionRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == syscall.Errno(10061)
}

func matchesObservedTransport(err error, snapshot progressSnapshot) bool {
	for _, observed := range []error{snapshot.readErr, snapshot.writeErr} {
		if observed != nil && (errors.Is(err, observed) || errors.Is(observed, err) ||
			isTransientSocketError(err) && isTransientSocketError(observed) ||
			isTimeoutError(err) && isTimeoutError(observed) ||
			errors.Is(err, io.EOF) && errors.Is(observed, io.EOF)) {
			return true
		}
	}
	return false
}

func transportLost(stage connectStage, err error, probe *progressConn) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr *imaplib.Error
	if errors.As(err, &statusErr) || isCertificateError(err) || isConnectionRefused(err) {
		return false
	}

	if stage == stageDial {
		return isTransientSocketError(err) || isTimeoutError(err)
	}
	if probe == nil {
		return false
	}

	snapshot := probe.snapshot()
	if stage == stageSetup && snapshot.greetingPending {
		return false
	}
	if stage == stageSetup && snapshot.startTLSResponseBytes > 0 {
		return false
	}
	if snapshot.bytesSinceRequestWrite > 0 {
		return false
	}
	if stage == stageSetup && snapshot.requestWritten && snapshot.readErr == nil &&
		errors.Is(snapshot.writeErr, net.ErrClosed) && isTransientSocketError(err) {
		return true
	}
	if stage == stageGreeting && !snapshot.requestWritten &&
		snapshot.bytesSinceRequestWrite == 0 && errors.Is(snapshot.readErr, io.EOF) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		if err == io.ErrUnexpectedEOF && stage == stageSetup && snapshot.startTLSCommandSent &&
			snapshot.requestWritten && snapshot.bytesSinceRequestWrite == 0 &&
			snapshot.startTLSResponseBytes == 0 && snapshot.readErr == nil && snapshot.writeErr == nil {
			return true
		}
		if err == io.ErrUnexpectedEOF && stage == stageSetup && snapshot.requestWritten && snapshot.bytesSinceRequestWrite == 0 &&
			snapshot.readErr == nil && errors.Is(snapshot.writeErr, net.ErrClosed) {
			return true
		}
		return snapshot.requestWritten &&
			snapshot.bytesSinceRequestWrite == 0 &&
			(errors.Is(snapshot.readErr, io.EOF) || isTransientSocketError(snapshot.readErr))
	}
	if stage == stageSetup && snapshot.startTLSCommandSent && snapshot.requestWritten &&
		snapshot.bytesSinceRequestWrite == 0 && snapshot.startTLSResponseBytes == 0 &&
		!snapshot.greetingPending &&
		(isTransientSocketError(err) || isTimeoutError(err) || errors.Is(err, io.EOF)) {
		return true
	}
	if !isTransientSocketError(err) && !isTimeoutError(err) && !errors.Is(err, io.EOF) {
		return false
	}
	return matchesObservedTransport(err, snapshot)
}

func connectStageError(stage connectStage, addr string, err error) error {
	switch stage {
	case stageGreeting:
		return fmt.Errorf("IMAP greeting from %s: %w", addr, err)
	case stageAuth:
		return fmt.Errorf("IMAP authentication from %s: %w", addr, err)
	default:
		return fmt.Errorf("dial IMAP %s: %w", addr, err)
	}
}

func classifyConnect(stage connectStage, addr string, err error, probe *progressConn) error {
	wrapped := connectStageError(stage, addr, err)
	if transportLost(stage, err, probe) {
		return &retryableConnectError{err: wrapped}
	}
	return wrapped
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
