package imap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftRemoveExactUIDCondStoreWire(t *testing.T) {
	for _, scenario := range []struct {
		name            string
		conflictStatus  string
		externalDeleted bool
	}{
		{name: "success"},
		{name: "NO conflict", conflictStatus: "NO"},
		{name: "OK conflict", conflictStatus: "OK"},
		{name: "OK conflict with external Deleted", conflictStatus: "OK", externalDeleted: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			addr, server := startDraftRemovalWireServer(t, draftRemovalWireOptions{
				conflictStatus: scenario.conflictStatus, externalDeleted: scenario.externalDeleted, fetchModSeq: "41",
			})
			client := reviewIMAPClient(t, addr)
			removed, err := client.RemoveDraft(t.Context(), DraftReceipt{
				Mailbox: "Drafts", UIDValidity: 77, UID: 7,
			})
			assertions.True(removed.WriteAttempted)
			if scenario.conflictStatus != "" {
				requirements.Error(err)
				assertions.Equal("store_conflict", removed.Code)
				assertions.False(removed.Complete)
				assertions.True(removed.Present)
				assertions.Equal(scenario.externalDeleted, removed.Deleted)
			} else {
				requirements.NoError(err)
				assertions.True(removed.Complete)
				assertions.False(removed.Present)
			}

			commands := server.commandTexts()
			requirements.GreaterOrEqual(len(commands), 5)
			joined := strings.Join(commands, "\n")
			assertions.Contains(joined, "SELECT \"Drafts\" (CONDSTORE)")
			assertions.Contains(joined, "UID FETCH 7 (")
			assertions.Contains(joined, "MODSEQ")
			assertions.Contains(joined, "FLAGS")
			assertions.Contains(joined, "UID STORE 7 (UNCHANGEDSINCE 41) +FLAGS.SILENT (\\Deleted)")
			assertions.Contains(joined, "SELECT \"Drafts\"")
			assertions.Contains(joined, "UID FETCH 7 (UID FLAGS)")
			if scenario.conflictStatus != "" {
				assertions.NotContains(joined, "UID EXPUNGE")
				return
			}
			assertions.Contains(joined, "UID EXPUNGE 7")
			assertions.Contains(joined, "EXAMINE \"Drafts\"")
		})
	}
}

func TestDraftMailboxModSeq(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		options draftRemovalWireOptions
		code    string
	}{
		{name: "persistent", options: draftRemovalWireOptions{fetchModSeq: "41"}},
		{name: "NOMODSEQ", options: draftRemovalWireOptions{noModSeq: true}, code: "modseq_unusable"},
		{name: "missing SELECT MODSEQ", options: draftRemovalWireOptions{omitSelectModSeq: true}, code: "modseq_unusable"},
		{name: "without CONDSTORE", options: draftRemovalWireOptions{noCondStore: true, omitSelectModSeq: true}},
		{name: "missing message MODSEQ", code: "modseq_unusable"},
		{name: "zero message MODSEQ", options: draftRemovalWireOptions{fetchModSeq: "0"}, code: "modseq_unusable"},
		{name: "FETCH failure", options: draftRemovalWireOptions{fetchError: true}, code: "fetch_failed"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			addr, server := startDraftRemovalWireServer(t, scenario.options)
			client := reviewIMAPClient(t, addr)
			receipt := DraftReceipt{Mailbox: "Drafts", UIDValidity: 77, UID: 7}
			observation, err := client.InspectDraft(t.Context(), receipt)
			if scenario.code == "" {
				requirements.NoError(err)
				assertions.True(observation.Present)
			} else {
				requirements.Error(err)
				assertions.Equal(scenario.code, observation.Code)
			}
			assertions.False(observation.WriteAttempted)
			commands := strings.Join(server.commandTexts(), "\n")
			if scenario.options.noCondStore {
				assertions.NotContains(commands, "(CONDSTORE)")
			} else {
				assertions.Contains(commands, "EXAMINE \"Drafts\" (CONDSTORE)")
			}
			assertions.NotContains(commands, "UID STORE")

			removed, err := client.RemoveDraft(t.Context(), receipt)
			commands = strings.Join(server.commandTexts(), "\n")
			if scenario.code != "" {
				requirements.Error(err)
				assertions.Equal(scenario.code, removed.Code)
				assertions.False(removed.WriteAttempted)
				assertions.NotContains(commands, "UID STORE")
				assertions.NotContains(commands, "UID EXPUNGE")
				return
			}
			requirements.NoError(err)
			assertions.True(removed.Complete)
			assertions.True(removed.WriteAttempted)
			assertions.Contains(commands, "UID EXPUNGE 7")
			if scenario.options.noCondStore {
				assertions.NotContains(commands, "MODSEQ")
				assertions.NotContains(commands, "UNCHANGEDSINCE")
				assertions.Contains(commands, "UID STORE 7 +FLAGS.SILENT (\\Deleted)")
			} else {
				assertions.Contains(commands, "UID STORE 7 (UNCHANGEDSINCE 41) +FLAGS.SILENT (\\Deleted)")
			}
		})
	}
}

