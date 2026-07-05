package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
)

type legacyRawBatchClient struct {
	rawMessages []*gmail.RawMessage
	rawErr      error
}

func (c *legacyRawBatchClient) GetProfile(context.Context) (*gmail.Profile, error) {
	return &gmail.Profile{EmailAddress: testEmail}, nil
}

func (c *legacyRawBatchClient) ListLabels(context.Context) ([]*gmail.Label, error) {
	return nil, nil
}

func (c *legacyRawBatchClient) ListMessages(context.Context, string, string) (*gmail.MessageListResponse, error) {
	return &gmail.MessageListResponse{}, nil
}

func (c *legacyRawBatchClient) GetMessageRaw(context.Context, string) (*gmail.RawMessage, error) {
	return &gmail.RawMessage{}, nil
}

func (c *legacyRawBatchClient) GetMessagesRawBatch(context.Context, []string) ([]*gmail.RawMessage, error) {
	return c.rawMessages, c.rawErr
}

func (c *legacyRawBatchClient) ListHistory(context.Context, uint64, string) (*gmail.HistoryResponse, error) {
	return &gmail.HistoryResponse{}, nil
}

func (c *legacyRawBatchClient) TrashMessage(context.Context, string) error {
	return nil
}

func (c *legacyRawBatchClient) DeleteMessage(context.Context, string) error {
	return nil
}

func (c *legacyRawBatchClient) BatchDeleteMessages(context.Context, []string) error {
	return nil
}

func (c *legacyRawBatchClient) Close() error {
	return nil
}

var _ gmail.API = (*legacyRawBatchClient)(nil)

type diagnosticRawBatchClient struct {
	legacyRawBatchClient

	results []gmail.RawMessageBatchResult
	err     error
}

func (c *diagnosticRawBatchClient) GetMessagesRawBatchWithErrors(context.Context, []string) ([]gmail.RawMessageBatchResult, error) {
	return c.results, c.err
}

func TestGetMessagesRawBatchWithDiagnosticsUsesMissingRawForLegacyNil(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	client := &legacyRawBatchClient{
		rawMessages: []*gmail.RawMessage{nil},
	}
	_, hasDiagnostics := any(client).(rawBatchWithErrors)
	require.False(hasDiagnostics)
	syncer := New(client, nil, nil)

	results, err := syncer.getMessagesRawBatchWithDiagnostics(context.Background(), []string{"Archive|10"})

	require.NoError(err)
	require.Len(results, 1)
	assert.Equal("Archive|10", results[0].ID)
	assert.Nil(results[0].Message)
	require.ErrorIs(results[0].Err, errRawBatchMissing)
}

func TestGetMessagesRawBatchWithDiagnosticsPreservesClientError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	errFetch := errors.New("fetch failed")
	client := &diagnosticRawBatchClient{
		results: []gmail.RawMessageBatchResult{
			{ID: "Archive|10", Err: errFetch},
		},
	}
	syncer := New(client, nil, nil)

	results, err := syncer.getMessagesRawBatchWithDiagnostics(context.Background(), []string{"Archive|10"})

	require.NoError(err)
	require.Len(results, 1)
	assert.Equal("Archive|10", results[0].ID)
	assert.Nil(results[0].Message)
	require.ErrorIs(results[0].Err, errFetch)
}
