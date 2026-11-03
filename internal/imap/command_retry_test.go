package imap

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The real IMAP parser must deliver typed errors to each production read path.
func TestLimitedSyncCommands(t *testing.T) {
	for _, command := range []string{"LIST", "STATUS", "SELECT", "UID SEARCH", "UID FETCH", "ENABLE"} {
		t.Run(command, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var attempts atomic.Int32
			count := uint32(1)
			addr, _ := startQresyncTestServer(t, qresyncServerConfig{
				capabilities: []string{"IMAP4rev1 ENABLE CONDSTORE QRESYNC"},
				uidValidity:  1, uidNext: 2, highestModSeq: 2, numMessages: &count, searchUIDs: []imap.UID{1},
				commandHook: func(conn net.Conn, _ int, line string) bool {
					tag, cmd, _ := strings.Cut(line, " ")
					if !strings.HasPrefix(cmd, command) {
						return false
					}
					if attempts.Add(1) != 1 {
						return false
					}
					_, _ = fmt.Fprintf(conn, "%s NO [LIMIT] retry in 0 seconds\r\n", tag)
					return true
				},
			})
			client := newTestClient(t, addr)
			ctx := t.Context()
			switch command {
			case "LIST":
				labels, err := client.ListLabels(ctx)
				require.NoError(err)
				require.Len(labels, 1)
				assert.Equal("INBOX", labels[0].Name)
			case "STATUS":
				profile, err := client.GetProfile(ctx)
				require.NoError(err)
				assert.Equal(int64(1), profile.MessagesTotal)
			default:
				err := client.withConn(ctx, func(*imapclient.Client) error {
					if command == "ENABLE" {
						assert.True(client.enableQresync(t.Context()))
						return nil
					}
					uids, err := client.enumerateMailbox(ctx, "INBOX", 0)
					if err != nil {
						return err
					}
					assert.Equal([]imap.UID{1}, uids)
					if command == "UID FETCH" {
						msgs, fatal, err := client.fetchChunk(ctx, "INBOX", imap.UIDSetNum(1), &imap.FetchOptions{UID: true, Flags: true})
						assert.False(fatal)
						if err != nil {
							return err
						}
						require.Len(msgs, 1)
						assert.Equal(imap.UID(1), msgs[0].UID)
					}
					return nil
				})
				require.NoError(err)
			}
			assert.Equal(int32(2), attempts.Load())
		})
	}
}

func TestLimitedCommandExhaustion(t *testing.T) {
	for _, tt := range []struct {
		name, response string
		attempts       int32
	}{
		{"limit", "NO [LIMIT] retry in 0 seconds", 4},
		{"hint only", "NO retry in 0 seconds", 4},
		{"ordinary no", "NO permission denied", 1},
		{"bad limit", "BAD [LIMIT] retry in 0 seconds", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var attempts atomic.Int32
			addr, _ := startQresyncTestServer(t, qresyncServerConfig{capabilities: []string{"IMAP4rev1"}, commandHook: func(conn net.Conn, _ int, line string) bool {
				tag, cmd, _ := strings.Cut(line, " ")
				if !strings.HasPrefix(cmd, "LIST") {
					return false
				}
				attempts.Add(1)
				_, _ = fmt.Fprintf(conn, "%s %s\r\n", tag, tt.response)
				return true
			}})
			labels, err := newTestClient(t, addr).ListLabels(t.Context())
			require.Error(err)
			assert.Nil(labels)
			var response *imap.Error
			require.ErrorAs(err, &response)
			assert.Equal(tt.attempts, attempts.Load())
		})
	}
}

func TestLimitedCommandWaitAndCancellation(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelWait), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var attempts atomic.Int32
			responses := make(chan time.Time, 2)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			addr, _ := startQresyncTestServer(t, qresyncServerConfig{capabilities: []string{"IMAP4rev1"}, commandHook: func(conn net.Conn, _ int, line string) bool {
				tag, cmd, _ := strings.Cut(line, " ")
				if !strings.HasPrefix(cmd, "LIST") {
					return false
				}
				n := attempts.Add(1)
				// The client can start its retry timer as soon as NO arrives.
				responded := time.Now()
				if n == 1 {
					_, _ = fmt.Fprintf(conn, "%s NO [LIMIT] retry in 1 second\r\n", tag)
				}
				responses <- responded
				return n == 1
			}})
			client := newTestClient(t, addr)
			done := make(chan error, 1)
			go func() { _, err := client.ListLabels(ctx); done <- err }()
			first := <-responses
			if cancelWait {
				cancel()
			}
			select {
			case err := <-done:
				if cancelWait {
					require.ErrorIs(err, context.Canceled)
					assert.Equal(int32(1), attempts.Load())
				} else {
					require.NoError(err)
					second := <-responses
					assert.GreaterOrEqual(second.Sub(first), time.Second)
				}
			case <-time.After(5 * time.Second):
				require.FailNow("command did not finish after response/cancellation")
			}
		})
	}
}