type draftRemovalWireOptions struct {
	conflictStatus   string
	externalDeleted  bool
	noModSeq         bool
	noCondStore      bool
	omitSelectModSeq bool
	fetchModSeq      string
	fetchError       bool
}

type draftRemovalWireServer struct {
	mu       sync.Mutex
	commands []string
}

func (s *draftRemovalWireServer) record(command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, command)
}

func (s *draftRemovalWireServer) commandTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	commands := make([]string, 0, len(s.commands))
	for _, line := range s.commands {
		_, command, ok := strings.Cut(line, " ")
		if ok {
			commands = append(commands, command)
		}
	}
	return commands
}

func startDraftRemovalWireServer(t *testing.T, options draftRemovalWireOptions) (string, *draftRemovalWireServer) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &draftRemovalWireServer{}
	go serveDraftRemovalWire(listener, server, options)
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), server
}

func serveDraftRemovalWire(listener net.Listener, server *draftRemovalWireServer, options draftRemovalWireOptions) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go serveDraftRemovalWireConn(conn, server, options)
	}
}

func serveDraftRemovalWireConn(conn net.Conn, server *draftRemovalWireServer, options draftRemovalWireOptions) {
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "* OK synthetic IMAP ready\r\n")
	reader := bufio.NewReader(conn)
	deleted, expunged := false, false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		server.record(line)
		tag, command, ok := strings.Cut(line, " ")
		if !ok {
			return
		}
		upper := strings.ToUpper(command)
		switch {
		case upper == "CAPABILITY":
			caps := "IMAP4rev1 UIDPLUS"
			if !options.noCondStore {
				caps += " CONDSTORE"
			}
			_, _ = fmt.Fprintf(conn, "* CAPABILITY %s\r\n%s OK CAPABILITY completed\r\n", caps, tag)
		case strings.HasPrefix(upper, "LOGIN "):
			_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
		case strings.HasPrefix(upper, "SELECT ") || strings.HasPrefix(upper, "EXAMINE "):
			exists := 1
			if expunged {
				exists = 0
			}
			modSeqCode := "HIGHESTMODSEQ 42"
			if options.noModSeq {
				modSeqCode = "NOMODSEQ"
			}
			modSeqResponse := "* OK [" + modSeqCode + "]\r\n"
			if options.omitSelectModSeq {
				modSeqResponse = ""
			}
			_, _ = fmt.Fprintf(conn,
				"* FLAGS (\\Draft \\Deleted)\r\n* %d EXISTS\r\n* OK [UIDVALIDITY 77]\r\n* OK [UIDNEXT 8]\r\n%s%s OK SELECT completed\r\n",
				exists, modSeqResponse, tag)
		case strings.HasPrefix(upper, "UID FETCH "):
			if options.fetchError || (options.noModSeq && strings.Contains(upper, "MODSEQ")) {
				_, _ = fmt.Fprintf(conn, "%s BAD FETCH rejected\r\n", tag)
				continue
			}
			if !expunged {
				flags := "\\Draft"
				if deleted {
					flags += " \\Deleted"
				}
				modSeq := ""
				if options.fetchModSeq != "" {
					modSeq = " MODSEQ (" + options.fetchModSeq + ")"
				}
				_, _ = fmt.Fprintf(conn, "* 1 FETCH (UID 7 FLAGS (%s)%s)\r\n", flags, modSeq)
			}
			_, _ = fmt.Fprintf(conn, "%s OK UID FETCH completed\r\n", tag)
		case strings.HasPrefix(upper, "UID STORE "):
			if options.noModSeq && strings.Contains(upper, "UNCHANGEDSINCE") {
				_, _ = fmt.Fprintf(conn, "%s BAD STORE rejected\r\n", tag)
			} else if options.conflictStatus != "" {
				if options.externalDeleted {
					deleted = true
					options.fetchModSeq = "42"
				}
				_, _ = fmt.Fprintf(conn, "%s %s [MODIFIED 7] conditional conflict\r\n", tag, options.conflictStatus)
			} else {
				deleted = true
				_, _ = fmt.Fprintf(conn, "%s OK UID STORE completed\r\n", tag)
			}
		case strings.HasPrefix(upper, "UID EXPUNGE "):
			expunged = deleted
			_, _ = fmt.Fprintf(conn, "%s OK UID EXPUNGE completed\r\n", tag)
		case upper == "LOGOUT":
			_, _ = fmt.Fprintf(conn, "* BYE closing\r\n%s OK LOGOUT completed\r\n", tag)
			return
		default:
			_, _ = fmt.Fprintf(conn, "%s BAD unsupported synthetic command\r\n", tag)
		}
	}
}

