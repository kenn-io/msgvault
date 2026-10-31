package x

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/netguard"
)

const (
	DefaultBaseURL   = "https://api.x.com/2"
	maxResponseBytes = 8 << 20
	postFields       = "id,text,author_id,created_at,conversation_id,in_reply_to_user_id,referenced_tweets,entities,lang,edit_history_tweet_ids,edit_controls,withheld,note_tweet"
)

var (
	ErrDecodeResponse = errors.New("X API response could not be decoded; check API field compatibility")
	ErrResponseShape  = errors.New("X API returned resources beyond the admitted request shape")
	// ErrReadModelExceeded distinguishes billable count excess from malformed
	// objects and unexpected IDs, which do not establish a pricing-model excess.
	ErrReadModelExceeded = fmt.Errorf("X read resource count exceeds the admitted model: %w", ErrResponseShape)
	ErrTransport         = errors.New("X API transport failed; request outcome may be unknown")
)

// APIError omits provider-controlled text, URLs, and credentials.
type APIError struct {
	StatusCode     int
	Code           string
	Definite       bool
	RetryAfter     string
	RateLimitReset string
}

func (e *APIError) Error() string { return fmt.Sprintf("X API: HTTP %d (%s)", e.StatusCode, e.Code) }

// Client performs exactly one request per method, including writes. Its caller
// owns credentials, admission, retry policy, and durable response settlement.
type Client struct {
	baseURL string
	http    *http.Client
	initErr error
}

var _ API = (*Client)(nil)

// NewClient preserves HTTP policy while disabling native connection replay.
// Custom RoundTrippers must honor the single-request contract themselves.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{baseURL: strings.TrimRight(baseURL, "/")}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && (u.Scheme != "http" || !netguard.IsLoopbackHost(u.Hostname()))) {
		c.initErr = errors.New("X API base URL must be HTTPS or a loopback test endpoint")
	}
	c.http = singleRequestHTTPClient(httpClient)
	return c
}
func (c *Client) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, "X REST client") }

func (c *Client) request(ctx context.Context, method, path string, q url.Values, payload any, status int) ([]byte, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, errors.New("invalid X request")
		}
		body = bytes.NewReader(b)
	}
	endpoint := c.baseURL + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, errors.New("invalid X request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "msgvault-x/1.0")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrTransport
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		return nil, ErrTransport
	}
	if len(b) > maxResponseBytes {
		return nil, ErrDecodeResponse
	}
	if resp.StatusCode != status {
		code := "provider_error"
		switch resp.StatusCode {
		case 401:
			code = "unauthorized"
		case 402:
			code = "credits_exhausted"
		case 403:
			code = "provider_rejected"
		case 429:
			code = "rate_limited"
		}
		var rejection struct {
			Title  string           `json:"title"`
			Errors []jsontext.Value `json:"errors"`
		}
		valid := json.Unmarshal(b, &rejection) == nil && (rejection.Title != "" || len(rejection.Errors) > 0)
		if method == http.MethodGet && resp.StatusCode == http.StatusBadRequest && rejectedPageToken(b, q) {
			code = "invalid_pagination_token"
		}
		return nil, &APIError{StatusCode: resp.StatusCode, Code: code, Definite: valid && resp.StatusCode >= 400 && resp.StatusCode < 500, RetryAfter: resp.Header.Get("Retry-After"), RateLimitReset: resp.Header.Get("x-rate-limit-reset")}
	}
	return b, nil
}

