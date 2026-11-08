package notionmeetings

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

type discoverySource interface {
	RetrieveSelf(ctx context.Context) (*User, error)
	QueryMeetingNotesFiltered(ctx context.Context, limit int, filter *MeetingFilter) (*QueryResult, error)
	Search(ctx context.Context, cursor string) (*DiscoveryPage, error)
	QueryDataSource(ctx context.Context, id, cursor string) (*DiscoveryPage, error)
	RetrieveDatabase(ctx context.Context, id string) (*Database, error)
}
type discoveryRun struct {
	state    *syncState
	client   discoverySource
	source   Source
	limits   discoveryLimits
	now      time.Time
	requests int
	visible  map[string]bool
}

func (r *discoveryRun) capacity() int { return max(0, r.limits.Pending-len(r.state.Admissions)) }
func (r *discoveryRun) admit(id string) {
	if r.visible == nil {
		r.visible = map[string]bool{}
	}
	r.visible[id] = true
	if slices.Contains(r.state.Admissions, id) {
		return
	}
	r.state.Admissions = append(r.state.Admissions, id)
}
func (r *discoveryRun) stepQuery(ctx context.Context) (bool, error) {
	s := r.state
	if len(s.Windows) == 0 || r.requests >= r.limits.Requests || r.capacity() < maxQueryResults {
		return false, nil
	}
	window := s.Windows[0]
	r.requests++
	result, err := r.client.QueryMeetingNotesFiltered(ctx, maxQueryResults, window.filter())
	if err != nil && (ctx.Err() != nil || errors.Is(err, ErrRequestBudget)) && len(s.Windows) > 1 {
		s.Windows = append(s.Windows[1:], window)
	}
	if errors.Is(err, ErrUnauthorized) || ctx.Err() != nil || errors.Is(err, ErrRequestBudget) {
		return false, err
	}
	if err != nil || result == nil || len(result.Results) > maxQueryResults {
		reason := errorCategory(err)
		if err == nil {
			reason = "malformed_response"
		}
		s.deferAttemptedWindow(window, reason, r.now)
		s.Windows = s.Windows[1:]
		return true, nil
	}
	for _, m := range result.Results {
		if strings.TrimSpace(m.ID) == "" {
			s.reason("malformed_response", "query")
			continue
		}
		r.admit(m.ID)
	}
	s.LastQueryAt = r.now.Format(time.RFC3339)
	s.Coverage.Returned += len(result.Results)
	s.Coverage.HasMore = s.Coverage.HasMore || result.HasMore
	s.Windows = s.Windows[1:]
	if result.HasMore {
		if split, ok := window.split(result.Results); ok {
			s.Windows = append(s.Windows, split...)
		} else {
			s.deferAttemptedWindow(window, "query_cap", r.now)
		}
	} else {
		s.resolveWindow(window)
	}
	return true, nil
}
func errorCategory(err error) string {
	var api *APIError
	switch {
	case errors.Is(err, ErrUnauthorized):
		return "invalid_credentials"
	case errors.Is(err, ErrUnknownIdentity):
		return "unknown_identity"
	case errors.As(err, &api) && (api.Status == 403 || api.Status == 404):
		return "inaccessible_subtree"
	case errors.Is(err, ErrMeetingAccess), errors.Is(err, ErrReadContent):
		return "inaccessible_subtree"
	case errors.Is(err, ErrMalformedResponse):
		return "malformed_response"
	case errors.Is(err, ErrRequestBudget):
		return "request_budget"
	case errors.Is(err, context.Canceled):
		return "interrupted"
	case errors.Is(err, context.DeadlineExceeded):
		return "time_budget"
	default:
		return "transient_error"
	}
}
func (r *discoveryRun) addTask(t scopeTask) {
	s := r.state
	if s.Seen[t.key()] {
		return
	}
	if t.Depth > r.limits.Depth {
		s.deferTask(t, "depth_limit", r.now)
		return
	}
	if len(s.Seen) >= r.limits.Scopes {
		s.deferTask(t, "scope_limit", r.now)
		return
	}
	s.Seen[t.key()] = true
	if len(s.Frontier) >= r.limits.Frontier {
		s.deferTask(t, "frontier_budget", r.now)
		return
	}
	s.Frontier = append(s.Frontier, t)
}
func (r *discoveryRun) continuation(task scopeTask, more bool, next string) {
	if !more {
		return
	}
	if next == "" || next == task.Cursor {
		r.state.deferAttemptedTask(task, "cursor_cycle", r.now)
		return
	}
	if slices.Contains(task.Cursors, next) {
		r.state.deferAttemptedTask(task, "cursor_cycle", r.now)
		return
	}
	task.Cursors = append(task.Cursors, task.Cursor)
	task.Cursor = next
	if len(r.state.Frontier) >= r.limits.Frontier {
		r.state.deferTask(task, "frontier_budget", r.now)
		return
	}
	r.state.Frontier = append(r.state.Frontier, task)
}
func (r *discoveryRun) stepTraversal(ctx context.Context) (bool, error) {
	s := r.state
	if len(s.Frontier) == 0 || r.requests >= r.limits.Requests || r.capacity() == 0 {
		return false, nil
	}
	// Explicit root sets are checked against the same bounded scope/frontier
	// ceilings as discovered children. A ceiling ends this sweep with gaps.
	if len(s.Seen) > r.limits.Scopes || len(s.Frontier) > r.limits.Frontier {
		for _, task := range s.Frontier {
			s.deferTask(task, "scope_limit", r.now)
		}
		s.Frontier = nil
		s.Seen = map[string]bool{}
		return true, nil
	}
	task := s.Frontier[0]
	if task.Depth > r.limits.Depth {
		s.Frontier = s.Frontier[1:]
		s.deferTask(task, "depth_limit", r.now)
		return true, nil
	}
	size := min(pageSize, r.capacity(), r.limits.Frontier-len(s.Frontier)+1)
	ctx = context.WithValue(ctx, pageSizeKey{}, max(1, size))
	var page *DiscoveryPage
	var blocks *BlockPage
	var db *Database
	var err error
	r.requests++
	switch task.Kind {
	case "search":
		page, err = r.client.Search(ctx, task.Cursor)
	case "data_source":
		page, err = r.client.QueryDataSource(ctx, task.ID, task.Cursor)
	case "database":
		db, err = r.client.RetrieveDatabase(ctx, task.ID)
	case "blocks":
		if sized, ok := r.source.(interface {
			retrieveBlockChildrenSized(ctx context.Context, id, cursor string, size int) (*BlockPage, error)
		}); ok {
			blocks, err = sized.retrieveBlockChildrenSized(ctx, task.ID, task.Cursor, size)
		} else {
			blocks, err = r.source.RetrieveBlockChildren(ctx, task.ID, task.Cursor)
		}
	case "meeting":
		r.admit(task.ID)
	default:
		err = ErrMalformedResponse
	}
	if err != nil && (ctx.Err() != nil || errors.Is(err, ErrRequestBudget)) && len(s.Frontier) > 1 {
		s.Frontier = append(s.Frontier[1:], task)
	}
	if errors.Is(err, ErrUnauthorized) || ctx.Err() != nil || errors.Is(err, ErrRequestBudget) {
		return false, err
	}
	if err != nil {
		s.Frontier = s.Frontier[1:]
		s.deferAttemptedTask(task, errorCategory(err), r.now)
		return true, nil
	}
	// Validate before advancing so a malformed/oversize response cannot skip work.
	if (page != nil && (len(page.Results) > size || (page.HasMore && page.NextCursor == ""))) || (blocks != nil && (len(blocks.Results) > size || (blocks.HasMore && blocks.NextCursor == ""))) {
		s.Frontier = s.Frontier[1:]
		s.deferAttemptedTask(task, "malformed_response", r.now)
		return true, nil
	}
	if task.Kind != "meeting" && page == nil && blocks == nil && db == nil {
		s.Frontier = s.Frontier[1:]
		s.deferAttemptedTask(task, "malformed_response", r.now)
		return true, nil
	}
	s.Frontier = s.Frontier[1:]
	s.resolveTask(task)
	if page != nil {
		if page.RequestStatus.Type == "incomplete" {
			s.deferAttemptedTask(task, "data_source_cap", r.now)
		}
		for _, object := range page.Results {
			if strings.TrimSpace(object.ID) == "" {
				s.reason("malformed_response", task.Kind)
				continue
			}
			switch object.Object {
			case "page":
				r.addTask(scopeTask{Kind: "blocks", ID: object.ID, Depth: task.Depth + 1})
			case "data_source":
				r.addTask(scopeTask{Kind: "data_source", ID: object.ID, Depth: task.Depth + 1})
			default:
				s.reason("malformed_response", task.Kind)
			}
		}
		r.continuation(task, page.HasMore, page.NextCursor)
	}
	if blocks != nil {
		for _, block := range blocks.Results {
			if strings.TrimSpace(block.ID) == "" {
				s.reason("malformed_response", task.Kind)
				continue
			}
			switch block.Type {
			case meetingNotesType:
				r.admit(block.ID)
			case "child_database":
				r.addTask(scopeTask{Kind: "database", ID: block.ID, Depth: task.Depth + 1})
			case "child_page":
				r.addTask(scopeTask{Kind: "blocks", ID: block.ID, Depth: task.Depth + 1})
			default:
				if block.HasChildren {
					r.addTask(scopeTask{Kind: "blocks", ID: block.ID, Depth: task.Depth + 1})
				}
			}
		}
		r.continuation(task, blocks.HasMore, blocks.NextCursor)
	}
	if db != nil {
		if task.Offset < 0 || task.Offset > len(db.DataSources) {
			s.deferAttemptedTask(scopeTask{Kind: "database", ID: task.ID, Depth: task.Depth}, "malformed_response", r.now)
			return true, nil
		}
		// A database response has no page-size control. Replay a bounded suffix
		// through an offset rather than requiring room for all its data sources.
		count := min(len(db.DataSources)-task.Offset, max(1, r.limits.Frontier-len(s.Frontier)))
		for _, object := range db.DataSources[task.Offset : task.Offset+count] {
			r.addTask(scopeTask{Kind: "data_source", ID: object.ID, Depth: task.Depth + 1})
		}
		task.Offset += count
		if task.Offset < len(db.DataSources) {
			if len(s.Frontier) < r.limits.Frontier {
				s.Frontier = append(s.Frontier, task)
			} else {
				s.deferTask(task, "frontier_budget", r.now)
			}
		}
	}
	return true, nil
}