func TestDraftRemoveExactUIDRefusals(t *testing.T) {
	t.Run("initially absent", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
			MessagesPerMailbox: map[string]int{"Drafts": 0},
			Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
		})
		client := reviewIMAPClient(t, addr)
		receipt, err := client.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("target"))
		requirements.NoError(err)
		requirements.NoError(client.Close())
		testutil.ExpungeIMAPMessage(t, addr, "Drafts", emersionimap.UID(receipt.UID))
		client = reviewIMAPClient(t, addr)
		removed, err := client.RemoveDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: receipt.UIDValidity, UID: receipt.UID})
		requirements.Error(err)
		assertions.Equal("not_found", removed.Code)
		assertions.False(removed.WriteAttempted)
	})

	t.Run("UIDVALIDITY mismatch", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
			MessagesPerMailbox: map[string]int{"Drafts": 1},
			Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
		})
		client := reviewIMAPClient(t, addr)
		observation, err := client.InspectDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: 999, UID: 1})
		requirements.Error(err)
		assertions.Equal("uidvalidity_mismatch", observation.Code)
	})

	t.Run("folder generation changed", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		addr, user := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
			MessagesPerMailbox: map[string]int{"Drafts": 0},
			Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
		})
		client := reviewIMAPClient(t, addr)
		receipt, err := client.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("old generation"))
		requirements.NoError(err)
		requirements.NoError(user.Delete("Drafts"))
		requirements.NoError(user.Create("Drafts", nil))
		observation, err := client.InspectDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: receipt.UIDValidity, UID: receipt.UID})
		requirements.Error(err)
		assertions.Equal("uidvalidity_mismatch", observation.Code)
	})

	t.Run("already deleted", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
			MessagesPerMailbox: map[string]int{"Drafts": 0},
			Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
		})
		client := reviewIMAPClient(t, addr)
		receipt, err := client.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("target"))
		requirements.NoError(err)
		requirements.NoError(client.Close())
		reviewStoreFlags(t, addr, receipt.UID, emersionimap.StoreFlagsAdd)
		client = reviewIMAPClient(t, addr)
		removed, err := client.RemoveDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: receipt.UIDValidity, UID: receipt.UID})
		requirements.Error(err)
		assertions.Equal("already_deleted", removed.Code)
		assertions.False(removed.WriteAttempted)
	})

	t.Run("missing Draft flag", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		addr, user := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
			MessagesPerMailbox: map[string]int{"Drafts": 0},
			Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
		})
		testutil.AppendIMAPRawMessage(t, user, "Drafts", reviewDraftRaw("ordinary"))
		client := reviewIMAPClient(t, addr)
		observation, err := client.InspectDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: 1, UID: 1})
		requirements.Error(err)
		assertions.Equal("not_draft", observation.Code)
	})

	t.Run("UIDPLUS required", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
			MessagesPerMailbox: map[string]int{"Drafts": 0},
			Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}},
		})
		client := reviewIMAPClient(t, addr)
		removed, err := client.RemoveDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: 1, UID: 1})
		requirements.Error(err)
		assertions.Equal("uidplus_required", removed.Code)
		assertions.False(removed.WriteAttempted)
	})
}