func TestFailedSelectClearsPreviousMailbox(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var selected atomic.Int32
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{capabilities: []string{"IMAP4rev1"}, uidValidity: 1, uidNext: 2, searchUIDs: []imap.UID{1}, commandHook: func(conn net.Conn, _ int, line string) bool {
		tag, cmd, _ := strings.Cut(line, " ")
		if !strings.HasPrefix(cmd, "SELECT") {
			return false
		}
		selected.Add(1)
		if strings.Contains(cmd, "Missing") {
			_, _ = fmt.Fprintf(conn, "%s NO no such mailbox\r\n", tag)
			return true
		}
		return false
	}})
	client := newTestClient(t, addr)
	require.NoError(client.withConn(t.Context(), func(*imapclient.Client) error {
		require.NoError(client.selectMailbox(t.Context(), "INBOX"))
		require.Error(client.selectMailbox(t.Context(), "Missing"))
		assert.Empty(client.selectedMailbox)
		assert.Zero(client.selectedUIDValidity)
		assert.Zero(client.selectedNumMessages)
		return client.selectMailbox(t.Context(), "INBOX")
	}))
	assert.Equal(int32(3), selected.Load(), "the old mailbox must be selected again after a failed SELECT")
}

func TestCommandLimiterPacesRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var attempts atomic.Int32
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{capabilities: []string{"IMAP4rev1"}, commandHook: func(conn net.Conn, _ int, line string) bool {
		tag, cmd, _ := strings.Cut(line, " ")
		if !strings.HasPrefix(cmd, "LIST") {
			return false
		}
		if attempts.Add(1) != 1 {
			return false
		}
		_, _ = fmt.Fprintf(conn, "%s NO [LIMIT] retry in 0 seconds\r\n", tag)
		return true
	}})
	client := newTestClient(t, addr, WithMaxCommandsPerMinute(600))
	start := time.Now()
	labels, err := client.ListLabels(t.Context())
	require.NoError(err)
	require.Len(labels, 1)
	assert.Equal(int32(2), attempts.Load())
	// LOGIN consumes the initial token; two LIST attempts each need another.
	assert.GreaterOrEqual(time.Since(start), 200*time.Millisecond)
}

func TestCommandLimiterCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{capabilities: []string{"IMAP4rev1"}})
	client := newTestClient(t, addr, WithMaxCommandsPerMinute(1))
	require.NoError(connectRetryClient(t.Context(), t, client))
	require.NotNil(client.commandLimiter)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.ListLabels(ctx); done <- err }()
	require.Eventually(func() bool { return client.commandLimiter.Tokens() < 0 }, 5*time.Second, time.Millisecond, "wait until LIST reserves a future command slot")
	cancel()
	require.ErrorIs(<-done, context.Canceled)
	for _, line := range server.commandsFor(1) {
		assert.NotContains(line, " LIST ")
	}
	assert.Greater(client.commandLimiter.Tokens(), float64(-0.1), "cancellation restores the future slot")
}

func TestCommandLimiterSurvivesReconnect(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{capabilities: []string{"IMAP4rev1"}})
	client := newTestClient(t, addr, WithMaxCommandsPerMinute(600))
	start := time.Now()
	require.NoError(connectRetryClient(t.Context(), t, client))
	client.mu.Lock()
	err := client.reconnect(t.Context())
	client.mu.Unlock()
	require.NoError(err)
	assert.GreaterOrEqual(time.Since(start), 100*time.Millisecond, "replacement LOGIN must share the original client's command budget")
}

func TestLimitedFetchReconnectBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var attempts atomic.Int32
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{capabilities: []string{"IMAP4rev1"}, uidValidity: 1, uidNext: 2, searchUIDs: []imap.UID{1}, commandHook: func(conn net.Conn, _ int, line string) bool {
		tag, cmd, _ := strings.Cut(line, " ")
		if !strings.HasPrefix(cmd, "UID FETCH") {
			return false
		}
		if attempts.Add(1) == 4 {
			_ = conn.Close()
			return true
		}
		_, _ = fmt.Fprintf(conn, "%s NO [LIMIT] retry in 0 seconds\r\n", tag)
		return true
	}})
	client := newTestClient(t, addr)
	require.NoError(client.withConn(t.Context(), func(*imapclient.Client) error {
		require.NoError(client.selectMailbox(t.Context(), "INBOX"))
		msgs, fatal, err := client.fetchChunk(t.Context(), "INBOX", imap.UIDSetNum(1), &imap.FetchOptions{UID: true})
		require.Error(err)
		assert.Nil(msgs)
		assert.False(fatal)
		assert.Equal(int32(8), attempts.Load(), "one reconnect starts one fresh four-attempt budget")
		return nil
	}))
}

