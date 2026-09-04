package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gmailapi "go.kenn.io/msgvault/internal/gmail"
)

// Option is a functional option for Client.
type Option func(*Client)

// WithLogger sets the logger.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Client) { c.logger = logger }
}

// WithTokenSource sets a callback that provides OAuth2 access tokens
// for XOAUTH2 SASL authentication. Required when Config.AuthMethod is AuthXOAuth2.
func WithTokenSource(fn func(ctx context.Context) (string, error)) Option {
	return func(c *Client) { c.tokenSource = fn }
}

// WithDateFilter restricts IMAP SEARCH to messages within the given date range.
func WithDateFilter(since, before time.Time) Option {
	return func(c *Client) {
		c.since = since
		c.before = before
	}
}

// WithListProgress sets a callback reporting mailbox-enumeration
// progress during the first ListMessages call of a session. See the
// listProgress field for the callback contract.
func WithListProgress(fn func(done, total int, mailbox string, found, unchanged int)) Option {
	return func(c *Client) { c.listProgress = fn }
}

// WithFolderFilter sets folder include/exclude lists for a single sync run.
// --folder/--skip-folder replaces or filters the config's folder list.
func WithFolderFilter(include, exclude []string) Option {
	return func(c *Client) {
		c.folderFilterInclude = include
		c.folderFilterExclude = exclude
	}
}

// fetchChunkSize is the maximum number of UIDs per UID FETCH command.
// Large FETCH sets cause server-side timeouts on big mailboxes; chunking
// keeps each round-trip short.
//
// listPageSize is the number of message IDs returned per ListMessages
// call. Each page ends with a checkpoint write and a progress update,
// and IMAP fetches are slow enough (single connection, chunked FETCH)
// that Gmail-sized 500-message pages left half-minute gaps between
// progress updates.
const (
	fetchChunkSize = 50
	listPageSize   = 100
)

var connectRetryDelays = [...]time.Duration{
	5 * time.Second,
	15 * time.Second,
	45 * time.Second,
}

// Trash is the canonical fallback name for the trash mailbox on servers
// that do not advertise \Trash via special-use attributes.
const Trash = "Trash"

// Client implements gmail.API for IMAP servers.
type Client struct {
	config      *Config
	password    string
	tokenSource func(ctx context.Context) (string, error) // XOAUTH2 token callback
	logger      *slog.Logger

	mu                    sync.Mutex
	conn                  *imapclient.Client
	selectedMailbox       string               // currently selected mailbox
	selectedUIDValidity   uint32               // UIDVALIDITY from the last SELECT
	selectedNumMessages   uint32               // EXISTS count from the last SELECT
	mailboxCache          []string             // cached list of selectable mailboxes
	messageListCache      []gmailapi.MessageID // full message ID list, built once per session
	trashMailbox          string               // cached trash mailbox name
	junkMailbox           string               // cached junk/spam mailbox name
	allMailFolder         string               // mailbox with \All attribute (empty if not detected)
	msgIDToLabels         map[string][]string  // RFC822 Message-ID → mailbox memberships
	seenRFC822IDs         map[string]bool      // dedup overlapping mailbox copies
	preferredRawSourceIDs map[[32]byte]string  // raw digest → canonical \All source ID
	sourceMessageAliases  map[string]string    // mailbox UID source ID → durable canonical source ID
	activeSourceAliases   map[string]string    // aliases validated by this session's QRESYNC SELECTs
	labelMapComplete      bool                 // latest listing collected every mailbox membership
	since                 time.Time            // IMAP SINCE date filter (zero = no filter)
	before                time.Time            // IMAP BEFORE date filter (zero = no filter)

	// aliasLoader resolves durable aliases for the mailbox UIDs a listing
	// actually touches. Nil when the caller keeps no durable state.
	aliasLoader     func(mailbox string, uids []uint32) (map[string]string, error)
	aliasLoadWarned bool // one failing load must not warn once per request

	// folderFilter overrides which mailboxes are included in the sync.
	// Zero-valued (empty include and exclude) means "all mailboxes".
	folderFilterInclude, folderFilterExclude []string

	forceFullEnumeration  bool
	priorFolderStates     map[string]FolderState // saved states from the last completed sync
	observedFolderStates  map[string]FolderState // states captured during this session's listing
	folderStateSave       func(string, FolderState)
	pendingFolderStates   map[string]FolderState
	pendingFolderCounts   map[string]int
	pendingMessageFolder  map[string]string
	completedFolders      map[string]bool
	observedMailboxDeltas []MailboxDelta
	observedMemberships   []MembershipObservation
	qresyncEnabled        bool
	qresyncCaptureMu      sync.Mutex
	qresyncCapture        *protocolDeltaCapture

	// listProgress, when set, is invoked during message-list
	// enumeration: once with done=0 after the mailbox list is known,
	// then after each mailbox is checked (the final call has
	// done == total). found is the running message-ID count and
	// unchanged the running count of mailboxes skipped via saved
	// folder state.
	listProgress func(done, total int, mailbox string, found, unchanged int)
	sleep        func(context.Context, time.Duration) error
}

// NewClient creates a new IMAP client.
func NewClient(cfg *Config, password string, opts ...Option) *Client {
	c := &Client{
		config:           cfg,
		password:         password,
		logger:           slog.Default(),
		labelMapComplete: true,
		sleep:            sleepContext,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// LabelsSnapshotComplete reports whether a sync sees every mailbox label.
// Folder- and date-filtered syncs only have a partial label view and must not
// replace labels or canonical message IDs discovered by an earlier full sync.
//
// A listing that skipped mailboxes it proved unchanged also holds a partial
// in-session label map, but it stays complete here: those mailboxes' deltas and
// memberships are published intact, and the post-sync mailbox-delta transaction
// is what reconciles labels. Syncer guarantees the pairing by forcing full
// enumeration on any client that reconciles labels immediately instead.
func (c *Client) LabelsSnapshotComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.labelMapComplete &&
		c.config != nil &&
		!c.labelsSnapshotFilteredLocked()
}

// LabelsSnapshotFiltered reports whether folder or date filters explicitly
// constrain the mailbox membership view.
func (c *Client) LabelsSnapshotFiltered() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.labelsSnapshotFilteredLocked()
}

func (c *Client) labelsSnapshotFilteredLocked() bool {
	return c.config != nil &&
		(!c.since.IsZero() ||
			!c.before.IsZero() ||
			len(c.config.Folders) > 0 ||
			len(c.folderFilterInclude) > 0 ||
			len(c.folderFilterExclude) > 0)
}

type retryableConnectError struct{ err error }

func (e *retryableConnectError) Error() string { return e.err.Error() }

func (e *retryableConnectError) Unwrap() error { return e.err }

type observedConn struct {
	net.Conn
	mu                    sync.Mutex
	readBytes             int64
	readErr               error
	startTLSCommandSent   bool
	startTLSResponse      []byte
	startTLSResponseReady bool
	startTLSResponseOK    bool
}

func (c *observedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.readBytes += int64(n)
	if !c.startTLSResponseReady && n > 0 {
		c.startTLSResponse = append(c.startTLSResponse, p[:n]...)
		if c.startTLSCommandSent {
			c.parseStartTLSResponseLocked()
		}
	}
	if err != nil && c.readErr == nil {
		c.readErr = err
	}
	c.mu.Unlock()
	return n, err
}

func (c *observedConn) parseStartTLSResponseLocked() {
	for {
		end := bytes.Index(c.startTLSResponse, []byte("\r\n"))
		if end < 0 {
			break
		}
		fields := strings.Fields(string(c.startTLSResponse[:end]))
		if len(fields) > 1 && fields[0] == "*" && strings.EqualFold(fields[1], "OK") {
			c.startTLSResponse = c.startTLSResponse[end+2:]
			continue
		}
		c.startTLSResponseReady = true
		c.startTLSResponseOK = len(fields) > 1 && strings.EqualFold(fields[1], "OK")
		break
	}
}