func TestDraftRemoveExactUIDKeepsUnrelatedDeletedUID(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
	})
	client := reviewIMAPClient(t, addr)
	target, err := client.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("target"))
	requirements.NoError(err)
	unrelated, err := client.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("unrelated"))
	requirements.NoError(err)
	requirements.NoError(client.Close())
	reviewStoreFlags(t, addr, unrelated.UID, emersionimap.StoreFlagsAdd)
	client = reviewIMAPClient(t, addr)
	removed, err := client.RemoveDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: target.UIDValidity, UID: target.UID})
	requirements.NoError(err)
	assertions.True(removed.Complete)
	remaining := reviewIMAPClient(t, addr)
	observation, err := remaining.InspectDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: unrelated.UIDValidity, UID: unrelated.UID})
	requirements.NoError(err)
	assertions.True(observation.Present)
	assertions.True(observation.Deleted)
}

func TestDraftRemoveExactUIDReportsStoreConflictAfterDeletedFlagRace(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	barrier := &reviewStoreBarrier{stored: make(chan struct{}), release: make(chan struct{})}
	addr := startReviewIMAPServer(t, emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}}, func(session imapserver.Session, _ *imapserver.Conn) imapserver.Session {
		return &reviewStoreBarrierSession{Session: session, barrier: barrier}
	})
	client := reviewIMAPClient(t, addr)
	receipt, err := client.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("target"))
	requirements.NoError(err)
	requirements.NoError(client.Close())

	type result struct {
		observation DraftObservation
		err         error
	}
	done := make(chan result, 1)
	client = reviewIMAPClient(t, addr)
	go func() {
		observation, err := client.RemoveDraft(t.Context(), DraftReceipt{Mailbox: "Drafts", UIDValidity: receipt.UIDValidity, UID: receipt.UID})
		done <- result{observation: observation, err: err}
	}()
	<-barrier.stored
	reviewStoreFlags(t, addr, receipt.UID, emersionimap.StoreFlagsDel)
	close(barrier.release)
	removed := <-done
	requirements.Error(removed.err)
	assertions.Equal("store_conflict", removed.observation.Code)
	assertions.False(removed.observation.Complete)
	assertions.True(removed.observation.Present)
	assertions.Zero(barrier.expungeCalls.Load())
}

func TestDraftRemoveExactUIDReportsSurvivorAfterExpunge(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	addr := startReviewIMAPServer(t, emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}}, func(session imapserver.Session, _ *imapserver.Conn) imapserver.Session {
		return &reviewNoopExpungeSession{Session: session}
	})
	client := reviewIMAPClient(t, addr)
	receipt, err := client.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("target"))
	requirements.NoError(err)
	removed, err := client.RemoveDraft(t.Context(), receiptToDraftReceipt(receipt))
	requirements.Error(err)
	assertions.Equal("survivor", removed.Code)
	assertions.False(removed.Complete)
	assertions.True(removed.Present)
}

func TestDraftRemoveExactUIDRefusesDraftFlagChangeBeforeExpunge(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	barrier := &reviewStoreBarrier{stored: make(chan struct{}), release: make(chan struct{})}
	addr := startReviewIMAPServer(t, emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}}, func(session imapserver.Session, _ *imapserver.Conn) imapserver.Session {
		return &reviewStoreBarrierSession{Session: session, barrier: barrier}
	})
	client := reviewIMAPClient(t, addr)
	receipt, err := client.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("target"))
	requirements.NoError(err)
	requirements.NoError(client.Close())

	type result struct {
		observation DraftObservation
		err         error
	}
	done := make(chan result, 1)
	client = reviewIMAPClient(t, addr)
	go func() {
		observation, err := client.RemoveDraft(t.Context(), receiptToDraftReceipt(receipt))
		done <- result{observation: observation, err: err}
	}()
	<-barrier.stored
	reviewStoreFlag(t, addr, receipt.UID, emersionimap.StoreFlagsDel, emersionimap.FlagDraft)
	close(barrier.release)
	removed := <-done
	requirements.Error(removed.err)
	assertions.Equal("not_draft", removed.observation.Code)
	assertions.True(removed.observation.WriteAttempted)
	assertions.False(removed.observation.Complete)
	assertions.Zero(barrier.expungeCalls.Load())
}

