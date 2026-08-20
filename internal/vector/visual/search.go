package visual

import (
	"go.kenn.io/docbank/document/media"

	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
)

const MaxQueryImageBytes int64 = 20 << 20

var (
	ErrSearchNotReady = errors.New("visual attachment search is not ready")
	ErrInvalidQuery   = errors.New("visual attachment search requires exactly one of text or image")
	ErrInvalidCursor  = errors.New("visual attachment search cursor does not match the active query")
)

type SearchQuery struct {
	Text       string
	Image      *MediaInput
	Limit      int
	Cursor     string
	Person     *personscope.Scope
	SourceID   int64
	MessageID  int64
	Filename   string
	MIMEPrefix string
	After      *time.Time
	Before     *time.Time
}

type AttachmentSearchResult struct {
	AttachmentID     int64                   `json:"attachment_id"`
	MessageID        int64                   `json:"message_id"`
	ConversationID   int64                   `json:"conversation_id"`
	SourceID         int64                   `json:"source_id"`
	SourceMessageID  string                  `json:"source_message_id"`
	BlobHash         string                  `json:"blob_hash"`
	Filename         string                  `json:"filename"`
	MIMEType         string                  `json:"mime_type"`
	Size             int64                   `json:"size"`
	SentAt           time.Time               `json:"sent_at"`
	Score            float64                 `json:"score"`
	Rank             int                     `json:"rank"`
	PersonProvenance *personscope.Provenance `json:"person_provenance,omitempty"`
}

type SearchResponse struct {
	GenerationID int64                    `json:"generation_id"`
	Model        string                   `json:"model"`
	QueryMode    string                   `json:"query_mode"`
	Results      []AttachmentSearchResult `json:"results"`
	Usage        Usage                    `json:"usage"`
	NextCursor   string                   `json:"next_cursor,omitempty"`
}

type searchCursor struct {
	GenerationID int64   `json:"generation_id"`
	QueryHash    string  `json:"query_hash"`
	Score        float64 `json:"score"`
	Token        string  `json:"token"`
}

type searchQueryHashPayload struct {
	Text       string             `json:"text"`
	ImageHash  string             `json:"image_hash"`
	Filename   string             `json:"filename"`
	MIMEPrefix string             `json:"mime_prefix"`
	Person     *personscope.Scope `json:"person"`
	SourceID   int64              `json:"source_id"`
	MessageID  int64              `json:"message_id"`
	After      *time.Time         `json:"after"`
	Before     *time.Time         `json:"before"`
}

type SearchService struct {
	archive           *store.Store
	provider          Provider
	backend           Backend
	allowImageQueries bool
}

// DecodeQueryImage validates uploaded bytes with the same detection and
// pixel ceiling used for indexed media, without persisting the query. Still
// images only; whether the format's query capability was probed is enforced
// by the provider when the query is embedded.
func DecodeQueryImage(data []byte) (*MediaInput, error) {
	if len(data) == 0 || int64(len(data)) > MaxQueryImageBytes {
		return nil, errors.New("invalid visual query image size")
	}
	metadata, reason := media.InspectBytes(data, "", media.Policy{
		MaxBytes: MaxQueryImageBytes, MaxPixels: defaultMaxPixels, AllowStill: true,
	})
	if reason != media.ReasonEligible || metadata.Kind != media.KindImage || metadata.Animated {
		return nil, errors.New("unsupported visual query image")
	}
	input := mediaInputFrom(metadata)
	input.Bytes = data
	return input, nil
}

func NewSearchService(archive *store.Store, provider Provider, backend Backend, allowImageQueries bool) (*SearchService, error) {
	if archive == nil || provider == nil || backend == nil {
		return nil, errors.New("visual search requires archive, provider, and backend")
	}
	return &SearchService{archive: archive, provider: provider, backend: backend, allowImageQueries: allowImageQueries}, nil
}

