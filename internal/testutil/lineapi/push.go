package lineapi

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type pushRecord struct {
	firstAt   time.Time
	body      []byte
	receipt   string
	requestID string
	accepted  bool
}

type pushState struct {
	now      func() time.Time
	records  map[string]*pushRecord
	accepted int
}

func newPushState() *pushState {
	return &pushState{now: time.Now, records: make(map[string]*pushRecord)}
}

// SetClock models LINE's 24-hour first-request retry window.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.pushes.now = now
	}
}

func (s *Server) AcceptedPushes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pushes.accepted
}

// Called under Server.mu. Overrides can model an accepted request followed by
// 5xx/disconnection, while the provider retains the first receipt for recovery.
func (s *Server) pushResponse(key string, body []byte, override *Response) Response {
	var input struct {
		To       string `json:"to"`
		Messages []struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			QuoteToken string `json:"quoteToken"`
		} `json:"messages"`
	}
	if len(key) != 36 || uuid.Validate(key) != nil || json.Unmarshal(body, &input) != nil || input.To == "" || len(input.Messages) != 1 || input.Messages[0].Type != "text" || input.Messages[0].Text == "" {
		return Response{Status: 400, Body: `{"message":"invalid synthetic push"}`}
	}
	now := s.pushes.now()
	record := s.pushes.records[key]
	if record != nil && !now.Before(record.firstAt.Add(24*time.Hour)) {
		delete(s.pushes.records, key)
		record = nil
	}
	if record != nil && !bytes.Equal(record.body, body) {
		return Response{Status: 400, Body: `{"message":"retry payload changed"}`}
	}
	if record == nil {
		record = &pushRecord{firstAt: now, body: append([]byte(nil), body...)}
		s.pushes.records[key] = record
	}
	if override == nil && record.accepted {
		return Response{Status: 409, Header: http.Header{
			"X-Line-Accepted-Request-Id": {record.requestID},
			"X-Line-Request-Id":          {fmt.Sprintf("synthetic-retry-%d", len(s.requests))},
		}, Body: record.receipt}
	}
	response := Response{Status: 200}
	if override != nil {
		response = *override
		response.Header = response.Header.Clone()
	}
	if response.Status == 0 {
		response.Status = 200
	}
	acceptedConflict := response.Status == 409 && strings.TrimSpace(response.Header.Get("X-Line-Accepted-Request-Id")) != ""
	accepted := response.Accept || response.Status >= 200 && response.Status < 300 || acceptedConflict
	if accepted && !record.accepted {
		s.pushes.accepted++
		record.accepted = true
		record.requestID = fmt.Sprintf("synthetic-request-%d", s.pushes.accepted)
		if header := response.Header.Get("X-Line-Request-Id"); header != "" {
			record.requestID = header
		}
		if acceptedConflict {
			record.requestID = response.Header.Get("X-Line-Accepted-Request-Id")
		}
		record.receipt = fmt.Sprintf(`{"sentMessages":[{"id":"9%017d","quoteToken":"synthetic-sent-quote-%d"}]}`, s.pushes.accepted, s.pushes.accepted)
		// Preserve a valid explicitly returned ID as the actual accepted receipt.
		var explicit struct {
			SentMessages []struct {
				ID string `json:"id"`
			} `json:"sentMessages"`
		}
		if json.Unmarshal([]byte(response.Body), &explicit) == nil && len(explicit.SentMessages) == 1 && strings.TrimSpace(explicit.SentMessages[0].ID) != "" {
			record.receipt = response.Body
		}
	}
	if override == nil {
		response.Body = record.receipt
	}
	if accepted && response.Header.Get("X-Line-Request-Id") == "" {
		if response.Header == nil {
			response.Header = make(http.Header)
		}
		response.Header.Set("X-Line-Request-Id", record.requestID)
	}
	return response
}
