// Package pocket archives personal recordings through Pocket's official APIs.
package pocket

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	SourceType         = "pocket"
	DefaultBaseURL     = "https://public.heypocketai.com/api/v1"
	DefaultMCPEndpoint = "https://public.heypocketai.com/mcp"
	maxEvidenceBytes   = 64 << 20
)

var ErrContract = errors.New("pocket response contract error")

type Account struct {
	Email  string `json:"email"`
	UserID string `json:"userId"`
}

type Recording struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	RecordedAt string   `json:"recording_at"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	Duration   *float64 `json:"duration"`
	Owner      *struct {
		Email  string `json:"email"`
		UserID string `json:"user_id"`
		Name   string `json:"display_name"`
	} `json:"recorded_by"`
	StartedAt time.Time      `json:"-"`
	Raw       jsontext.Value `json:"-"`
}

type Page struct {
	Recordings  []Recording
	HasMore     bool
	Page, Total int
}

type Source interface {
	CurrentAccount(ctx context.Context) (Account, error)
	ListRecordings(ctx context.Context, page int) (Page, error)
	Recording(ctx context.Context, id string) (Recording, error)
}

func DecodeRecording(raw []byte) (Recording, error) {
	var rec Recording
	if len(raw) > maxEvidenceBytes || json.Unmarshal(raw, &rec) != nil || strings.TrimSpace(rec.ID) == "" {
		return rec, fmt.Errorf("%w: invalid recording metadata", ErrContract)
	}
	for _, value := range []string{rec.RecordedAt, rec.CreatedAt, rec.UpdatedAt} {
		if value != "" {
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				return rec, fmt.Errorf("%w: invalid recording timestamp", ErrContract)
			}
		}
	}
	if rec.Duration != nil && (*rec.Duration < 0 || math.IsNaN(*rec.Duration) || math.IsInf(*rec.Duration, 0)) {
		return rec, fmt.Errorf("%w: invalid duration", ErrContract)
	}
	date := rec.RecordedAt
	if date == "" {
		date = rec.CreatedAt
	}
	if date != "" {
		rec.StartedAt, _ = time.Parse(time.RFC3339Nano, date)
	}
	rec.Raw = jsontext.Value(raw).Clone()
	if err := rec.Raw.Canonicalize(); err != nil {
		return rec, fmt.Errorf("%w: invalid recording JSON", ErrContract)
	}
	return rec, nil
}