func TestDraftRemoveExactUIDRefusesGenerationChangeBeforeExpunge(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	barrier := &reviewStoreBarrier{stored: make(chan struct{}), release: make(chan struct{})}
	addr := startReviewIMAPServer(t, emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}}, func(session imapserver.Session, _ *imapserver.Conn) imapserver.Session {
		return &reviewStoreBarrierSession{Session: session, barrier: barrier}
	})
	client := reviewIMAPClient(t, addr)
	receipt, err := client.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("target"))
	requirements.NoError(err)
	requirements.NoError(client.Close())

	generationChanged := make(chan error, 1)
	barrier.onStore = func() {
		external, dialErr := imapclient.DialInsecure(addr, nil)
		if dialErr != nil {
			generationChanged <- dialErr
			return
		}
		defer func() { _ = external.Close() }()
		if loginErr := external.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait(); loginErr != nil {
			generationChanged <- loginErr
			return
		}
		if deleteErr := external.Delete("Drafts").Wait(); deleteErr != nil {
			generationChanged <- deleteErr
			return
		}
		generationChanged <- external.Create("Drafts", nil).Wait()
	}

	type result struct {
		observation DraftObservation
		err         error
	}
	done := make(chan result, 1)
	client = reviewIMAPClient(t, addr)
	go func() {
		observation, err := client.RemoveDraft(t.Context(), receiptToDraftReceipt(receipt))
		done <- result{observation: observation, err: err}
	}()
	<-barrier.stored
	requirements.NoError(<-generationChanged)
	close(barrier.release)
	removed := <-done
	requirements.Error(removed.err)
	assertions.Equal("uidvalidity_mismatch", removed.observation.Code)
	assertions.True(removed.observation.WriteAttempted)
	assertions.False(removed.observation.Complete)
	assertions.Zero(barrier.expungeCalls.Load())
}

func TestDraftLifecycleCancellationBarriers(t *testing.T) {
	t.Run("append cancellation leaves uncertain result", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		barrier := newReviewWireBarrier("APPEND")
		defer barrier.releaseNow()
		addr := startReviewIMAPServer(t, emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}}, func(session imapserver.Session, conn *imapserver.Conn) imapserver.Session {
			return &reviewWireBarrierSession{Session: session, barrier: barrier}
		})
		seedClient := reviewIMAPClient(t, addr)
		_, err := seedClient.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("seed"))
		requirements.NoError(err)
		requirements.NoError(seedClient.Close())
		barrier.armed.Store(true)
		client := reviewIMAPClient(t, addr)
		oldConn := reviewFreshConn(t, client)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		resultCh := make(chan DraftAppendResult, 1)
		errCh := make(chan error, 1)
		go func() {
			result, err := client.AppendDraft(ctx, "Drafts", reviewDraftRaw("candidate"))
			resultCh <- result
			errCh <- err
		}()
		<-barrier.entered
		cancel()
		result, err := awaitReviewAppendResult(t, resultCh, errCh)
		requirements.Error(err)
		assertions.Equal(DraftStateRemoteUnknown, result.State)
		assertions.Equal("remote_unknown", result.Code)
		assertions.Equal(int32(2), barrier.appendCalls.Load())
		assertions.Zero(barrier.selectCalls.Load())
		assertions.Zero(barrier.fetchCalls.Load())
		assertions.Zero(barrier.storeCalls.Load())
		assertions.Zero(barrier.expungeCalls.Load())
		requirements.Nil(reviewCurrentConn(t, client))
		barrier.releaseNow()
		fresh := reviewFreshConn(t, client)
		assertions.NotSame(oldConn, fresh)
	})

	for _, command := range []string{"SELECT", "FETCH", "STORE", "EXPUNGE"} {
		t.Run("removal cancellation stops before "+command, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			barrier := newReviewWireBarrier(command)
			defer barrier.releaseNow()
			addr := startReviewIMAPServer(t, emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}}, func(session imapserver.Session, _ *imapserver.Conn) imapserver.Session {
				return &reviewWireBarrierSession{Session: session, barrier: barrier}
			})
			seedClient := reviewIMAPClient(t, addr)
			receipt, err := seedClient.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("target"))
			requirements.NoError(err)
			requirements.NoError(seedClient.Close())
			barrier.armed.Store(true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			resultCh := make(chan DraftObservation, 1)
			errCh := make(chan error, 1)
			client := reviewIMAPClient(t, addr)
			oldConn := reviewFreshConn(t, client)
			go func() {
				observation, err := client.RemoveDraft(ctx, DraftReceipt{Mailbox: "Drafts", UIDValidity: receipt.UIDValidity, UID: receipt.UID})
				resultCh <- observation
				errCh <- err
			}()
			<-barrier.entered
			cancel()
			observation, err := awaitReviewRemovalResult(t, resultCh, errCh)
			requirements.Error(err)
			assertions.Equal("cancelled", observation.Code)
			assertions.Equal(command == "STORE" || command == "EXPUNGE", observation.WriteAttempted)
			assertions.False(observation.Complete)
			switch command {
			case "SELECT":
				assertions.Equal(int32(1), barrier.selectCalls.Load())
				assertions.Zero(barrier.fetchCalls.Load())
				assertions.Zero(barrier.storeCalls.Load())
				assertions.Zero(barrier.expungeCalls.Load())
			case "FETCH":
				assertions.Equal(int32(1), barrier.selectCalls.Load())
				assertions.Equal(int32(1), barrier.fetchCalls.Load())
				assertions.Zero(barrier.storeCalls.Load())
				assertions.Zero(barrier.expungeCalls.Load())
			case "STORE":
				assertions.Equal(int32(1), barrier.selectCalls.Load())
				assertions.Equal(int32(1), barrier.fetchCalls.Load())
				assertions.Equal(int32(1), barrier.storeCalls.Load())
				assertions.Zero(barrier.expungeCalls.Load())
			case "EXPUNGE":
				assertions.Equal(int32(2), barrier.selectCalls.Load())
				assertions.Equal(int32(2), barrier.fetchCalls.Load())
				assertions.Equal(int32(1), barrier.storeCalls.Load())
				assertions.Equal(int32(1), barrier.expungeCalls.Load())
			}
			requirements.Nil(reviewCurrentConn(t, client))
			barrier.releaseNow()
			fresh := reviewFreshConn(t, client)
			assertions.NotSame(oldConn, fresh)
		})
	}
}

