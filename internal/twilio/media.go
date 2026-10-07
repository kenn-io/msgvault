package twilio

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"go.kenn.io/msgvault/internal/callsync"
	"go.kenn.io/msgvault/internal/export"
)

// OpenRecording returns the recording's WAV audio from the pinned Voice origin.
// It refuses a declared size over maxBytes; the caller caps the stream itself.
func (c *Client) OpenRecording(ctx context.Context, recording Recording, maxBytes int64) (io.ReadCloser, error) {
	target := c.endpoint("voice", c.accountPath()+"/Recordings/"+recording.SID+".wav", nil)
	if recording.Channels == 2 {
		target.RawQuery = url.Values{"RequestedChannels": {"2"}}.Encode()
	}
	client := callsync.MediaClient(c.http, maxBytes)
	response, err := c.get(ctx, client, "recording media", target)
	var apiErr *APIError
	if recording.Channels == 2 && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest {
		target.RawQuery = url.Values{"RequestedChannels": {"1"}}.Encode()
		response, err = c.get(ctx, client, "recording media", target)
	}
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxBytes {
		return nil, errors.Join(export.ErrAttachmentTooLarge, response.Body.Close())
	}
	buffered := bufio.NewReaderSize(response.Body, 512)
	prefix, err := buffered.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.Join(fmt.Errorf("twilio: inspect recording audio: %w", err), response.Body.Close())
	}
	if http.DetectContentType(prefix) != "audio/wave" {
		return nil, errors.Join(errors.New("twilio: recording response is not audio"), response.Body.Close())
	}
	return struct {
		io.Reader
		io.Closer
	}{buffered, response.Body}, nil
}
