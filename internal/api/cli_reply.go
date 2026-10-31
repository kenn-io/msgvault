package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/store"
	xapi "go.kenn.io/msgvault/internal/x"
)

// ReplyPrepareRequest contains only reviewed draft identity. Authority and
// prices come from the authenticated daemon, never this request.
type ReplyPrepareRequest struct {
	DraftID   string `json:"draft_id"`
	Revision  int64  `json:"revision"`
	AdoptPost string `json:"adopt_post,omitempty"`
}

func (r ReplyPrepareRequest) Validate() error {
	if !strings.HasPrefix(r.DraftID, store.ChatDraftIDPrefix) || len(r.DraftID) > 128 || r.Revision <= 0 || (r.AdoptPost != "" && xapi.ValidateID(r.AdoptPost) != nil) {
		return errors.New("a managed chat draft and exact positive revision are required")
	}
	return nil
}

type ReplyPreparer interface {
	PrepareReply(context.Context, ReplyPrepareRequest, *agentgrant.Grant) (xapi.ReplyPreparation, error)
}

// SendReplyRequest selects either an archived parent and body, or an existing
// managed draft and revision. Neither form accepts a source selector.
type SendReplyRequest struct {
	ReplyPrepareRequest
	MessageID int64
	Body      string
	JSON      bool
}

func IsCLIRunSendReply(args []string) bool { return len(args) > 0 && args[0] == "send-reply" }
func ParseCLIRunSendReply(req CLIRunRequest) (SendReplyRequest, error) {
	invalid := errors.New("send-reply accepts parent message ID with --body, or --draft with --revision and optional --adopt-post; no environment, working directory or delegation")
	var out SendReplyRequest
	if !IsCLIRunSendReply(req.Args) || len(req.Env) > 0 || req.Cwd != "" || req.Grant != nil || req.GrantDecided {
		return out, invalid
	}
	seen := map[string]bool{}
	args := req.Args[1:]
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		if !strings.HasPrefix(arg, "--") {
			id, err := strconv.ParseInt(arg, 10, 64)
			if err != nil || id <= 0 || strconv.FormatInt(id, 10) != arg || out.MessageID != 0 {
				return SendReplyRequest{}, invalid
			}
			out.MessageID = id
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if seen[name] {
			return SendReplyRequest{}, invalid
		}
		seen[name] = true
		switch name {
		case "draft", "revision", "adopt-post", "body":
			if !hasValue {
				if len(args) == 0 {
					return SendReplyRequest{}, invalid
				}
				value, args = args[0], args[1:]
			}
			switch name {
			case "draft":
				out.DraftID = value
			case "body":
				out.Body = value
			case "adopt-post":
				out.AdoptPost = value
			case "revision":
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil || n <= 0 || strconv.FormatInt(n, 10) != value {
					return SendReplyRequest{}, invalid
				}
				out.Revision = n
			}
		case "json":
			if hasValue && value != "true" && value != "false" {
				return SendReplyRequest{}, invalid
			}
			out.JSON = !hasValue || value == "true"
		default:
			return SendReplyRequest{}, invalid
		}
	}
	if seen["draft"] {
		if out.MessageID != 0 || seen["body"] || !seen["revision"] || out.ReplyPrepareRequest.Validate() != nil || (seen["adopt-post"] && out.AdoptPost == "") {
			return SendReplyRequest{}, invalid
		}
	} else if out.MessageID == 0 || !seen["body"] || seen["revision"] || seen["adopt-post"] || xapi.ValidateReplyText(out.Body) != nil {
		return SendReplyRequest{}, invalid
	}
	return out, nil
}
func (s *Server) handleReplyPrepare(w http.ResponseWriter, r *http.Request) {
	auth := s.requestAuthentication(r)
	if auth.Mode == AuthModeDelegated || auth.Grant != nil {
		writeError(w, 403, "not_permitted", "reply sending and preparation require the owner")
		return
	}
	var req ReplyPrepareRequest
	dec := jsontext.NewDecoder(r.Body)
	if err := json.UnmarshalDecode(dec, &req, json.RejectUnknownMembers(true)); err != nil {
		writeError(w, 400, "invalid_args", "invalid reply preparation request")
		return
	}
	if !requireSingleJSONValue(w, dec, "invalid_args") {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, 400, "invalid_args", err.Error())
		return
	}
	service, ok := s.store.(ReplyPreparer)
	if !ok {
		writeError(w, 503, "unsupported_capability", "x_reply_send is unavailable")
		return
	}
	result, err := service.PrepareReply(r.Context(), req, nil)
	if err != nil {
		var coded *CLIRunCodedError
		if errors.As(err, &coded) {
			writeError(w, 409, coded.Code, coded.Code)
		} else {
			writeError(w, 500, "reply_prepare_failed", "reply preparation failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}