func TestDraftNetworkFailureInvalidatesTransport(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	var dropped atomic.Bool
	addr := startReviewIMAPServer(t, emersionimap.CapSet{
		emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {},
	}, func(session imapserver.Session, conn *imapserver.Conn) imapserver.Session {
		return &reviewDropConnectionSession{
			Session: session, conn: conn, dropped: &dropped,
		}
	})
	seedClient := reviewIMAPClient(t, addr)
	receipt, err := seedClient.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("target"))
	requirements.NoError(err)
	requirements.NoError(seedClient.Close())
	client := reviewIMAPClient(t, addr)
	oldConn := reviewFreshConn(t, client)
	observation, err := client.InspectDraft(t.Context(), receiptToDraftReceipt(receipt))
	requirements.Error(err)
	assertions.True(dropped.Load())
	assertions.Equal("select_failed", observation.Code)
	requirements.Nil(reviewCurrentConn(t, client))
	observation, err = client.InspectDraft(t.Context(), receiptToDraftReceipt(receipt))
	requirements.NoError(err)
	assertions.True(observation.Present)
	fresh := reviewCurrentConn(t, client)
	assertions.NotSame(oldConn, fresh)
}

func TestAppendDraftPreservesTransportResultWhenCancellationRaces(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	baseCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tracingCtx := &reviewAppendRaceContext{Context: baseCtx, cancel: cancel}
	var enabled atomic.Bool
	addr := startReviewIMAPServer(t, emersionimap.CapSet{
		emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {},
	}, func(session imapserver.Session, conn *imapserver.Conn) imapserver.Session {
		return &reviewDropAppendSession{
			Session: session, conn: conn, enabled: &enabled, arm: tracingCtx.arm,
		}
	})
	seedClient := reviewIMAPClient(t, addr)
	_, err := seedClient.AppendDraft(t.Context(), "Drafts", reviewDraftRaw("seed"))
	requirements.NoError(err)
	requirements.NoError(seedClient.Close())
	enabled.Store(true)
	client := reviewIMAPClient(t, addr)
	result, err := client.AppendDraft(tracingCtx, "Drafts", reviewDraftRaw("candidate"))
	requirements.Error(err)
	assertions.Equal(DraftStateRemoteUnknown, result.State)
	assertions.Equal("remote_unknown", result.Code)
	assertions.Equal("remote_unknown", err.Error())
	assertions.True(tracingCtx.cancelled.Load())
}

func receiptToDraftReceipt(receipt DraftAppendResult) DraftReceipt {
	return DraftReceipt{Mailbox: "Drafts", UIDValidity: receipt.UIDValidity, UID: receipt.UID}
}

type reviewWireBarrier struct {
	command   string
	entered   chan struct{}
	release   chan struct{}
	armed     atomic.Bool
	blocked   atomic.Bool
	releaseMu sync.Once

	appendCalls  atomic.Int32
	selectCalls  atomic.Int32
	fetchCalls   atomic.Int32
	storeCalls   atomic.Int32
	expungeCalls atomic.Int32
}

