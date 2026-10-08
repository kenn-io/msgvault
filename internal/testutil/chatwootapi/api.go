package chatwootapi

import (
	"cmp"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// API models producer listing and bounded message ranges independently of importers.
type API struct {
	clock               func() time.Time
	Mu                  sync.Mutex
	Conversations       map[int64][]map[string]any
	ConversationInboxID int64
	PageSize            int
	Cap                 int
	IgnoreBounds        bool
	Contact             map[string]any
	Assignee            map[string]any
	requests            []string
	UpdatedAt           map[int64]float64
	ActivityAt          map[int64]int64
	Hidden              map[int64]bool
	DeniedDetails       map[int64]bool
	DeniedMessages      map[int64]bool
	OnRequest           func(string)
}

// New serves messages as conversation 42 of inbox 7, account 3.
func New(pageCap int, messages []map[string]any, clock func() time.Time) *API {
	api := &API{
		Conversations: map[int64][]map[string]any{}, UpdatedAt: map[int64]float64{}, ActivityAt: map[int64]int64{}, PageSize: 25, Cap: pageCap, clock: clock,
		Contact:  map[string]any{"id": int64(7), "type": "contact", "name": "Example Contact", "phone_number": "+12025550101"},
		Assignee: map[string]any{"id": int64(99), "type": "user", "name": "Example Assignee"},
	}
	if messages != nil {
		api.Conversations[42] = messages
		api.UpdatedAt[42] = float64(api.clock().Unix())
	}
	return api
}

func (api *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.Mu.Lock()
	defer api.Mu.Unlock()
	if r.Header.Get("Api_access_token") != "synthetic-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var result any
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/accounts/3")
	if api.OnRequest != nil {
		api.OnRequest(path)
	}
	var id int64
	switch {
	case path == "/agents":
		api.requests = append(api.requests, "agents")
		result = []any{map[string]any{"id": 7, "name": "Example Agent"}, map[string]any{"id": 8, "name": "Example Owner"}, api.Assignee}
	case path == "/conversations":
		if r.URL.Query().Get("status") != "all" {
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
		sortBy := r.URL.Query().Get("sort_by")
		api.requests = append(api.requests, "list "+sortBy)
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page <= 0 {
			http.Error(w, "invalid page", http.StatusBadRequest)
			return
		}
		ids := slices.Sorted(maps.Keys(api.Conversations))
		ids = slices.DeleteFunc(ids, func(id int64) bool { return api.Hidden[id] })
		if sortBy == "last_activity_at_desc" {
			slices.SortStableFunc(ids, func(a, b int64) int { return cmp.Compare(api.activity(b), api.activity(a)) })
		}
		payload := []any{}
		for index := (page - 1) * api.PageSize; index < min(page*api.PageSize, len(ids)); index++ {
			payload = append(payload, api.conversation(ids[index]))
		}
		result = map[string]any{"data": map[string]any{"payload": payload}}
	case strings.HasSuffix(path, "/messages"):
		_, err := fmt.Sscanf(path, "/conversations/%d/messages", &id)
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		// The importer always sends both bounds; after is inclusive with them.
		after, afterErr := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		before, beforeErr := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		if afterErr != nil || beforeErr != nil {
			http.Error(w, "invalid bounds", http.StatusBadRequest)
			return
		}
		api.requests = append(api.requests, fmt.Sprintf("messages %d %d %d", id, after, before))
		if api.DeniedMessages[id] {
			http.Error(w, "synthetic access denied", http.StatusUnauthorized)
			return
		}
		payload := []map[string]any{}
		for _, message := range api.Conversations[id] {
			if messageID := fixtureInt(message, "id"); api.IgnoreBounds || (messageID >= after && messageID < before) {
				payload = append(payload, message)
			}
		}
		slices.SortStableFunc(payload, func(a, b map[string]any) int {
			return cmp.Compare(fixtureInt(a, "created_at"), fixtureInt(b, "created_at"))
		})
		result = map[string]any{"meta": map[string]any{"contact": api.Contact, "assignee": api.Assignee}, "payload": payload[:min(len(payload), api.Cap)]}
	default:
		_, err := fmt.Sscanf(path, "/conversations/%d", &id)
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		api.requests = append(api.requests, fmt.Sprintf("conversation %d", id))
		if api.DeniedDetails[id] {
			http.Error(w, "synthetic access denied", http.StatusUnauthorized)
			return
		}
		result = api.conversation(id)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

// fixtureInt reads an int64 fixture field; fixtures always set them.
func fixtureInt(message map[string]any, key string) int64 {
	value, _ := message[key].(int64)
	return value
}

func (api *API) activity(id int64) int64 {
	if value, ok := api.ActivityAt[id]; ok {
		return value
	}
	activity := int64(1767225500)
	for _, message := range api.Conversations[id] {
		activity = max(activity, fixtureInt(message, "created_at"))
	}
	return activity
}

func (api *API) conversation(id int64) map[string]any {
	inboxID := api.ConversationInboxID
	if inboxID == 0 {
		inboxID = 7
	}
	// Chatwoot seeds the listing with the newest message by creation time. The
	// seed's content is private and differs from the message list, to catch
	// leakage through raw conversation context.
	var newest map[string]any
	for _, message := range api.Conversations[id] {
		if newest == nil || cmp.Or(cmp.Compare(fixtureInt(message, "created_at"), fixtureInt(newest, "created_at")), cmp.Compare(fixtureInt(message, "id"), fixtureInt(newest, "id"))) > 0 {
			newest = message
		}
	}
	private := map[string]any{"private": true, "content": "excluded-private-seed"}
	if newest != nil {
		private["id"] = newest["id"]
	}
	result := map[string]any{
		"id": id, "account_id": 3, "inbox_id": inboxID, "status": "resolved", "created_at": int64(1767225500), "updated_at": api.UpdatedAt[id], "last_activity_at": api.activity(id),
		"meta":     map[string]any{"sender": api.Contact, "assignee": api.Assignee},
		"messages": []any{private}, "last_non_activity_message": private,
	}
	if _, ok := api.UpdatedAt[id]; !ok {
		delete(result, "updated_at")
	}
	return result
}

func (api *API) AddMessage(conversationID, messageID int64, at time.Time, attachments ...map[string]any) {
	api.Mu.Lock()
	defer api.Mu.Unlock()
	message := Message(messageID, at.Unix(), nil)
	message["conversation_id"] = conversationID
	if len(attachments) > 0 {
		message["attachments"] = attachments
	}
	api.UpdatedAt[conversationID] = float64(api.clock().Unix())
	api.Conversations[conversationID] = append(api.Conversations[conversationID], message)
}

// TakeRequests returns and clears the requests seen since the last call.
func (api *API) TakeRequests() []string {
	api.Mu.Lock()
	defer api.Mu.Unlock()
	requests := api.requests
	api.requests = nil
	return requests
}

func Message(id, at int64, sender map[string]any) map[string]any {
	message := map[string]any{
		"id": id, "inbox_id": int64(7), "conversation_id": int64(42), "content": fmt.Sprintf("Synthetic message %d", id),
		"content_type": "text", "message_type": 0, "created_at": at, "private": false, "status": "sent", "content_attributes": map[string]any{},
	}
	if sender != nil {
		message["sender"] = sender
	}
	return message
}
