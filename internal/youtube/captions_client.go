package youtube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
)

func (c *Client) ListCaptionTracks(ctx context.Context, videoID string) ([]CaptionTrack, error) {
	if !validID(videoID) {
		return nil, errors.New("invalid YouTube video ID")
	}
	var result page[CaptionTrack]
	if err := c.get(ctx, "/captions", url.Values{"part": {"snippet"}, "videoId": {videoID}}, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

func (c *Client) DownloadCaption(ctx context.Context, trackID string) ([]byte, error) {
	if !validID(trackID) {
		return nil, errors.New("invalid YouTube caption ID")
	}
	resp, err := c.request(ctx, http.MethodGet, "/captions/"+trackID, url.Values{"tfmt": {"vtt"}}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes+1))
	if err != nil {
		return nil, errors.New("read YouTube caption failed")
	}
	if len(data) > maxAPIBytes {
		return nil, ErrInvalidCaption
	}
	return data, nil
}