func (c *observedConn) Write(p []byte) (int, error) {
	if strings.Contains(strings.ToUpper(string(p)), "STARTTLS") {
		c.mu.Lock()
		c.startTLSCommandSent = true
		c.startTLSResponseReady = false
		c.startTLSResponseOK = false
		for {
			end := bytes.Index(c.startTLSResponse, []byte("\r\n"))
			if end < 0 {
				break
			}
			fields := strings.Fields(string(c.startTLSResponse[:end]))
			if len(fields) > 1 && fields[0] == "*" && strings.EqualFold(fields[1], "OK") {
				c.startTLSResponse = c.startTLSResponse[end+2:]
				continue
			}
			break
		}
		c.mu.Unlock()
	}
	return c.Conn.Write(p)
}

func (c *observedConn) hasReadBytes() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readBytes > 0
}

func (c *observedConn) resetReadObservation() {
	c.mu.Lock()
	c.readBytes = 0
	c.readErr = nil
	c.mu.Unlock()
}

func (c *observedConn) transientEmptyGreeting() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readBytes == 0 &&
		(errors.Is(c.readErr, io.EOF) || isTransientSocketError(c.readErr))
}

func (c *observedConn) transientStartTLSFailure(err error) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.startTLSCommandSent || !isTransientConnectError(err) {
		return false
	}
	return c.startTLSResponseOK || len(c.startTLSResponse) == 0
}

func isTransientConnectError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isTransientSocketError(err) {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func isTransientGreetingError(err error, observed *observedConn) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var protocolErr *imap.Error
	if errors.As(err, &protocolErr) || observed != nil && observed.hasReadBytes() {
		return false
	}
	if isTransientSocketError(err) {
		return true
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return true
	}
	return observed != nil && observed.transientEmptyGreeting()
}

func isTransientSocketError(err error) bool {
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	// Windows exposes WSA connection-reset codes through syscall.Errno.
	return errno == syscall.Errno(10053) || errno == syscall.Errno(10054) || errno == syscall.Errno(64)
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

// connect establishes and authenticates the IMAP connection. Dial and
// greeting failures are retried with bounded backoff. Caller must hold mu.
func (c *Client) connect(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	for attempt := 0; ; attempt++ {
		err := c.connectOnce(ctx)
		if err == nil {
			return nil
		}
		var retryErr *retryableConnectError
		if !errors.As(err, &retryErr) {
			return err
		}
		if attempt >= len(connectRetryDelays) {
			return retryErr.Unwrap()
		}
		delay := connectRetryDelays[attempt]
		c.logger.Warn("retrying IMAP connection",
			"addr", c.config.Addr(),
			"attempt", attempt+2,
			"limit", len(connectRetryDelays)+1,
			"delay", delay,
			"error", retryErr.Unwrap())
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func (c *Client) connectOnce(ctx context.Context) error {

	addr := c.config.Addr()
	c.logger.Debug("connecting to IMAP server", "addr", addr, "tls", c.config.TLS, "starttls", c.config.STARTTLS)

	imapOpts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Vanished: c.captureQresyncVanished,
		},
	}
	conn, err, retryable, observed := c.dialIMAP(ctx, addr, imapOpts)
	if err != nil {
		if !retryable {
			return fmt.Errorf("dial IMAP %s: %w", addr, err)
		}
		return &retryableConnectError{
			err: fmt.Errorf("dial IMAP %s: %w", addr, err),
		}
	}

	if err := waitGreeting(ctx, conn); err != nil {
		_ = conn.Close()
		var protocolErr *imap.Error
		if errors.As(err, &protocolErr) {
			return fmt.Errorf("IMAP greeting from %s: %w", addr, err)
		}
		if !isTransientGreetingError(err, observed) {
			return fmt.Errorf("IMAP greeting from %s: %w", addr, err)
		}
		return &retryableConnectError{
			err: fmt.Errorf("IMAP greeting from %s: %w", addr, err),
		}
	}

	switch c.config.EffectiveAuthMethod() {
	case AuthXOAuth2:
		if c.tokenSource == nil {
			_ = conn.Close()
			return errors.New("XOAUTH2 auth requires a token source (use WithTokenSource)")
		}
		token, err := c.tokenSource(ctx)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("get XOAUTH2 token: %w", err)
		}
		saslClient := NewXOAuth2Client(c.config.Username, token)
		if err := conn.Authenticate(saslClient); err != nil {
			_ = conn.Close()
			return fmt.Errorf("XOAUTH2 authenticate: %w", err)
		}
	default:
		if err := conn.Login(c.config.Username, c.password).Wait(); err != nil {
			_ = conn.Close()
			return fmt.Errorf("IMAP login: %w", err)
		}
	}

	c.conn = conn
	c.selectedMailbox = ""
	c.selectedUIDValidity = 0
	c.qresyncEnabled = false
	c.logger.Debug("connected and authenticated", "user", c.config.Username)
	return nil
}

func waitGreeting(ctx context.Context, conn *imapclient.Client) error {
	result := make(chan error, 1)
	go func() { result <- conn.WaitGreeting() }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = conn.Close()
		return ctx.Err()
	}
}

type startTLSResult struct {
	conn *imapclient.Client
	err  error
}

func newStartTLS(ctx context.Context, conn net.Conn, options *imapclient.Options) (*imapclient.Client, error) {
	result := make(chan startTLSResult, 1)
	go func() {
		client, err := imapclient.NewStartTLS(conn, options)
		if ctx.Err() != nil {
			if client != nil {
				_ = client.Close()
			}
			if err == nil {
				err = ctx.Err()
			}
		}
		result <- startTLSResult{conn: client, err: err}
	}()
	select {
	case result := <-result:
		return result.conn, result.err
	case <-ctx.Done():
		_ = conn.Close()
		return nil, ctx.Err()
	}
}

func (c *Client) dialIMAP(ctx context.Context, addr string, options *imapclient.Options) (*imapclient.Client, error, bool, *observedConn) {
	rawConn, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err, isTransientConnectError(err), nil
	}
	observed := &observedConn{Conn: rawConn}
	if c.config.TLS {
		tlsConfig := options.TLSConfig
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		} else {
			tlsConfig = tlsConfig.Clone()
		}
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = normalizeHost(c.config.Host)
		}
		if tlsConfig.NextProtos == nil {
			tlsConfig.NextProtos = []string{"imap"}
		}
		tlsConn := tls.Client(observed, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, err, isTransientConnectError(err), observed
		}
		observed.resetReadObservation()
		return imapclient.New(tlsConn, options), nil, false, observed
	}
	if c.config.STARTTLS {
		startTLSOptions := *options
		startTLSOptions.TLSConfig = &tls.Config{ServerName: normalizeHost(c.config.Host)}
		conn, err := newStartTLS(ctx, observed, &startTLSOptions)
		if err != nil {
			return nil, err,
				observed.transientStartTLSFailure(err) || isTransientGreetingError(err, observed), observed
		}
		return conn, nil, false, observed
	}
	return imapclient.New(observed, options), nil, false, observed
}

// reconnect closes the current connection and re-establishes it.
// Only connection-level state is cleared; per-sync caches
// (messageListCache, msgIDToLabels, seenRFC822IDs, mailbox
// metadata) are preserved so callers can continue operating
// after a transient disconnect.
// Caller must hold mu.
func (c *Client) reconnect(ctx context.Context) error {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.selectedMailbox = ""
	c.selectedUIDValidity = 0
	c.qresyncEnabled = false
	c.clearQresyncCapture()
	c.logger.Debug("reconnecting to IMAP server", "addr", c.config.Addr())
	return c.connect(ctx)
}

// withConn runs fn with the active connection, connecting if necessary.
// It holds the mutex for the duration of fn.
// If fn returns a network error the dead connection is cleared so the next
// call reconnects cleanly.
func (c *Client) withConn(ctx context.Context, fn func(*imapclient.Client) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connect(ctx); err != nil {
		return err
	}
	err := fn(c.conn)
	if err != nil && isNetworkError(err) {
		if c.conn != nil {
			_ = c.conn.Close()
		}
		c.conn = nil
		c.selectedMailbox = ""
		c.selectedUIDValidity = 0
		c.qresyncEnabled = false
		c.clearQresyncCapture()
	}
	return err
}