func newReviewWireBarrier(command string) *reviewWireBarrier {
	return &reviewWireBarrier{
		command: command,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *reviewWireBarrier) record(command string) {
	switch command {
	case "APPEND":
		b.appendCalls.Add(1)
	case "SELECT":
		b.selectCalls.Add(1)
	case "FETCH":
		b.fetchCalls.Add(1)
	case "STORE":
		b.storeCalls.Add(1)
	case "EXPUNGE":
		b.expungeCalls.Add(1)
	}
	if !b.armed.Load() || command != b.command || !b.blocked.CompareAndSwap(false, true) {
		return
	}
	close(b.entered)
	<-b.release
}

func (b *reviewWireBarrier) releaseNow() {
	b.releaseMu.Do(func() { close(b.release) })
}

type reviewWireBarrierSession struct {
	imapserver.Session

	barrier *reviewWireBarrier
}

type reviewDropConnectionSession struct {
	imapserver.Session

	conn    *imapserver.Conn
	dropped *atomic.Bool
}

type reviewAppendRaceContext struct {
	context.Context

	cancel    context.CancelFunc
	armed     atomic.Bool
	cancelled atomic.Bool
}

func (c *reviewAppendRaceContext) arm() {
	c.armed.Store(true)
}

func (c *reviewAppendRaceContext) Err() error {
	if c.armed.Load() && c.cancelled.CompareAndSwap(false, true) {
		c.cancel()
	}
	return c.Context.Err()
}

type reviewDropAppendSession struct {
	imapserver.Session

	conn    *imapserver.Conn
	enabled *atomic.Bool
	arm     func()
}

func (s *reviewDropAppendSession) Append(
	mailbox string,
	r emersionimap.LiteralReader,
	options *emersionimap.AppendOptions,
) (*emersionimap.AppendData, error) {
	data, err := s.Session.Append(mailbox, r, options)
	if s.enabled.Load() {
		s.arm()
		_ = s.conn.NetConn().Close()
	}
	if err != nil {
		return data, fmt.Errorf("drop append session: %w", err)
	}
	return data, nil
}

func (s *reviewDropConnectionSession) Select(
	mailbox string,
	options *emersionimap.SelectOptions,
) (*emersionimap.SelectData, error) {
	data, err := s.Session.Select(mailbox, options)
	if s.dropped.CompareAndSwap(false, true) {
		_ = s.conn.NetConn().Close()
	}
	if err != nil {
		return data, fmt.Errorf("drop connection session SELECT: %w", err)
	}
	return data, nil
}

func (s *reviewWireBarrierSession) Append(
	mailbox string,
	r emersionimap.LiteralReader,
	options *emersionimap.AppendOptions,
) (*emersionimap.AppendData, error) {
	data, err := s.Session.Append(mailbox, r, options)
	s.barrier.record("APPEND")
	if err != nil {
		return data, fmt.Errorf("wire barrier session APPEND: %w", err)
	}
	return data, nil
}

func (s *reviewWireBarrierSession) Select(
	mailbox string,
	options *emersionimap.SelectOptions,
) (*emersionimap.SelectData, error) {
	data, err := s.Session.Select(mailbox, options)
	s.barrier.record("SELECT")
	if err != nil {
		return data, fmt.Errorf("wire barrier session SELECT: %w", err)
	}
	return data, nil
}

func (s *reviewWireBarrierSession) Fetch(
	w *imapserver.FetchWriter,
	numSet emersionimap.NumSet,
	options *emersionimap.FetchOptions,
) error {
	err := s.Session.Fetch(w, numSet, options)
	s.barrier.record("FETCH")
	if err != nil {
		return fmt.Errorf("wire barrier session FETCH: %w", err)
	}
	return nil
}

func (s *reviewWireBarrierSession) Store(
	w *imapserver.FetchWriter,
	numSet emersionimap.NumSet,
	flags *emersionimap.StoreFlags,
	options *emersionimap.StoreOptions,
) error {
	err := s.Session.Store(w, numSet, flags, options)
	s.barrier.record("STORE")
	if err != nil {
		return fmt.Errorf("wire barrier session STORE: %w", err)
	}
	return nil
}

func (s *reviewWireBarrierSession) Expunge(
	w *imapserver.ExpungeWriter,
	uids *emersionimap.UIDSet,
) error {
	err := s.Session.Expunge(w, uids)
	s.barrier.record("EXPUNGE")
	if err != nil {
		return fmt.Errorf("wire barrier session EXPUNGE: %w", err)
	}
	return nil
}

func reviewCurrentConn(t *testing.T, client *Client) *imapclient.Client {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.conn
}

func reviewFreshConn(t *testing.T, client *Client) *imapclient.Client {
	t.Helper()
	var fresh *imapclient.Client
	require.NoError(t, client.withDraftConn(t.Context(), func(conn *imapclient.Client) error {
		fresh = conn
		return nil
	}))
	return fresh
}

func awaitReviewAppendResult(
	t *testing.T,
	results <-chan DraftAppendResult,
	errs <-chan error,
) (DraftAppendResult, error) {
	t.Helper()
	var result DraftAppendResult
	var err error
	select {
	case result = <-results:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "draft append did not return while the response barrier was held")
	}
	select {
	case err = <-errs:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "draft append error did not return while the response barrier was held")
	}
	return result, err
}