func TestLimitedWriteIsNotRetried(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var writes atomic.Int32
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{capabilities: []string{"IMAP4rev1 UIDPLUS"}, uidValidity: 1, uidNext: 2, searchUIDs: []imap.UID{1}, commandHook: func(conn net.Conn, _ int, line string) bool {
		tag, cmd, _ := strings.Cut(line, " ")
		if !strings.HasPrefix(cmd, "UID STORE") {
			return false
		}
		writes.Add(1)
		_, _ = fmt.Fprintf(conn, "%s NO [LIMIT] retry in 0 seconds\r\n", tag)
		return true
	}})
	err := newTestClient(t, addr).DeleteMessage(t.Context(), "INBOX|1")
	require.Error(err)
	assert.Equal(int32(1), writes.Load())
}

func TestLimitedQresyncPreservesVanishedAndDiscardsPartialFetch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var attempts atomic.Int32
	count := uint32(2)
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{
		capabilities: []string{"IMAP4rev1 ENABLE CONDSTORE QRESYNC"}, uidValidity: 1, uidNext: 5, highestModSeq: 5, numMessages: &count,
		searchUIDs: []imap.UID{3, 4}, fetchChanged: []imap.UID{3}, fetchVanished: []imap.UID{2}, fetchModSeq: map[imap.UID]uint64{3: 5, 4: 1},
		commandHook: func(conn net.Conn, _ int, line string) bool {
			tag, cmd, _ := strings.Cut(line, " ")
			if !strings.HasPrefix(cmd, "UID FETCH") || !strings.Contains(cmd, "CHANGEDSINCE") {
				return false
			}
			if attempts.Add(1) != 1 {
				return false
			}
			// UID 4's partial flags belong only to the rejected attempt. VANISHED
			// 1 remains true even though the succeeding command reports only 2.
			_, _ = fmt.Fprintf(conn, "* VANISHED (EARLIER) 1\r\n* 1 FETCH (UID 4 FLAGS (\\Flagged) MODSEQ (5))\r\n%s NO [LIMIT] retry in 0 seconds\r\n", tag)
			return true
		},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{"INBOX": {UIDValidity: 1, UIDNext: 5, HighestModSeq: 2, KnownUIDs: []uint32{1, 2, 3, 4}}})
	response, err := client.ListMessages(t.Context(), "", "")
	require.NoError(err)
	require.Len(response.Messages, 1)
	assert.Equal("INBOX|3", response.Messages[0].ID)
	deltas := client.ObservedMailboxDeltas()
	require.Len(deltas, 1)
	assert.Equal([]imap.UID{1, 2}, deltas[0].VanishedUIDs)
	assert.Equal([]imap.UID{3}, deltas[0].ChangedUIDs)
	assert.Equal([]uint32{3, 4}, deltas[0].State.KnownUIDs)
	assert.Nil(client.qresyncCapture, "capture cannot leak into another mailbox or fallback")
}

func TestLimitedLabelFetchDiscardsRejectedMembership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var attempts atomic.Int32
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{capabilities: []string{"IMAP4rev1"}, uidValidity: 1, uidNext: 2, searchUIDs: []imap.UID{1}, commandHook: func(conn net.Conn, _ int, line string) bool {
		tag, cmd, _ := strings.Cut(line, " ")
		if !strings.HasPrefix(cmd, "UID FETCH") {
			return false
		}
		n := attempts.Add(1)
		messageID := "final@example.test"
		if n == 1 {
			messageID = "rejected@example.test"
		}
		header := fmt.Sprintf("Message-ID: <%s>\r\n\r\n", messageID)
		_, _ = fmt.Fprintf(conn, "* 1 FETCH (UID 1 FLAGS () BODY[HEADER.FIELDS (\"Message-ID\")] {%d}\r\n%s)\r\n", len(header), header)
		if n == 1 {
			_, _ = fmt.Fprintf(conn, "%s NO [LIMIT] retry in 0 seconds\r\n", tag)
		} else {
			_, _ = fmt.Fprintf(conn, "%s OK FETCH done\r\n", tag)
		}
		return true
	}})
	client := newTestClient(t, addr)
	results, err := client.GetMessageLabelsBatch(t.Context(), []string{"INBOX|1"})
	require.NoError(err)
	require.Len(results, 1)
	require.NoError(results[0].Err)
	assert.Equal("final@example.test", results[0].RFC822MessageID)
	assert.Equal([]string{"INBOX"}, results[0].LabelIDs)
	observed := client.ObservedMemberships()
	require.Len(observed, 1)
	assert.Equal("final@example.test", observed[0].RFC822MessageID)
	assert.Equal(int32(2), attempts.Load())
}