// selectMailbox selects a mailbox if not already selected. Caller must hold mu.
func (c *Client) selectMailbox(mailbox string) error {
	if c.selectedMailbox == mailbox {
		return nil
	}
	data, err := c.conn.Select(mailbox, nil).Wait()
	if err != nil {
		return fmt.Errorf("SELECT %q: %w", mailbox, err)
	}
	c.selectedMailbox = mailbox
	c.selectedUIDValidity = data.UIDValidity
	c.selectedNumMessages = data.NumMessages
	return nil
}

// listOptionsLocked requests RFC 6154 special-use attributes only when the
// server advertises support. Sending extended LIST options to legacy servers
// can make otherwise valid mailbox enumeration fail.
func (c *Client) listOptionsLocked() *imap.ListOptions {
	if !c.conn.Caps().Has(imap.CapSpecialUse) {
		return nil
	}
	return &imap.ListOptions{ReturnSpecialUse: true}
}

// listMailboxesLocked returns all selectable mailboxes, caching the result.
// Also detects special-use attributes (\Trash, \All) for later use.
// Caller must hold mu.
func (c *Client) listMailboxesLocked() ([]string, error) {
	if c.mailboxCache != nil {
		return c.mailboxCache, nil
	}

	items, err := c.conn.List("", "*", c.listOptionsLocked()).Collect()
	if err != nil {
		return nil, fmt.Errorf("LIST: %w", err)
	}

	var names []string
	for _, item := range items {
		if hasAttr(item.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		names = append(names, item.Mailbox)
		if c.trashMailbox == "" && hasAttr(item.Attrs, imap.MailboxAttrTrash) {
			c.trashMailbox = item.Mailbox
		}
		if c.allMailFolder == "" && hasAttr(item.Attrs, imap.MailboxAttrAll) {
			c.allMailFolder = item.Mailbox
		}
		if c.junkMailbox == "" && hasAttr(item.Attrs, imap.MailboxAttrJunk) {
			c.junkMailbox = item.Mailbox
		}
	}

	// Fallback: look for common junk/spam folder names
	if c.junkMailbox == "" {
		for _, candidate := range []string{
			"Spam", "[Gmail]/Spam",
			"Junk", "Junk Email", "Junk E-mail",
		} {
			for _, mb := range names {
				if strings.EqualFold(mb, candidate) {
					c.junkMailbox = mb
					break
				}
			}
			if c.junkMailbox != "" {
				break
			}
		}
	}

	// Fallback: look for common trash folder names
	if c.trashMailbox == "" {
		for _, candidate := range []string{Trash, "[Gmail]/Trash", "Deleted Items", "Deleted Messages"} {
			for _, mb := range names {
				if strings.EqualFold(mb, candidate) {
					c.trashMailbox = mb
					break
				}
			}
			if c.trashMailbox != "" {
				break
			}
		}
	}

	c.mailboxCache = names
	return names, nil
}

// clearMailboxDiscoveryLocked discards connection-derived mailbox and
// special-use metadata. Conservative fallback calls this after reconnect so
// the authoritative scan cannot reuse a mailbox topology from the failed
// QRESYNC connection.
func (c *Client) clearMailboxDiscoveryLocked() {
	c.mailboxCache = nil
	c.trashMailbox = ""
	c.junkMailbox = ""
	c.allMailFolder = ""
}

// enumerateMailboxSearchCriteria always constrains the search with an
// explicit UID range: some servers (e.g. iCloud) return sequence-number-like
// values for an unconstrained UID SEARCH, which later fail to fetch.
// Callers must not run the search against an empty mailbox, where the "*"
// in the range has no referent and some servers answer BAD.
func enumerateMailboxSearchCriteria(since, before time.Time, minUID imap.UID) *imap.SearchCriteria {
	if minUID == 0 {
		minUID = 1
	}
	var allUIDs imap.UIDSet
	allUIDs.AddRange(minUID, 0)

	criteria := &imap.SearchCriteria{
		UID: []imap.UIDSet{allUIDs},
	}
	if !since.IsZero() {
		criteria.Since = since
	}
	if !before.IsZero() {
		criteria.Before = before
	}
	return criteria
}

func messageIDHeaderFetchOptions() *imap.FetchOptions {
	return &imap.FetchOptions{
		UID:   true,
		Flags: true,
		BodySection: []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"Message-ID"},
			Peek:         true,
		}},
	}
}

func addMessageIDsFromHeaderFetchResults(dst map[string]bool, msgs []*imapclient.FetchMessageBuffer) {
	for _, msg := range msgs {
		if len(msg.BodySection) == 0 {
			continue
		}
		if msgID := rawMIMEMessageID(msg.BodySection[0].Bytes); msgID != "" {
			dst[msgID] = true
		}
	}
}

// mailboxScan is the resolved enumeration plan for one mailbox on a listing
// without QRESYNC. It is computed once, before the label map is built, so the
// label map and the message listing cannot disagree about what changed: a
// mailbox skipped by one and enumerated by the other would publish a Reset
// delta with no membership observations behind it.
type mailboxScan struct {
	skip     bool       // membership provably unchanged; publish a no-op delta
	reset    bool       // republish the whole mailbox
	uids     []imap.UID // UIDs to fetch and list
	vanished []imap.UID // UIDs that left the mailbox since the baseline
	known    []uint32   // resulting KnownUIDs baseline
	err      error      // enumeration failed; the run is not authoritative
}

// statusMessageCount reports whether the server's MESSAGES count is known and
// equals want.
func statusMessageCount(state FolderState, want int) bool {
	return state.NumMessages != nil && int(*state.NumMessages) == want
}

// planMailboxScans decides, per mailbox, whether a listing can skip the mailbox
// entirely, enumerate only UIDs at or above the saved UIDNEXT, or must
// enumerate it in full.
//
// STATUS MESSAGES stands in for the change tracking CONDSTORE would provide.
// UIDNEXT is monotonic within a UIDVALIDITY epoch and is raised by every
// APPEND, so an unchanged UIDNEXT proves no message arrived; an unchanged
// message count then proves none was expunged either. When UIDNEXT has
// advanced, the same count check applied to the merged UID set proves that
// nothing below the high water mark vanished while the new messages arrived.
//
// Every uncertainty resolves toward more enumeration, never less: membership
// rows are only ever written from UIDs the client actually observed, so a
// baseline can lag the server but cannot spuriously exceed it, and a lagging
// baseline fails the count check and triggers a full rebuild.
//
// Caller must hold c.mu.
func (c *Client) planMailboxScans(
	ctx context.Context, mailboxes []string, statuses map[string]FolderState,
) map[string]mailboxScan {
	scans := make(map[string]mailboxScan, len(mailboxes))
	for _, mailbox := range mailboxes {
		scans[mailbox] = c.planMailboxScan(ctx, mailbox, statuses)
	}
	return scans
}