// Recognize only a structured rejection of the token actually sent. Other
// invalid parameters must be corrected by the caller, rather than replayed.
func rejectedPageToken(body []byte, query url.Values) bool {
	var response struct {
		Errors []struct {
			Parameters map[string][]string `json:"parameters"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Errors) == 0 {
		return false
	}
	for _, item := range response.Errors {
		if len(item.Parameters) != 1 {
			return false
		}
		for name, values := range item.Parameters {
			if (name != "pagination_token" && name != "next_token") || query.Get(name) == "" || len(values) != 1 || values[0] != query.Get(name) {
				return false
			}
		}
	}
	return true
}

func fields() url.Values {
	return url.Values{"tweet.fields": {postFields}, "expansions": {"author_id"}, "user.fields": {"id,name,username,protected"}}
}
func bounds(q url.Values, after, before time.Time) error {
	if !after.IsZero() && !before.IsZero() && !after.Before(before) {
		return errors.New("X time bounds must increase")
	}
	if !after.IsZero() {
		q.Set("start_time", after.UTC().Format(time.RFC3339Nano))
	}
	if !before.IsZero() {
		q.Set("end_time", before.UTC().Format(time.RFC3339Nano))
	}
	return nil
}
func (c *Client) Me(ctx context.Context) (User, error) {
	b, err := c.request(ctx, http.MethodGet, "/users/me", url.Values{"user.fields": {"id,name,username,protected"}}, nil, http.StatusOK)
	if err != nil {
		return User{}, err
	}
	var env struct {
		Data User `json:"data"`
	}
	if json.Unmarshal(b, &env) != nil || validateUser(env.Data) != nil {
		return User{}, ErrDecodeResponse
	}
	return env.Data, nil
}
func (c *Client) Timeline(ctx context.Context, r TimelineRequest) (Page, error) {
	if ValidateID(r.OwnerID) != nil || r.MaxResults < 5 || r.MaxResults > 100 {
		return Page{}, errors.New("invalid X timeline request")
	}
	q := fields()
	if err := bounds(q, r.After, r.Before); err != nil {
		return Page{}, err
	}
	if r.SinceID != "" {
		if ValidateID(r.SinceID) != nil || !r.After.IsZero() {
			return Page{}, errors.New("invalid X ID/time bounds")
		}
		q.Set("since_id", r.SinceID)
	}
	q.Set("max_results", strconv.Itoa(r.MaxResults))
	if r.Token != "" {
		q.Set("pagination_token", r.Token)
	}
	kind := "tweets"
	if r.Mentions {
		kind = "mentions"
	}
	b, err := c.request(ctx, http.MethodGet, "/users/"+r.OwnerID+"/"+kind, q, nil, http.StatusOK)
	if err != nil {
		return Page{}, err
	}
	return decodePage(b, r.MaxResults)
}
func (c *Client) SearchAll(ctx context.Context, r SearchRequest) (Page, error) {
	if strings.TrimSpace(r.Query) == "" || len(r.Query) > 4096 || r.MaxResults < 10 || r.MaxResults > 500 {
		return Page{}, errors.New("invalid X archive search request")
	}
	q := fields()
	if err := bounds(q, r.After, r.Before); err != nil {
		return Page{}, err
	}
	q.Set("query", r.Query)
	q.Set("sort_order", "recency")
	q.Set("max_results", strconv.Itoa(r.MaxResults))
	if r.Token != "" {
		q.Set("next_token", r.Token)
	}
	b, err := c.request(ctx, http.MethodGet, "/tweets/search/all", q, nil, http.StatusOK)
	if err != nil {
		return Page{}, err
	}
	return decodePage(b, r.MaxResults)
}
func (c *Client) Lookup(ctx context.Context, ids []string) (Page, error) {
	if len(ids) < 1 || len(ids) > 100 {
		return Page{}, errors.New("X lookup requires 1–100 unique IDs")
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if ValidateID(id) != nil || seen[id] {
			return Page{}, errors.New("X lookup requires valid unique IDs")
		}
		seen[id] = true
	}
	q := fields()
	q.Set("ids", strings.Join(ids, ","))
	b, err := c.request(ctx, http.MethodGet, "/tweets", q, nil, http.StatusOK)
	if err != nil {
		return Page{}, err
	}
	page, err := decodePage(b, len(ids))
	if err != nil {
		return Page{}, err
	}
	for _, post := range page.Posts {
		if !seen[post.ID] {
			return Page{}, ErrResponseShape
		}
	}
	return page, nil
}
func (c *Client) CreateReply(ctx context.Context, text, parentID string) (string, error) {
	if ValidateID(parentID) != nil {
		return "", errors.New("invalid X reply parent")
	}
	payload := struct {
		Text  string `json:"text"`
		Reply struct {
			ID string `json:"in_reply_to_tweet_id"`
		} `json:"reply"`
	}{Text: text}
	payload.Reply.ID = parentID
	b, err := c.request(ctx, http.MethodPost, "/tweets", nil, payload, http.StatusCreated)
	if err != nil {
		return "", err
	}
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &env) != nil || ValidateID(env.Data.ID) != nil {
		return "", ErrDecodeResponse
	}
	return env.Data.ID, nil
}

type wirePost struct {
	ID             string         `json:"id"`
	AuthorID       string         `json:"author_id"`
	ConversationID string         `json:"conversation_id"`
	ReplyUserID    string         `json:"in_reply_to_user_id"`
	Text           *string        `json:"text"`
	CreatedAt      time.Time      `json:"created_at"`
	References     jsontext.Value `json:"referenced_tweets"`
	PostReferences jsontext.Value `json:"referenced_posts"`
	History        jsontext.Value `json:"edit_history_tweet_ids"`
	PostHistory    jsontext.Value `json:"edit_history_post_ids"`
	Note           jsontext.Value `json:"note_tweet"`
	PostNote       jsontext.Value `json:"note_post"`
	Withheld       jsontext.Value `json:"withheld"`
	Entities       struct {
		Mentions []Mention `json:"mentions"`
	} `json:"entities"`
}

func alias[T any](a, b jsontext.Value, out *T) error {
	if len(a) > 0 {
		if err := json.Unmarshal(a, out); err != nil {
			return ErrDecodeResponse
		}
	}
	if len(b) > 0 {
		var other T
		if json.Unmarshal(b, &other) != nil {
			return ErrDecodeResponse
		}
		if len(a) > 0 && !reflect.DeepEqual(*out, other) {
			return ErrDecodeResponse
		}
		*out = other
	}
	return nil
}
func decodePost(raw jsontext.Value) (Post, error) {
	var w wirePost
	if json.Unmarshal(raw, &w) != nil || ValidateID(w.ID) != nil || ValidateID(w.AuthorID) != nil || ValidateID(w.ConversationID) != nil || w.Text == nil || w.CreatedAt.IsZero() {
		return Post{}, ErrDecodeResponse
	}
	p := Post{ID: w.ID, AuthorID: w.AuthorID, ConversationID: w.ConversationID, ReplyUserID: w.ReplyUserID, Text: *w.Text, CreatedAt: w.CreatedAt, Mentions: w.Entities.Mentions, Raw: append(jsontext.Value(nil), raw...)}
	if alias(w.References, w.PostReferences, &p.References) != nil || alias(w.History, w.PostHistory, &p.EditHistory) != nil {
		return Post{}, ErrDecodeResponse
	}
	var note struct {
		Text string `json:"text"`
	}
	if alias(w.Note, w.PostNote, &note) != nil {
		return Post{}, ErrDecodeResponse
	}
	p.LongText = note.Text
	// Completeness describes the selected archive text, which uses LongText
	// when available. A null/empty note supplies no positive completeness fact.
	p.TextIncomplete = p.LongText == ""
	p.Withheld = len(w.Withheld) > 0 && string(w.Withheld) != "null"
	parents := 0
	for _, ref := range p.References {
		if ValidateID(ref.ID) != nil || ref.ID == p.ID {
			return Post{}, ErrDecodeResponse
		}
		switch ref.Type {
		case "replied_to":
			parents++
		case "quoted", "retweeted":
		default:
			return Post{}, ErrDecodeResponse
		}
	}
	if parents > 1 || (p.ReplyUserID != "" && (ValidateID(p.ReplyUserID) != nil || parents != 1)) {
		return Post{}, ErrDecodeResponse
	}
	if len(p.EditHistory) == 0 {
		p.EditHistory = []string{p.ID}
	}
	found := false
	for i, id := range p.EditHistory {
		if ValidateID(id) != nil || (i > 0 && CompareID(p.EditHistory[i-1], id) >= 0) {
			return Post{}, ErrDecodeResponse
		}
		found = found || id == p.ID
	}
	if !found {
		return Post{}, ErrDecodeResponse
	}
	for _, m := range p.Mentions {
		if m.ID != "" && ValidateID(m.ID) != nil {
			return Post{}, ErrDecodeResponse
		}
	}
	return p, nil
}
func validateUser(u User) error {
	if ValidateID(u.ID) != nil || u.Username == "" || len(u.Username) > 256 || len(u.Name) > 4096 {
		return ErrDecodeResponse
	}
	return nil
}
func decodePage(b []byte, maxResults int) (Page, error) {
	var env struct {
		Data     []jsontext.Value          `json:"data"`
		Includes map[string]jsontext.Value `json:"includes"`
		Meta     *struct {
			NextToken   string `json:"next_token"`
			ResultCount *int   `json:"result_count"`
		} `json:"meta"`
		Errors []struct {
			ID    string `json:"resource_id"`
			Value string `json:"value"`
			Type  string `json:"type"`
			Title string `json:"title"`
		} `json:"errors"`
	}
	if json.Unmarshal(b, &env) != nil {
		return Page{}, ErrDecodeResponse
	}
	if env.Data == nil && len(env.Errors) == 0 && (env.Meta == nil || env.Meta.ResultCount == nil || *env.Meta.ResultCount != 0) {
		return Page{}, ErrDecodeResponse
	}
	if len(env.Data) > maxResults {
		return Page{}, ErrReadModelExceeded
	}
	var users []User
	if raw, ok := env.Includes["users"]; ok {
		if json.Unmarshal(raw, &users) != nil {
			return Page{}, ErrDecodeResponse
		}
		if len(users) > maxResults {
			return Page{}, ErrReadModelExceeded
		}
	}
	for key := range env.Includes {
		if key != "users" {
			return Page{}, ErrResponseShape
		}
	}
	nextToken := ""
	if env.Meta != nil {
		nextToken = env.Meta.NextToken
		if env.Meta.ResultCount != nil && *env.Meta.ResultCount != len(env.Data) {
			return Page{}, ErrDecodeResponse
		}
	}
	if len(nextToken) > 8192 {
		return Page{}, ErrDecodeResponse
	}
	page := Page{Users: users, NextToken: nextToken, PostCount: len(env.Data), UserCount: len(users), FetchedAt: time.Now().UTC()}
	authors := make(map[string]bool)
	for _, raw := range env.Data {
		post, err := decodePost(raw)
		if err != nil {
			return Page{}, err
		}
		page.Posts = append(page.Posts, post)
		authors[post.AuthorID] = true
	}
	seen := make(map[string]bool)
	for _, u := range page.Users {
		if validateUser(u) != nil || seen[u.ID] || !authors[u.ID] {
			return Page{}, ErrResponseShape
		}
		seen[u.ID] = true
	}
	for _, item := range env.Errors {
		id := item.ID
		if id == "" {
			id = item.Value
		}
		if id != "" && ValidateID(id) != nil {
			return Page{}, ErrDecodeResponse
		}
		// Only explicit item-level resource-not-found evidence confirms removal.
		unavailable := item.Type == "https://api.x.com/2/problems/resource-not-found" || item.Type == "https://api.twitter.com/2/problems/resource-not-found"
		code := "item_unresolved"
		if unavailable {
			code = "unavailable"
		}
		page.Errors = append(page.Errors, ItemError{ID: id, Code: code, Unavailable: unavailable && id != ""})
	}
	return page, nil
}
