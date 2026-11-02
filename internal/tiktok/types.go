// Package tiktok archives read-only TikTok API for Business sources.
package tiktok

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"
)

const (
	CommentsSourceType = "tiktok_comments"
	DefaultMaxRequests = 1000
	commentFormat      = "tiktok-comment-json"
)

// ID accepts native string or integer identifiers without a float64 conversion.
type ID string

// Integer accepts the API's quoted and unquoted timestamp representations.
type Integer int64

func (n *Integer) UnmarshalJSON(data []byte) error {
	var value string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	} else {
		value = string(data)
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return errors.New("invalid TikTok integer")
	}
	*n = Integer(parsed)
	return nil
}

func (id *ID) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		*id = ID(value)
		return nil
	}
	if len(data) == 0 || bytes.IndexFunc(data, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return errors.New("TikTok ID must be a string or unsigned integer")
	}
	*id = ID(data)
	return nil
}

func ValidID(value string) bool {
	return value != "" && len(value) <= 1024 && !strings.ContainsAny(value, "\x00\r\n\t ")
}

type Account struct {
	ID                ID     `json:"business_id,omitempty"`
	Username          string `json:"username,omitempty"`
	DisplayName       string `json:"display_name,omitempty"`
	HasAdvancedAccess bool   `json:"has_advanced_access,omitempty"`
}

type Video struct {
	ID         ID                        `json:"item_id"`
	Caption    string                    `json:"caption,omitempty"`
	CreateTime Integer                   `json:"create_time,omitempty"`
	ShareURL   string                    `json:"share_url,omitempty"`
	Extra      map[string]jsontext.Value `json:",unknown"`
}

type Comment struct {
	ID          ID                        `json:"comment_id"`
	VideoID     ID                        `json:"video_id,omitempty"`
	ParentID    ID                        `json:"parent_comment_id,omitempty"`
	ActorID     ID                        `json:"unique_identifier,omitempty"`
	Username    string                    `json:"username,omitempty"`
	DisplayName string                    `json:"display_name,omitempty"`
	Text        string                    `json:"text,omitempty"`
	CreateTime  Integer                   `json:"create_time,omitempty"`
	Replies     int                       `json:"replies,omitempty"`
	Status      string                    `json:"status,omitempty"`
	Owner       bool                      `json:"owner,omitempty"`
	ImageURL    string                    `json:"image_url,omitempty"`
	Extra       map[string]jsontext.Value `json:",unknown"`
}

type VideoPage struct {
	Videos  []Video `json:"videos"`
	Cursor  ID      `json:"cursor,omitempty"`
	HasMore bool    `json:"has_more"`
}
type CommentPage struct {
	Comments []Comment `json:"comments"`
	Cursor   ID        `json:"cursor,omitempty"`
	HasMore  bool      `json:"has_more"`
}
