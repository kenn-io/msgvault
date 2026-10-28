package beeper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"time"
)

// SendParams describes one immediate message.
type SendParams struct {
	Text             string
	ReplyToMessageID string
	UploadID         string
}

// SendResult is the destination and pending ID accepted by Beeper.
type SendResult struct {
	ChatID           string `json:"chatID"`
	PendingMessageID string `json:"pendingMessageID"`
}

// SendMessage posts once, returning the provider receipt.
func (c *Client) SendMessage(ctx context.Context, chatID string, params SendParams) (SendResult, error) {
	body := struct {
		Text             string `json:"text,omitempty"`
		ReplyToMessageID string `json:"replyToMessageID,omitempty"`
		Attachment       *struct {
			UploadID string `json:"uploadID"`
		} `json:"attachment,omitempty"`
	}{Text: params.Text, ReplyToMessageID: params.ReplyToMessageID}
	if params.UploadID != "" {
		body.Attachment = &struct {
			UploadID string `json:"uploadID"`
		}{params.UploadID}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return SendResult{}, err
	}
	data, err := c.writeOnce(ctx, http.MethodPost, "/v1/chats/"+url.PathEscape(chatID)+"/messages", "application/json", bytes.NewReader(encoded), c.http.Timeout)
	if err != nil {
		return SendResult{}, err
	}
	var response SendResult
	if err := json.Unmarshal(data, &response); err != nil {
		return SendResult{}, &WriteError{Err: err}
	}
	if response.ChatID == "" || response.PendingMessageID == "" {
		return SendResult{}, &WriteError{Err: errors.New("missing chat or pending message ID")}
	}
	return response, nil
}

// UploadAsset streams one file to the Desktop API without retrying.
func (c *Client) UploadAsset(ctx context.Context, file io.Reader, fileName string) (string, error) {
	mimeType := mime.TypeByExtension(filepath.Ext(fileName))
	if mimeType == "" {
		buffered := bufio.NewReader(file)
		prefix, err := buffered.Peek(512)
		if err != nil && err != io.EOF {
			return "", err
		}
		mimeType = http.DetectContentType(prefix)
		file = buffered
	}
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": fileName}))
		header.Set("Content-Type", mimeType)
		part, err := multipartWriter.CreatePart(header)
		if err == nil {
			_, err = io.Copy(part, file)
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	data, err := c.writeOnce(ctx, http.MethodPost, "/v1/assets/upload", multipartWriter.FormDataContentType(), reader, 0)
	_ = reader.CloseWithError(err)
	uploadErr := <-done
	if err != nil {
		return "", err
	}
	if uploadErr != nil {
		return "", uploadErr
	}
	var response struct {
		UploadID string `json:"uploadID"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return "", &WriteError{Err: err}
	}
	if response.UploadID == "" {
		return "", &WriteError{Err: errors.New("upload did not return an asset")}
	}
	return response.UploadID, nil
}

// ErrArchivedMessageMismatch reports a live message whose identity or timestamp differs from the archive.
var ErrArchivedMessageMismatch = errors.New("archived message identity or timestamp mismatch")

// VerifyArchivedMessage checks that an archived reply still identifies the same message.
func (c *Client) VerifyArchivedMessage(ctx context.Context, chatID, messageID string, sentAt time.Time) error {
	message, err := c.GetMessage(ctx, chatID, messageID)
	if err != nil {
		return err
	}
	delta := message.Timestamp.Sub(sentAt)
	if message.ID != messageID || message.ChatID != chatID || delta < -archivedSampleTolerance || delta > archivedSampleTolerance {
		return fmt.Errorf("archived reply target no longer matches Beeper: %w", ErrArchivedMessageMismatch)
	}
	return nil
}