func (c *Client) planMailboxScan(
	ctx context.Context, mailbox string, statuses map[string]FolderState,
) mailboxScan {
	full := func() mailboxScan {
		uids, err := c.enumerateMailbox(ctx, mailbox, 0)
		if err != nil {
			return mailboxScan{err: err}
		}
		return mailboxScan{reset: true, uids: uids, known: uidsToUint32(uids)}
	}

	status, ok := statuses[mailbox]
	if !ok || c.forceFullEnumeration {
		return full()
	}
	// A nil KnownUIDs means no baseline was ever recorded, which is not the
	// same as a baseline of zero messages: GetIMAPKnownUIDs returns an empty
	// non-nil slice for a mailbox that is genuinely empty.
	prior, ok := c.priorFolderStates[mailbox]
	if !ok || prior.KnownUIDs == nil ||
		prior.UIDValidity != status.UIDValidity ||
		prior.UIDNext > status.UIDNext {
		return full()
	}

	if folderStateUnchanged(prior, status) &&
		statusMessageCount(status, len(prior.KnownUIDs)) {
		return mailboxScan{skip: true, known: cloneKnownUIDs(prior.KnownUIDs)}
	}

	// A server that reports mod-sequences can change flags on an existing
	// message without moving UIDNEXT or the message count, and those messages
	// sit below the high water mark where the search below cannot see them.
	// Only servers that report no mod-sequence at all can be summarised by
	// UIDNEXT plus a count; a QRESYNC server never reaches here, because
	// CHANGEDSINCE handles it.
	if status.HighestModSeq != prior.HighestModSeq {
		return full()
	}

	uids, err := c.enumerateMailbox(ctx, mailbox, imap.UID(prior.UIDNext))
	if err != nil {
		return mailboxScan{err: err}
	}
	// Count the union rather than the sum: "UID SEARCH UID n:*" always returns
	// the last message in the mailbox even when n is past it (RFC 3501 6.4.8),
	// so a search above the high water mark can re-report a UID already in the
	// baseline.
	known := mergeKnownUIDs(prior.KnownUIDs, uids)
	if statusMessageCount(status, len(known)) {
		return mailboxScan{uids: uids, known: known}
	}
	// The counts disagree: something at or below the high water mark vanished.
	// Only a full enumeration can say what, but its answer is a diff, not a
	// reason to republish: prior.KnownUIDs is read back from the stored
	// memberships, so the difference between it and the current UID set names
	// the additions and the removals outright. Republishing instead would cost
	// a Message-ID fetch for every message in the mailbox, twice -- once to
	// rebuild the label map and once to refresh labels downstream.
	current, err := c.enumerateMailbox(ctx, mailbox, 0)
	if err != nil {
		return mailboxScan{err: err}
	}
	added, vanished := diffKnownUIDs(prior.KnownUIDs, current)
	return mailboxScan{
		uids:     added,
		vanished: vanished,
		known:    uidsToUint32(current),
	}
}

