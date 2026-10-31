// Package lineapi provides a synthetic Messaging API for receiver/processor tests.
package lineapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Response overrides an endpoint's next response. Body is sent verbatim.
type Response struct {
	Status int
	Header http.Header
	Body   string
}

// Request is a recorded request; all values belong to synthetic tests.
type Request struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

// Server implements http.Handler with useful defaults and queued overrides.
// Enqueue and Requests can be called while concurrent clients are running.
type Server struct {
	mu        sync.Mutex
	responses map[string][]Response
	requests  []Request
	tokens    int
}

func New() *Server { return &Server{responses: make(map[string][]Response)} }

func (s *Server) Enqueue(method, path string, responses ...Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, response := range responses {
		response.Header = response.Header.Clone()
		s.responses[method+" "+path] = append(s.responses[method+" "+path], response)
	}
}

func (s *Server) Count(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, request := range s.requests {
		if request.Method == method && request.Path == path {
			count++
		}
	}
	return count
}

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	requests := make([]Request, len(s.requests))
	for i, r := range s.requests {
		r.Header = r.Header.Clone()
		r.Body = append([]byte(nil), r.Body...)
		requests[i] = r
	}
	return requests
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	path := r.URL.EscapedPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{r.Method, path, r.Header.Clone(), body})
	key := r.Method + " " + path
	response := Response{Status: 200, Body: `{}`}
	switch {
	case key == "POST /oauth2/v3/token":
		s.tokens++
		response.Body = fmt.Sprintf(`{"access_token":"synthetic-token-%d","token_type":"Bearer","expires_in":900}`, s.tokens)
	case r.Header.Get("Authorization") == "":
		response.Status = 401
	case key == "GET /v2/bot/info":
		response.Body = `{"userId":"U00000000000000000000000000000001","basicId":"@example","displayName":"Example Bot"}`
	case strings.Contains(path, "/profile/") || strings.Contains(path, "/member/"):
		// Decode the final escaped path segment rather than splitting URL.Path:
		// an encoded slash is part of the requested user ID.
		userID, _ := url.PathUnescape(path[strings.LastIndex(path, "/")+1:])
		profile := map[string]string{"userId": userID, "displayName": "Example User", "pictureUrl": "https://example.com/avatar.png"}
		if strings.Contains(path, "/profile/") {
			profile["language"] = "en"
		}
		body, err := json.Marshal(profile)
		if err != nil {
			http.Error(w, "cannot encode profile", http.StatusInternalServerError)
			return
		}
		response.Body = string(body)
	case strings.HasSuffix(path, "/summary"):
		response.Body = `{"groupId":"C00000000000000000000000000000003","groupName":"Example Group"}`
	case strings.HasSuffix(path, "/content"):
		response.Body = "synthetic-content"
	case key == "POST /v2/bot/channel/webhook/test":
		response.Body = `{"success":true,"timestamp":"2026-10-30T00:00:00Z","statusCode":200,"reason":"OK","detail":""}`
	}
	if queued := s.responses[key]; len(queued) > 0 {
		response = queued[0]
		s.responses[key] = queued[1:]
	}
	for name, values := range response.Header {
		w.Header()[name] = append([]string(nil), values...)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	status := response.Status
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, response.Body)
}

// Sign creates the official base64 HMAC-SHA256 webhook signature.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
