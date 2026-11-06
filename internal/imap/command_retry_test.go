package imap

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunCommandRetryFloorSurvivesRetryExhaustion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var attempts atomic.Int32
	_, err := runCommand(t.Context(), &Client{logger: slog.New(slog.DiscardHandler)}, func() (struct{}, error) {
		attempts.Add(1)
		return struct{}{}, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "retry in 1 second"}
	})
	require.Error(err)
	assert.EqualValues(4, attempts.Load(), "one command keeps its existing bounded retry budget")
	var limited interface{ RetryNotBefore() time.Time }
	require.ErrorAs(err, &limited)
	assert.True(limited.RetryNotBefore().After(time.Now()), "the final server retry hint remains live on terminal failure")
}

func TestRunCommandCancellationAfterLimitResponseRetainsRetryFloor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var limited atomic.Int32
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{
		capabilities: []string{"IMAP4rev1"},
		commandHook: func(conn net.Conn, _ int, line string) bool {
			tag, command, _ := strings.Cut(line, " ")
			if !strings.HasPrefix(command, "LIST") {
				return false
			}
			limited.Add(1)
			_, _ = fmt.Fprintf(conn, "%s NO [LIMIT] retry in 60 seconds\r\n", tag)
			return true
		},
	})
	client := newTestClient(t, addr)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	passCtx, capture := WithRetryFloorCapture(ctx)
	var callbackErr error
	err := client.withConn(passCtx, func(conn *imapclient.Client) error {
		_, err := runCommand(passCtx, client, func() ([]*imap.ListData, error) {
			result, err := conn.List("", "*", nil).Collect()
			if err != nil {
				err = fmt.Errorf("collect IMAP LIST: %w", err)
			}
			callbackErr = err
			cancel()
			return result, err
		})
		return err
	})
	var response *imap.Error
	require.ErrorAs(callbackErr, &response, "the server response is a typed IMAP LIMIT")
	assert.Equal(imap.ResponseCodeLimit, response.Code)
	require.ErrorIs(err, context.Canceled)
	var floor interface{ RetryNotBefore() time.Time }
	require.ErrorAs(err, &floor)
	assert.True(floor.RetryNotBefore().After(time.Now()), "cancellation keeps the deadline from the completed LIMIT response")
	assert.True(capture.RetryNotBefore().After(time.Now()), "the acquisition capture retains the typed response deadline")
	assert.EqualValues(1, limited.Load())
}

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

func TestLimitedQresyncPreservesVanished(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var attempts atomic.Int32
	count := uint32(2)
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{
		capabilities: []string{"IMAP4rev1 ENABLE CONDSTORE QRESYNC"}, uidValidity: 1, uidNext: 5, highestModSeq: 6, selectHighestModSeq: 5, numMessages: &count,
		searchUIDs: []imap.UID{3, 4}, fetchChanged: []imap.UID{3}, fetchVanished: []imap.UID{2},
		commandHook: func(conn net.Conn, _ int, line string) bool {
			tag, cmd, _ := strings.Cut(line, " ")
			if !strings.HasPrefix(cmd, "UID FETCH") || !strings.Contains(cmd, "CHANGEDSINCE") {
				return false
			}
			if attempts.Add(1) != 1 {
				// UID 4 was expunged and UID 3 changed after the rejected FETCH.
				_, _ = fmt.Fprint(conn, "* VANISHED 4\r\n")
				return false
			}
			// UID 4's partial flags belong only to the rejected attempt. VANISHED
			// 1 remains true even though the succeeding command reports 2 and 4.
			_, _ = fmt.Fprintf(conn, "* VANISHED (EARLIER) 1\r\n* 2 FETCH (UID 4 FLAGS (\\Flagged) MODSEQ (5))\r\n%s NO [LIMIT] retry in 0 seconds\r\n", tag)
			return true
		},
	})
	client := newTestClient(t, addr)
	require.NoError(client.withConn(t.Context(), func(*imapclient.Client) error {
		require.True(client.enableQresync(t.Context()))
		_, err := client.selectCommand(t.Context(), "INBOX", &imap.SelectOptions{CondStore: true})
		require.NoError(err)
		client.beginQresyncCapture()
		defer client.clearQresyncCapture()
		_, err = client.fetchCommand(t.Context(), imap.UIDSetNum(1, 2, 3, 4), &imap.FetchOptions{
			UID: true, Flags: true, ModSeq: true, ChangedSince: 2, Vanished: true,
		})
		require.NoError(err)
		assert.ElementsMatch([]imap.UID{1, 2, 4}, client.snapshotQresyncCapture().vanished)
		return nil
	}))
}