func (s *SearchService) Search(ctx context.Context, query SearchQuery) (SearchResponse, error) {
	textSet := strings.TrimSpace(query.Text) != ""
	imageSet := query.Image != nil
	if textSet == imageSet {
		return SearchResponse{}, ErrInvalidQuery
	}
	if imageSet {
		if !s.allowImageQueries {
			return SearchResponse{}, errors.New("visual image queries are disabled")
		}
		if query.Image.Kind != MediaKindImage || int64(len(query.Image.Bytes)) > MaxQueryImageBytes {
			return SearchResponse{}, errors.New("invalid visual query image")
		}
	}
	if query.Limit == 0 {
		query.Limit = 20
	}
	if query.Limit < 1 || query.Limit > 100 {
		return SearchResponse{}, errors.New("visual search limit must be between 1 and 100")
	}
	if query.SourceID < 0 || query.MessageID < 0 ||
		(query.After != nil && query.Before != nil && !query.After.Before(*query.Before)) {
		return SearchResponse{}, errors.New("invalid visual search filters")
	}
	if query.Person != nil {
		if err := personscope.Validate(*query.Person); err != nil {
			return SearchResponse{}, fmt.Errorf("invalid visual person scope: %w", err)
		}
	}
	generation, err := s.archive.ActiveVisualGeneration(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SearchResponse{}, ErrSearchNotReady
		}
		return SearchResponse{}, err
	}
	queryHash, err := visualSearchQueryHash(query)
	if err != nil {
		return SearchResponse{}, err
	}
	var cursor searchCursor
	if query.Cursor != "" {
		cursor, err = decodeSearchCursor(query.Cursor)
		if err != nil || cursor.GenerationID != generation.ID || cursor.QueryHash != queryHash || cursor.Token == "" {
			return SearchResponse{}, ErrInvalidCursor
		}
	}
	vector, usage, err := s.provider.EmbedQuery(ctx, QueryInput{Text: query.Text, Image: query.Image})
	if err != nil {
		return SearchResponse{}, fmt.Errorf("embed visual query: %w", err)
	}
	request := SearchRequest{GenerationID: GenerationID(generation.ID), Vector: vector, Limit: query.Limit + 1,
		SourceID: query.SourceID, MessageID: query.MessageID,
		Person:   query.Person,
		Filename: strings.TrimSpace(query.Filename), MIMEPrefix: strings.ToLower(strings.TrimSpace(query.MIMEPrefix)),
		After: query.After, Before: query.Before}
	if query.Cursor != "" {
		request.AfterScore = &cursor.Score
		request.AfterToken = VectorToken(cursor.Token)
	}
	hits, err := s.backend.Search(ctx, request)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("search visual vectors: %w", err)
	}
	response := SearchResponse{GenerationID: generation.ID, Model: generation.Model, QueryMode: "text", Usage: usage,
		Results: make([]AttachmentSearchResult, 0, min(query.Limit, len(hits)))}
	if imageSet {
		response.QueryMode = "image"
	}
	for _, hit := range hits[:min(query.Limit, len(hits))] {
		occurrence, resolveErr := s.archive.ResolveVisualSearchOccurrence(ctx, generation.ID, string(hit.Token))
		if errors.Is(resolveErr, sql.ErrNoRows) {
			continue
		}
		if resolveErr != nil {
			return SearchResponse{}, resolveErr
		}
		response.Results = append(response.Results, AttachmentSearchResult{
			AttachmentID: occurrence.AttachmentID, MessageID: occurrence.MessageID,
			ConversationID: occurrence.ConversationID, SourceID: occurrence.SourceID,
			SourceMessageID: occurrence.SourceMessageID, BlobHash: occurrence.BlobHash,
			Filename: occurrence.Filename, MIMEType: occurrence.MIMEType, Size: occurrence.Size,
			SentAt: occurrence.SentAt, Score: hit.Score, Rank: len(response.Results) + 1,
		})
	}
	if query.Person != nil && len(response.Results) > 0 {
		messageIDs := make([]int64, len(response.Results))
		for i := range response.Results {
			messageIDs[i] = response.Results[i].MessageID
		}
		provenance, provenanceErr := s.archive.PersonProvenanceForMessages(ctx, messageIDs, *query.Person)
		if provenanceErr != nil {
			return SearchResponse{}, provenanceErr
		}
		for i := range response.Results {
			response.Results[i].PersonProvenance = provenance[response.Results[i].MessageID]
			if response.Results[i].PersonProvenance == nil {
				return SearchResponse{}, fmt.Errorf("visual person provenance missing for message %d", response.Results[i].MessageID)
			}
		}
	}
	if len(hits) > query.Limit {
		last := hits[query.Limit-1]
		response.NextCursor, err = encodeSearchCursor(searchCursor{
			GenerationID: generation.ID, QueryHash: queryHash, Score: last.Score, Token: string(last.Token),
		})
		if err != nil {
			return SearchResponse{}, err
		}
	}
	return response, nil
}

func visualSearchQueryHash(query SearchQuery) (string, error) {
	imageHash := ""
	if query.Image != nil {
		digest := sha256.Sum256(query.Image.Bytes)
		imageHash = hex.EncodeToString(digest[:])
	}
	payload, err := json.Marshal(searchQueryHashPayload{
		Text: strings.TrimSpace(query.Text), ImageHash: imageHash,
		Filename:   strings.TrimSpace(query.Filename),
		MIMEPrefix: strings.ToLower(strings.TrimSpace(query.MIMEPrefix)),
		Person:     query.Person, SourceID: query.SourceID, MessageID: query.MessageID,
		After: query.After, Before: query.Before,
	})
	if err != nil {
		return "", fmt.Errorf("marshal visual search query: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func encodeSearchCursor(cursor searchCursor) (string, error) {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeSearchCursor(raw string) (searchCursor, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return searchCursor{}, fmt.Errorf("decode visual search cursor: %w", err)
	}
	var cursor searchCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return searchCursor{}, err
	}
	return cursor, nil
}