func awaitReviewRemovalResult(
	t *testing.T,
	results <-chan DraftObservation,
	errs <-chan error,
) (DraftObservation, error) {
	t.Helper()
	var observation DraftObservation
	var err error
	select {
	case observation = <-results:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "draft removal did not return while the response barrier was held")
	}
	select {
	case err = <-errs:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "draft removal error did not return while the response barrier was held")
	}
	return observation, err
}

type reviewStoreBarrier struct {
	stored       chan struct{}
	release      chan struct{}
	blocked      atomic.Bool
	expungeCalls atomic.Int32
	onStore      func()
}

type reviewStoreBarrierSession struct {
	imapserver.Session

	barrier *reviewStoreBarrier
}

type reviewNoopExpungeSession struct {
	imapserver.Session
}

func (s *reviewStoreBarrierSession) Store(
	w *imapserver.FetchWriter,
	numSet emersionimap.NumSet,
	flags *emersionimap.StoreFlags,
	options *emersionimap.StoreOptions,
) error {
	err := s.Session.Store(w, numSet, flags, options)
	if s.barrier.blocked.CompareAndSwap(false, true) {
		close(s.barrier.stored)
		if s.barrier.onStore != nil {
			s.barrier.onStore()
		}
		<-s.barrier.release
	}
	if err != nil {
		return fmt.Errorf("store barrier session: %w", err)
	}
	return nil
}

func (s *reviewStoreBarrierSession) Expunge(
	w *imapserver.ExpungeWriter,
	uids *emersionimap.UIDSet,
) error {
	err := s.Session.Expunge(w, uids)
	s.barrier.expungeCalls.Add(1)
	if err != nil {
		return fmt.Errorf("expunge barrier session: %w", err)
	}
	return nil
}

func (s *reviewNoopExpungeSession) Expunge(
	*imapserver.ExpungeWriter,
	*emersionimap.UIDSet,
) error {
	return nil
}

func startReviewIMAPServer(
	t *testing.T,
	caps emersionimap.CapSet,
	wrap func(imapserver.Session, *imapserver.Conn) imapserver.Session,
) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("Drafts", nil))
	memServer := imapmemserver.New()
	memServer.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		Caps:         caps,
		InsecureAuth: true,
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			if wrap != nil {
				return wrap(memServer.NewSession(), conn), nil, nil
			}
			return memServer.NewSession(), nil, nil
		},
	})
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return ln.Addr().String()
}

func reviewIMAPClient(t *testing.T, addr string) *Client {
	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func reviewStoreFlags(t *testing.T, addr string, uid uint32, op emersionimap.StoreFlagsOp) {
	t.Helper()
	reviewStoreFlag(t, addr, uid, op, emersionimap.FlagDeleted)
}

func reviewStoreFlag(t *testing.T, addr string, uid uint32, op emersionimap.StoreFlagsOp, flag emersionimap.Flag) {
	t.Helper()
	client, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	_, err = client.Select("Drafts", nil).Wait()
	require.NoError(t, err)
	var uids emersionimap.UIDSet
	uids.AddNum(emersionimap.UID(uid))
	require.NoError(t, client.Store(uids, &emersionimap.StoreFlags{
		Op: op, Flags: []emersionimap.Flag{flag},
	}, nil).Close())
}

func reviewDraftRaw(body string) []byte {
	return []byte("From: alice@example.com\r\nTo: bob@example.com\r\n\r\n" + body + "\r\n")
}