// enumerateMailbox lists UIDs in a single mailbox. A non-zero minUID
// restricts the search to UIDs at or above it (new messages since a
// saved UIDNEXT high water mark). It handles network errors with one
// reconnect attempt.
func (c *Client) enumerateMailbox(
	ctx context.Context, mailbox string, minUID imap.UID,
) ([]imap.UID, error) {
	if err := c.selectMailbox(mailbox); err != nil {
		if isNetworkError(err) {
			c.logger.Warn("network error selecting mailbox, reconnecting",
				"mailbox", mailbox, "error", err)
			if reconErr := c.reconnect(ctx); reconErr != nil {
				return nil, fmt.Errorf(
					"reconnect failed listing mailbox %q: %w",
					mailbox, reconErr)
			}
			if err := c.selectMailbox(mailbox); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// An empty mailbox has no UIDs to enumerate. Skipping the search also
	// avoids sending "UID SEARCH UID 1:*", which some servers reject when
	// the mailbox is empty ("*" has no referent).
	if c.selectedNumMessages == 0 {
		return nil, nil
	}

	criteria := enumerateMailboxSearchCriteria(c.since, c.before, minUID)
	searchData, err := c.conn.UIDSearch(
		criteria,
		nil,
	).Wait()
	if err != nil {
		if isNetworkError(err) {
			c.logger.Warn("network error during UID SEARCH, reconnecting",
				"mailbox", mailbox, "error", err)
			if reconErr := c.reconnect(ctx); reconErr != nil {
				return nil, fmt.Errorf(
					"reconnect failed searching mailbox %q: %w",
					mailbox, reconErr)
			}
			if selErr := c.selectMailbox(mailbox); selErr != nil {
				return nil, selErr
			}
			searchData, err = c.conn.UIDSearch(
				criteria,
				nil,
			).Wait()
			if err != nil {
				return nil, fmt.Errorf("UID SEARCH after reconnect in mailbox %q: %w", mailbox, err)
			}
		} else {
			return nil, fmt.Errorf("UID SEARCH in mailbox %q: %w", mailbox, err)
		}
	}

	uidSet, ok := searchData.All.(imap.UIDSet)
	if !ok {
		return nil, nil
	}
	uids, _ := uidSet.Nums()
	return uids, nil
}

// fetchMailboxMessageIDs fetches RFC822 Message-ID headers for all
// UIDs in the given mailbox. Returns the valid Message-IDs and the UIDs whose
// header had no usable Message-ID, so callers can fetch those copies raw.
// Caller must hold mu.
// fetchMailboxMessageIDs reads the Message-ID header of every supplied UID and
// returns the membership map for the mailbox, the UIDs whose header was empty,
// and the UIDs the server never returned at all.
//
// The last of those three is not the same as the second. A UID with an empty
// header is a live message this run could not identify. A UID left out of the
// response is a message the run learned nothing about, and reading that
// silence as absence removes the message from the published topology.
func (c *Client) fetchMailboxMessageIDs(
	ctx context.Context, mailbox string, uids []imap.UID,
) (map[string]bool, []imap.UID, []imap.UID, error) {
	if len(uids) == 0 {
		return nil, nil, nil, nil
	}

	if err := c.selectMailbox(mailbox); err != nil {
		if !isNetworkError(err) {
			return nil, nil, nil, err
		}
		c.logger.Warn("network error selecting mailbox for label map, reconnecting",
			"mailbox", mailbox, "error", err)
		if reconErr := c.reconnect(ctx); reconErr != nil {
			return nil, nil, nil, fmt.Errorf(
				"reconnect failed building label map for %q: %w",
				mailbox, reconErr)
		}
		if err := c.selectMailbox(mailbox); err != nil {
			return nil, nil, nil, err
		}
	}

	result := make(map[string]bool, len(uids))
	var unidentified []imap.UID
	var missing []imap.UID
	fetchOpts := messageIDHeaderFetchOptions()

	for chunkStart := 0; chunkStart < len(uids); chunkStart += fetchChunkSize {
		if ctx.Err() != nil {
			return result, unidentified, missing, ctx.Err()
		}

		end := min(chunkStart+fetchChunkSize, len(uids))
		chunk := uids[chunkStart:end]

		var uidSet imap.UIDSet
		for _, uid := range chunk {
			uidSet.AddNum(uid)
		}

		msgs, _, err := c.fetchChunk(ctx, mailbox, uidSet, fetchOpts)
		if err != nil {
			return result, unidentified, missing, fmt.Errorf(
				"message-ID fetch failed in %q: %w", mailbox, err)
		}
		seen := c.recordMessageIDResults(mailbox, result, &unidentified, msgs)

		omitted := uidsNotIn(chunk, seen)
		if len(omitted) == 0 {
			continue
		}
		// The server left these out of a response it answered successfully.
		// Ask once more before concluding anything: an omission is a fact
		// about the response, not about the mailbox.
		var recheckSet imap.UIDSet
		for _, uid := range omitted {
			recheckSet.AddNum(uid)
		}
		recheckMsgs, _, err := c.fetchChunk(ctx, mailbox, recheckSet, fetchOpts)
		if err != nil {
			return result, unidentified, missing, fmt.Errorf(
				"message-ID recheck failed in %q: %w", mailbox, err)
		}
		seenAgain := c.recordMessageIDResults(mailbox, result, &unidentified, recheckMsgs)
		withheld := uidsNotIn(omitted, seenAgain)
		present, err := c.confirmOmittedPresent(mailbox, withheld)
		if err != nil {
			// The search proved nothing, so the map cannot claim to describe
			// every message in the mailbox.
			missing = append(missing, withheld...)
			continue
		}
		// A UID the mailbox no longer reports left it, and deletion detection
		// retires its stored membership. Only a UID the mailbox still holds is
		// a hole in the map.
		for _, uid := range withheld {
			if present[uid] {
				missing = append(missing, uid)
			}
		}
	}
	if len(missing) > 0 {
		c.logger.Warn("label map is missing UIDs the server did not return",
			"mailbox", mailbox, "uids", len(missing))
	}
	return result, unidentified, missing, nil
}

// recordMessageIDResults records a membership for every message the server
// returned and reports which UIDs those were.
func (c *Client) recordMessageIDResults(
	mailbox string,
	result map[string]bool,
	unidentified *[]imap.UID,
	msgs []*imapclient.FetchMessageBuffer,
) map[imap.UID]bool {
	seen := make(map[imap.UID]bool, len(msgs))
	for _, msg := range msgs {
		seen[msg.UID] = true
		var rfc822MessageID string
		if len(msg.BodySection) > 0 {
			rfc822MessageID = rawMIMEMessageID(msg.BodySection[0].Bytes)
		}
		c.recordMembershipLocked(
			mailbox, msg.UID, "", rfc822MessageID, [32]byte{}, 0, msg.Flags)
		if rfc822MessageID == "" {
			*unidentified = append(*unidentified, msg.UID)
		} else {
			result[rfc822MessageID] = true
		}
	}
	return seen
}

// uidsNotIn returns the UIDs of want that seen does not contain.
func uidsNotIn(want []imap.UID, seen map[imap.UID]bool) []imap.UID {
	var absent []imap.UID
	for _, uid := range want {
		if !seen[uid] {
			absent = append(absent, uid)
		}
	}
	return absent
}

// buildLabelMap enumerates every mailbox except \All (whose memberships come
// from the other mailboxes) and fetches Message-ID headers to build a
// Message-ID → mailbox membership map. When \All is absent, every mailbox is
// included.
//
// A non-nil scans supplies the enumeration plan resolved by planMailboxScans;
// mailboxes it marks unchanged contribute no entries because their membership
// rows already carry the answer, and mailboxes it marks incremental contribute
// only their new UIDs. Skipping this way is a success, not a degradation: the
// returned completeness flag stays true, because it reports whether the
// published topology is authoritative, not how much of it was re-read.
// Caller must hold mu.
func (c *Client) buildLabelMap(
	ctx context.Context, allMailboxes []string, scans map[string]mailboxScan,
) (bool, []gmailapi.MessageID, error) {
	c.msgIDToLabels = make(map[string][]string)
	complete := true
	var unidentified []gmailapi.MessageID

	for _, mailbox := range allMailboxes {
		if ctx.Err() != nil {
			return false, unidentified, ctx.Err()
		}
		if mailbox == c.allMailFolder {
			continue
		}

		var uids []imap.UID
		var err error
		if scans == nil {
			uids, err = c.enumerateMailbox(ctx, mailbox, 0)
		} else {
			scan := scans[mailbox]
			if scan.skip {
				continue
			}
			uids, err = scan.uids, scan.err
		}
		if err != nil {
			complete = false
			c.logger.Warn("skipping mailbox for label map",
				"mailbox", mailbox, "error", err)
			continue
		}
		if len(uids) == 0 {
			continue
		}

		msgIDs, unidentifiedUIDs, missingUIDs, err := c.fetchMailboxMessageIDs(ctx, mailbox, uids)
		if err != nil {
			complete = false
			c.logger.Warn("failed to fetch envelopes for label map",
				"mailbox", mailbox, "error", err)
			continue
		}
		if len(missingUIDs) > 0 {
			// The map does not describe every message in this mailbox, so the
			// topology built from it is not authoritative. Saying so is what
			// stops a republish deleting the rows of the messages that are
			// absent from it, which for a label-only mailbox is the only
			// record of their membership.
			complete = false
		}
		for _, uid := range unidentifiedUIDs {
			unidentified = append(unidentified, gmailapi.MessageID{
				ID: compositeID(mailbox, uid),
			})
		}

		for msgID := range msgIDs {
			c.msgIDToLabels[msgID] = append(
				c.msgIDToLabels[msgID], mailbox)
		}
		c.logger.Debug("built label map for mailbox",
			"mailbox", mailbox, "messages", len(msgIDs))
	}
	return complete, unidentified, nil
}

// buildMessageListCache enumerates mailboxes and populates
// c.messageListCache. On Gmail (detected via [Gmail]/ prefix),
// only \All + Trash + Junk are enumerated since Gmail's All Mail
// is a superset minus Trash/Spam. On non-Gmail servers with \All,
// all selectable mailboxes are enumerated with RFC822 Message-ID
// dedup to handle overlaps. A label map is built from non-\All
// mailboxes so labels are preserved.
// Caller must hold mu and have an active connection.
func (c *Client) buildMessageListCache(ctx context.Context) error {
	// Stay conservative until both membership collection and message
	// enumeration finish. Any recoverable mailbox failure makes labels from
	// this listing additive rather than authoritative.
	c.labelMapComplete = false
	c.seenRFC822IDs = nil
	c.preferredRawSourceIDs = nil
	c.activeSourceAliases = nil
	c.observedMailboxDeltas = nil
	c.observedMemberships = nil

	var allMailboxes, listMailboxes []string
	var isGmailAllMail bool
	buildMailboxPlan := func() error {
		mailboxes, listErr := c.listMailboxesLocked()
		if listErr != nil {
			return listErr
		}

		allMailboxes = filterMailboxes(
			mailboxes,
			c.effectiveFolderIncludeLocked(),
			c.folderFilterExclude,
		)
		listMailboxes = allMailboxes
		isGmailAllMail = false
		if c.allMailFolder != "" {
			isGmailAllMail = strings.HasPrefix(c.allMailFolder, "[Gmail]/")
			if isGmailAllMail && slices.Contains(allMailboxes, c.allMailFolder) {
				// Gmail's All Mail contains every message except Trash
				// and Spam. Enumerate those alongside All Mail to catch
				// messages only in those folders, but only when each
				// canonical mailbox remains in the effective folder filter.
				listMailboxes = []string{c.allMailFolder}
				if slices.Contains(allMailboxes, c.trashMailbox) {
					listMailboxes = append(listMailboxes, c.trashMailbox)
				}
				if slices.Contains(allMailboxes, c.junkMailbox) {
					listMailboxes = append(listMailboxes, c.junkMailbox)
				}
			}
		}
		c.preferredRawSourceIDs = nil
		if c.allMailFolder != "" {
			c.preferredRawSourceIDs = make(map[[32]byte]string)
		}
		return nil
	}

	err := buildMailboxPlan()
	if err != nil {
		if isNetworkError(err) {
			if reconErr := c.reconnect(ctx); reconErr != nil {
				return fmt.Errorf("reconnect after LIST error: %w", reconErr)
			}
			c.clearMailboxDiscoveryLocked()
			err = buildMailboxPlan()
		}
		if err != nil {
			return err
		}
	}
	// Folder-state tracking skips unchanged mailboxes via STATUS
	// UIDVALIDITY/UIDNEXT. Disabled under a date filter because a
	// filtered run does not fetch everything up to UIDNEXT, so the
	// high water mark would be wrong. When an \All mailbox exists, the label
	// map still needs full enumeration if anything changed, but a fully
	// unchanged resync can return immediately.
	trackFolders := c.since.IsZero() && c.before.IsZero()
	var folderStatuses map[string]FolderState
	if trackFolders {
		c.observedFolderStates = make(map[string]FolderState, len(allMailboxes))
		folderStatuses = c.observeFolderStates(ctx, allMailboxes)
	} else {
		c.clearFolderAcknowledgements()
	}

	qresyncFallback := false
	if trackFolders {
		requireQresync := !c.forceFullEnumeration &&
			!c.labelsSnapshotFilteredLocked() &&
			len(c.priorFolderStates) > 0
		handled, deltaErr := c.tryBuildQresyncMessageList(ctx, allMailboxes, folderStatuses)
		switch {
		case deltaErr != nil:
			// A failed attempt has already issued ENABLE and CONDSTORE SELECTs
			// and left partial deltas behind, so the connection and every
			// observation derived from it are discarded before enumerating.
			c.logger.Warn("QRESYNC failed, reconnecting for full enumeration", "error", deltaErr)
			c.observedMailboxDeltas = nil
			c.observedFolderStates = make(map[string]FolderState, len(allMailboxes))
			c.messageListCache = nil
			c.clearFolderAcknowledgements()
			if reconErr := c.reconnect(ctx); reconErr != nil {
				return fmt.Errorf("reconnect after QRESYNC failure: %w", reconErr)
			}
			c.clearMailboxDiscoveryLocked()
			if listErr := buildMailboxPlan(); listErr != nil {
				return fmt.Errorf("LIST after QRESYNC fallback: %w", listErr)
			}
			c.observedFolderStates = make(map[string]FolderState, len(allMailboxes))
			folderStatuses = c.observeFolderStates(ctx, allMailboxes)
			qresyncFallback = true
		case requireQresync && !handled:
			// Ineligible rather than failed. tryBuildQresyncMessageList returns
			// before issuing a command when no mailbox carries a mod-sequence
			// baseline, so the connection, the mailbox plan and the STATUS
			// results all still stand. A server that never reports a
			// mod-sequence takes this path on every run, where reconnecting
			// would redial and re-STATUS every mailbox to discard nothing.
			c.logger.Info("QRESYNC unavailable, enumerating fully")
			qresyncFallback = true
		case handled:
			return nil
		}
	}
	if trackFolders && !c.labelsSnapshotFilteredLocked() {
		// A clean full scan publishes the complete current topology. Keep a
		// non-nil empty slice so zero current mailboxes still reaches the
		// authoritative store apply and retires the prior topology.
		c.observedMailboxDeltas = make([]MailboxDelta, 0, len(allMailboxes))
	}

	// A scan without saved folder states or after a QRESYNC fallback builds an
	// authoritative membership map even when the server has no \All mailbox.
	// It is the mailbox coverage that makes the map authoritative, not a full
	// re-read: a mailbox the scan plan proves unchanged contributes a no-op
	// delta carrying its stored baseline, so the published topology still spans
	// every mailbox and the store reconciles that mailbox's labels from the
	// membership rows the no-op preserves. Filtered saved-state scans remain
	// additive.
	fullScanWithoutAll := c.allMailFolder == "" &&
		trackFolders &&
		(c.forceFullEnumeration || qresyncFallback || len(c.priorFolderStates) == 0) &&
		c.config != nil &&
		len(c.config.Folders) == 0 &&
		len(c.folderFilterInclude) == 0 &&
		len(c.folderFilterExclude) == 0
	buildMembershipMap := c.allMailFolder != "" || fullScanWithoutAll

	// Resolve every mailbox's enumeration plan up front so the label map and
	// the listing below agree. Only the no-\All path takes shortcuts; with an
	// \All mailbox both consumers keep their previous behaviour.
	var scans map[string]mailboxScan
	if trackFolders && c.allMailFolder == "" {
		scans = c.planMailboxScans(ctx, allMailboxes, folderStatuses)
	}
	labelMapComplete := false
	var unidentifiedMembershipMessages []gmailapi.MessageID
	if c.allMailFolder != "" {
		// On non-Gmail servers with \All, enumerate all selectable
		// mailboxes — \All may not be a superset of every folder.
		c.logger.Info("detected All Mail folder via \\All attribute",
			"folder", c.allMailFolder,
			"gmail", isGmailAllMail,
			"trash", c.trashMailbox,
			"junk", c.junkMailbox,
			"total_mailboxes", len(allMailboxes))
	}
	if buildMembershipMap {
		var mapErr error
		labelMapComplete, unidentifiedMembershipMessages, mapErr = c.buildLabelMap(ctx, allMailboxes, scans)
		if mapErr != nil {
			return mapErr
		}
		if labelMapComplete {
			// A complete membership map lets the first raw result carry every
			// mailbox label before overlapping copies become dedup stubs.
			c.seenRFC822IDs = make(map[string]bool)
		}
	}

	var messages []gmailapi.MessageID
	activeSourceAliases := make(map[string]string)
	var unchangedFolders int

	listOne := func(mailbox string) bool {
		var observed *FolderState
		var trackState FolderState
		var canTrackFolder bool
		var scan mailboxScan
		planned := false
		if trackFolders && c.allMailFolder == "" {
			if status, ok := folderStatuses[mailbox]; ok {
				observed = &status
				trackState = status
				canTrackFolder = true
				if scans != nil {
					scan, planned = scans[mailbox], true
					if scan.err != nil {
						c.logger.Warn("skipping mailbox",
							"mailbox", mailbox, "error", scan.err)
						return false
					}
					if scan.skip {
						// Publish an explicit no-op delta rather than dropping
						// the mailbox. ApplyIMAPMailboxDeltas derives the
						// current topology from the delta list alone and
						// retires -- and tombstones the messages of -- every
						// mailbox missing from it.
						trackState.KnownUIDs = scan.known
						c.observedFolderStates[mailbox] = trackState
						c.observedMailboxDeltas = append(c.observedMailboxDeltas, MailboxDelta{
							Mailbox:     mailbox,
							State:       trackState,
							Incremental: true,
						})
						unchangedFolders++
						return true
					}
				}
			}
		} else if trackFolders && c.allMailFolder != "" {
			if status, ok := folderStatuses[mailbox]; ok {
				trackState = status
				canTrackFolder = true
			}
		}

		var uids []imap.UID
		var knownUIDs []uint32
		if planned {
			uids, knownUIDs = scan.uids, scan.known
		} else {
			var err error
			uids, err = c.enumerateMailbox(ctx, mailbox, 0)
			if err != nil {
				c.logger.Warn("skipping mailbox", "mailbox", mailbox, "error", err)
				return false
			}
			knownUIDs = uidsToUint32(uids)
		}
		if observed != nil {
			observed.KnownUIDs = knownUIDs
			observed.UIDNext = baselineUIDNext(observed.UIDNext, knownUIDs)
			c.observedFolderStates[mailbox] = *observed
		}
		if canTrackFolder {
			trackState.KnownUIDs = knownUIDs
			trackState.UIDNext = baselineUIDNext(trackState.UIDNext, knownUIDs)
			c.trackFolderMessages(mailbox, trackState, uids)
		}
		if prior, ok := c.priorFolderStates[mailbox]; ok &&
			prior.UIDValidity == trackState.UIDValidity {
			c.loadSourceMessageAliases(mailbox, uids)
		}
		for _, uid := range uids {
			sourceMessageID := compositeID(mailbox, uid)
			messages = append(messages, gmailapi.MessageID{
				ID:       sourceMessageID,
				ThreadID: "",
			})
			if prior, ok := c.priorFolderStates[mailbox]; ok &&
				prior.UIDValidity == trackState.UIDValidity {
				if canonicalSourceMessageID := c.sourceMessageAliases[sourceMessageID]; canonicalSourceMessageID != "" {
					activeSourceAliases[sourceMessageID] = canonicalSourceMessageID
				}
			}
		}
		if canTrackFolder {
			_, hadPrior := c.priorFolderStates[mailbox]
			// A planned scan already decided whether this mailbox needs a full
			// republish; without a plan every listing is a full one.
			reset := !hadPrior || !planned || scan.reset
			if prior, ok := c.priorFolderStates[mailbox]; ok && prior.UIDValidity != trackState.UIDValidity {
				reset = true
			}
			c.observedMailboxDeltas = append(c.observedMailboxDeltas, MailboxDelta{
				Mailbox:      mailbox,
				State:        trackState,
				ChangedUIDs:  append([]imap.UID(nil), uids...),
				VanishedUIDs: append([]imap.UID(nil), scan.vanished...),
				Reset:        reset,
			})
		}
		c.logger.Debug("listed mailbox", "mailbox", mailbox, "count", len(uids))
		return true
	}

	if c.listProgress != nil {
		c.listProgress(0, len(listMailboxes), "", 0, 0)
	}
	enumerationComplete := true
	for i, mailbox := range listMailboxes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !listOne(mailbox) {
			enumerationComplete = false
		}
		if c.listProgress != nil {
			c.listProgress(i+1, len(listMailboxes), mailbox, len(messages), unchangedFolders)
		}
	}
	for _, message := range unidentifiedMembershipMessages {
		mailbox, _, parseErr := parseCompositeID(message.ID)
		if parseErr == nil && !slices.Contains(listMailboxes, mailbox) {
			messages = append(messages, message)
		}
	}
	statusesComplete := folderStatusesCoverMailboxes(allMailboxes, folderStatuses)
	authoritativeSnapshot := trackFolders && !c.labelsSnapshotFilteredLocked()
	if authoritativeSnapshot && (!statusesComplete || !labelMapComplete || !enumerationComplete) {
		labelMapComplete = false
		c.observedFolderStates = nil
		c.observedMailboxDeltas = nil
		c.clearFolderAcknowledgements()
	} else if authoritativeSnapshot && c.allMailFolder != "" {
		if labelMapComplete && enumerationComplete {
			deltaByMailbox := make(map[string]int, len(c.observedMailboxDeltas))
			for i, delta := range c.observedMailboxDeltas {
				deltaByMailbox[delta.Mailbox] = i
			}
			for _, mailbox := range allMailboxes {
				state := folderStatuses[mailbox]
				deltaIndex, hasDelta := deltaByMailbox[mailbox]
				if hasDelta {
					state.KnownUIDs = append(
						[]uint32(nil), c.observedMailboxDeltas[deltaIndex].State.KnownUIDs...)
				} else {
					state.KnownUIDs = make([]uint32, 0)
					for _, observation := range c.observedMemberships {
						if observation.Mailbox == mailbox && observation.UIDValidity == state.UIDValidity {
							state.KnownUIDs = append(state.KnownUIDs, observation.UID)
						}
					}
					slices.Sort(state.KnownUIDs)
				}
				// The snapshot rebuilds the state from STATUS, whose UIDNEXT was
				// read before the enumeration that produced these UIDs. A message
				// delivered in between belongs to the baseline and sits at or
				// above that mark, so the saved UIDNEXT has to cover it.
				state.UIDNext = baselineUIDNext(state.UIDNext, state.KnownUIDs)
				if hasDelta {
					c.observedMailboxDeltas[deltaIndex].State = state
				} else {
					c.observedMailboxDeltas = append(c.observedMailboxDeltas, MailboxDelta{
						Mailbox: mailbox,
						State:   state,
						Reset:   true,
					})
				}
				c.observedFolderStates[mailbox] = state
			}
		}
	}
	if authoritativeSnapshot && c.observedMailboxDeltas != nil &&
		!deltasCoverMailboxes(allMailboxes, c.observedMailboxDeltas) {
		// Publishing a partial topology is destructive, not merely incomplete:
		// the store retires every mailbox missing from the delta set, deleting
		// its memberships and tombstoning the messages that lived only there.
		c.logger.Warn("incomplete mailbox delta set, suppressing authoritative snapshot",
			"mailboxes", len(allMailboxes), "deltas", len(c.observedMailboxDeltas))
		labelMapComplete = false
		c.observedFolderStates = nil
		c.observedMailboxDeltas = nil
		c.clearFolderAcknowledgements()
	}
	if unchangedFolders > 0 {
		c.logger.Info("skipped unchanged mailboxes",
			"unchanged", unchangedFolders, "total", len(listMailboxes))
	}

	c.messageListCache = messages
	c.activeSourceAliases = activeSourceAliases
	c.labelMapComplete = labelMapComplete && enumerationComplete
	return nil
}

// deltasCoverMailboxes reports whether every current mailbox appears in the
// delta set. A mailbox that is genuinely gone is absent from mailboxes too, so
// a shortfall here means the listing dropped one, not that the server did.
func deltasCoverMailboxes(mailboxes []string, deltas []MailboxDelta) bool {
	covered := make(map[string]struct{}, len(deltas))
	for _, delta := range deltas {
		covered[delta.Mailbox] = struct{}{}
	}
	for _, mailbox := range mailboxes {
		if _, ok := covered[mailbox]; !ok {
			return false
		}
	}
	return true
}

func folderStatusesCoverMailboxes(
	mailboxes []string,
	statuses map[string]FolderState,
) bool {
	if len(statuses) != len(mailboxes) {
		return false
	}
	for _, mailbox := range mailboxes {
		if _, ok := statuses[mailbox]; !ok {
			return false
		}
	}
	return true
}

// isNetworkError reports whether err indicates the underlying TCP connection
// was closed or timed out, meaning the IMAP session must be re-established.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "operation timed out") ||
		strings.Contains(msg, "EOF")
}

// mailboxIncludedLocked reports whether mailbox is in the effective folder
// selection. Caller must hold mu.
func (c *Client) mailboxIncludedLocked(mailbox string) bool {
	return len(filterMailboxes(
		[]string{mailbox},
		c.effectiveFolderIncludeLocked(),
		c.folderFilterExclude,
	)) == 1
}

// effectiveFolderIncludeLocked returns the active folder allow list. Caller
// must hold mu.
func (c *Client) effectiveFolderIncludeLocked() []string {
	// CLI --folder replaces config folders when set; otherwise use
	// config.Folders.
	if len(c.folderFilterInclude) > 0 {
		return c.folderFilterInclude
	}
	return c.config.Folders
}

// filterMailboxes applies an include list (allow-list) and/or an
// exclude list (deny-list) to the mailbox names returned by LIST.
// Include and exclude are case-insensitive. Empty lists are no-ops.
// If both are non-empty, include is applied first, then exclude.
// When include is empty but exclude is set, the result is all
// mailboxes minus the excluded names (case-insensitive). When
// include is set but exclude is empty, only the names in include
// are kept (case-insensitively).
func filterMailboxes(all []string, include, exclude []string) []string {
	if len(include) == 0 && len(exclude) == 0 {
		return all
	}
	lcInclude := make(map[string]bool, len(include))
	for _, f := range include {
		lcInclude[strings.ToLower(f)] = true
	}
	lcExclude := make(map[string]bool, len(exclude))
	for _, f := range exclude {
		lcExclude[strings.ToLower(f)] = true
	}
	var out = make([]string, 0, len(all))
	for _, m := range all {
		lc := strings.ToLower(m)
		if len(lcInclude) > 0 && !lcInclude[lc] {
			continue
		}
		if len(lcExclude) > 0 && lcExclude[lc] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// hasAttr checks whether attr is in the attrs list.
func hasAttr(attrs []imap.MailboxAttr, attr imap.MailboxAttr) bool {
	return slices.Contains(attrs, attr)
}

// MailboxInfo is the result of listing a mailbox with its count.
type MailboxInfo struct {
	Mailbox     string
	NumMessages int64 // -1 = count unavailable
}

// listMailboxesLockedFromConn returns all selectable mailbox names
// for an already-connected IMAP client. It skips NoSelect mailboxes.
func listMailboxesLockedFromConn(conn *imapclient.Client) ([]string, error) {
	items, err := conn.List("", "*", nil).Collect()
	if err != nil {
		return nil, fmt.Errorf("LIST: %w", err)
	}
	var names []string
	for _, item := range items {
		if hasAttr(item.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		names = append(names, item.Mailbox)
	}
	return names, nil
}

// compositeID builds a message identifier as "mailbox|uid".
func compositeID(mailbox string, uid imap.UID) string {
	return mailbox + "|" + strconv.FormatUint(uint64(uid), 10)
}

// parseCompositeID splits a composite message ID into mailbox and UID.
func parseCompositeID(id string) (mailbox string, uid imap.UID, err error) {
	idx := strings.LastIndexByte(id, '|')
	if idx < 0 {
		return "", 0, fmt.Errorf("invalid IMAP message ID %q (expected mailbox|uid)", id)
	}
	n, parseErr := strconv.ParseUint(id[idx+1:], 10, 32)
	if parseErr != nil {
		return "", 0, fmt.Errorf("invalid UID in message ID %q: %w", id, parseErr)
	}
	return id[:idx], imap.UID(n), nil
}

// GetProfile returns the IMAP account profile.
// Uses STATUS INBOX to get the message count; the username is used as the email address.
func (c *Client) GetProfile(ctx context.Context) (*gmailapi.Profile, error) {
	var profile gmailapi.Profile
	err := c.withConn(ctx, func(conn *imapclient.Client) error {
		statusData, err := conn.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait()
		if err != nil {
			return fmt.Errorf("STATUS INBOX: %w", err)
		}
		var total int64
		if statusData.NumMessages != nil {
			total = int64(*statusData.NumMessages)
		}
		profile = gmailapi.Profile{
			EmailAddress:  c.config.Username,
			MessagesTotal: total,
			HistoryID:     0,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &profile, nil
}

// ListLabels returns all IMAP mailboxes as labels.
func (c *Client) ListLabels(ctx context.Context) ([]*gmailapi.Label, error) {
	var labels []*gmailapi.Label
	err := c.withConn(ctx, func(conn *imapclient.Client) error {
		items, err := conn.List("", "*", c.listOptionsLocked()).Collect()
		if err != nil {
			return fmt.Errorf("LIST: %w", err)
		}
		for _, item := range items {
			labelType := classifyLabelType(item.Mailbox, item.Attrs)
			labels = append(labels, &gmailapi.Label{
				ID:         item.Mailbox,
				Name:       item.Mailbox,
				Type:       labelType,
				SystemRole: systemRoleForMailbox(item.Attrs),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return labels, nil
}

// ListMailboxes returns all selectable mailbox names for this
// account. It connects, authenticates, and runs LIST "" "*. The
// result includes the approximate message count for each folder.
func (c *Client) ListMailboxes(ctx context.Context) ([]MailboxInfo, error) {
	var info []MailboxInfo
	err := c.withConn(ctx, func(conn *imapclient.Client) error {
		names, err := listMailboxesLockedFromConn(conn)
		if err != nil {
			return err
		}
		for _, mb := range names {
			// STATUS round-trip per mailbox — the go-imap/v2 library
			// at v2.0.0-beta.8 does not expose a multi-mailbox batch
			// STATUS call, so we issue one STATUS per folder.  This
			// can be replaced with a single batch STATUS once the
			// library adds a multi-mailbox API or a newer version is
			// adopted. See: github.com/emersion/go-imap/issues
			status, err := conn.Status(mb, &imap.StatusOptions{NumMessages: true}).Wait()
			if err != nil {
				info = append(info, MailboxInfo{Mailbox: mb, NumMessages: -1})
				continue
			}
			count := int64(0)
			if status.NumMessages != nil {
				count = int64(*status.NumMessages)
			}
			info = append(info, MailboxInfo{Mailbox: mb, NumMessages: count})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// ListMessages returns a page of message IDs from all IMAP mailboxes.
//
// The first call (pageToken == "") enumerates all mailboxes and caches the full
// list of message IDs; subsequent calls return successive pages of listPageSize
// using the returned NextPageToken as a numeric offset. This matches the Gmail
// pagination contract so the sync loop checkpoints and reports progress
// frequently on large mailboxes.
func (c *Client) ListMessages(ctx context.Context, query string, pageToken string) (*gmailapi.MessageListResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.connect(ctx); err != nil {
		return nil, err
	}

	// Build the full message ID list once per session.
	if c.messageListCache == nil {
		if err := c.buildMessageListCache(ctx); err != nil {
			return nil, err
		}
	}

	// Parse page offset from token.
	offset := 0
	if pageToken != "" {
		n, err := strconv.Atoi(pageToken)
		if err != nil || n < 0 {
			return &gmailapi.MessageListResponse{}, nil
		}
		offset = n
	}

	all := c.messageListCache
	total := int64(len(all))

	if offset >= len(all) {
		return &gmailapi.MessageListResponse{ResultSizeEstimate: total}, nil
	}

	end := min(offset+listPageSize, len(all))

	nextToken := ""
	if end < len(all) {
		nextToken = strconv.Itoa(end)
	}

	return &gmailapi.MessageListResponse{
		Messages:           all[offset:end],
		NextPageToken:      nextToken,
		ResultSizeEstimate: total,
	}, nil
}

// GetMessageRaw fetches a single IMAP message by composite ID.
func (c *Client) GetMessageRaw(ctx context.Context, messageID string) (*gmailapi.RawMessage, error) {
	msgs, err := c.GetMessagesRawBatch(ctx, []string{messageID})
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 || msgs[0] == nil {
		return nil, fmt.Errorf("message %s not found", messageID)
	}
	return msgs[0], nil
}

// GetMessagesRawBatch fetches multiple messages and drops per-item diagnostics
// for legacy callers. Results are returned in the same order as messageIDs.
func (c *Client) GetMessagesRawBatch(ctx context.Context, messageIDs []string) ([]*gmailapi.RawMessage, error) {
	results, err := c.GetMessagesRawBatchWithErrors(ctx, messageIDs)
	return rawBatchMessages(results), err
}

// ListHistory is not supported for IMAP servers.
// Callers should run a full sync instead of incremental sync for IMAP sources.
func (c *Client) ListHistory(_ context.Context, _ uint64, _ string) (*gmailapi.HistoryResponse, error) {
	return nil, errors.New("IMAP does not support history-based incremental sync")
}

// TrashMessage moves a message to the server's Trash folder.
func (c *Client) TrashMessage(ctx context.Context, messageID string) error {
	mailbox, uid, err := parseCompositeID(messageID)
	if err != nil {
		return err
	}
	return c.withConn(ctx, func(conn *imapclient.Client) error {
		if err := c.selectMailbox(mailbox); err != nil {
			return err
		}
		// Populate trashMailbox via LIST if not yet discovered.
		if c.trashMailbox == "" {
			if _, err := c.listMailboxesLocked(); err != nil {
				c.logger.Warn("failed to discover trash mailbox, will use default", "error", err)
			}
		}
		trashMailbox := c.trashMailbox
		if trashMailbox == "" {
			trashMailbox = Trash
		}
		var uidSet imap.UIDSet
		uidSet.AddNum(uid)
		if _, err := conn.Move(uidSet, trashMailbox).Wait(); err != nil {
			return fmt.Errorf("MOVE to %q: %w", trashMailbox, err)
		}
		return nil
	})
}

// DeleteMessage permanently deletes a message using UID STORE \Deleted
// + UID EXPUNGE. Requires the UIDPLUS extension (RFC 4315); without it
// plain EXPUNGE would remove every \Deleted message in the mailbox,
// not just the target.
func (c *Client) DeleteMessage(ctx context.Context, messageID string) error {
	mailbox, uid, err := parseCompositeID(messageID)
	if err != nil {
		return err
	}
	return c.withConn(ctx, func(conn *imapclient.Client) error {
		if !conn.Caps().Has(imap.CapUIDPlus) {
			return errors.New("server does not support UIDPLUS; " +
				"permanent delete requires UID EXPUNGE " +
				"(use trash instead)")
		}
		if err := c.selectMailbox(mailbox); err != nil {
			return err
		}
		var uidSet imap.UIDSet
		uidSet.AddNum(uid)
		if err := conn.Store(uidSet, &imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Silent: true,
			Flags:  []imap.Flag{imap.FlagDeleted},
		}, nil).Close(); err != nil {
			return fmt.Errorf("UID STORE \\Deleted: %w", err)
		}
		if err := conn.UIDExpunge(uidSet).Close(); err != nil {
			return fmt.Errorf("UID EXPUNGE: %w", err)
		}
		return nil
	})
}

// BatchDeleteMessages always returns an error to signal that IMAP
// does not support atomic batch deletion. The deletion executor
// falls back to per-message DeleteMessage calls, which avoids the
// double-retry problem that would occur if we deleted some messages
// here and then the executor retried the entire batch.
func (c *Client) BatchDeleteMessages(_ context.Context, _ []string) error {
	return errors.New("IMAP does not support batch delete")
}

// Close logs out and disconnects from the IMAP server.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	conn := c.conn
	c.conn = nil
	c.selectedMailbox = ""
	c.selectedUIDValidity = 0
	if err := conn.Logout().Wait(); err != nil {
		return fmt.Errorf("IMAP logout: %w", err)
	}
	return nil
}
