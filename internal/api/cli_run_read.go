package api

import (
	"bytes"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func agentCLIReadPermission(args []string) agentgrant.Permission {
	if len(args) == 0 {
		return ""
	}
	switch args[0] {
	case "search":
		return agentgrant.PermissionSearchRead
	case "show-message":
		return agentgrant.PermissionMessageRead
	case "stats":
		return agentgrant.PermissionStatsRead
	}
	return ""
}

// Scoped reads execute through the same API handlers. They never run an
// owner-authenticated subprocess, accept environment overrides, or access cwd.
func (s *Server) runAgentCLIRead(w http.ResponseWriter, r *http.Request, req CLIRunRequest) {
	permission := agentCLIReadPermission(req.Args)
	grant := s.requestAuthentication(r).Grant
	if grant == nil || !grant.HasPermission(permission) {
		writeAPIHTTPError(w, agentReadDenied(permission))
		return
	}
	if len(req.Env) > 0 || req.Cwd != "" {
		writeError(w, 400, "invalid_args", "Agent reads do not accept env or cwd")
		return
	}
	flags := pflag.NewFlagSet(req.Args[0], pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Bool("json", false, "JSON output")
	if req.Args[0] != "show-message" {
		flags.String("account", "", "Account")
		flags.String("collection", "", "Collection")
	}
	if req.Args[0] == "search" {
		flags.IntP("limit", "n", 50, "Limit")
		flags.Bool("explain", false, "Explain vector search (no effect in fts mode)")
		flags.Int("offset", 0, "Offset")
		flags.String("mode", "fts", "Mode")
		flags.String("deletion-scope", "active", "Deletion scope")
		flags.StringSlice("message-type", nil, "Message types")
	}
	if err := flags.Parse(req.Args[1:]); err != nil {
		writeError(w, 400, "invalid_args", err.Error())
		return
	}
	read := r.Clone(r.Context())
	url := *r.URL
	read.URL = &url
	read.Method = http.MethodGet
	q := read.URL.Query()
	q.Del("q")
	for _, name := range []string{"account", "collection", "mode", "limit", "offset"} {
		if flag := flags.Lookup(name); flag != nil {
			q.Set(name, flag.Value.String())
		}
	}
	operation := ""
	var handler http.HandlerFunc
	switch req.Args[0] {
	case "search":
		q.Set("q", strings.Join(flags.Args(), " "))
		deletion, _ := flags.GetString("deletion-scope")
		q.Set("deletion_scope", deletion)
		types, _ := flags.GetStringSlice("message-type")
		for _, typ := range types {
			q.Add("message_type", typ)
		}
		mode, _ := flags.GetString("mode")
		if mode != "fts" {
			writeError(w, 400, "unsupported_agent_scope", "Agent search supports fts mode only")
			return
		}
		operation = "searchCLI"
		handler = s.handleCLISearch
	case "show-message":
		if len(flags.Args()) != 1 {
			writeError(w, 400, "invalid_args", "show-message requires one message ID")
			return
		}
		q.Set("id", flags.Args()[0])
		operation = "getCLIMessage"
		handler = s.handleCLIMessage
	case "stats":
		if len(flags.Args()) != 0 {
			writeError(w, 400, "invalid_args", "stats accepts no arguments")
			return
		}
		operation = "getCLIStats"
		handler = s.handleCLIStats
	}
	read.URL.RawQuery = q.Encode()
	if err := s.authorizeAgentRead(read, operation, grant); err != nil {
		writeAPIHTTPError(w, err)
		return
	}
	response := &agentReadResponse{header: make(http.Header), status: http.StatusOK}
	handler(response, read)
	if response.status != http.StatusOK {
		maps.Copy(w.Header(), response.header)
		w.WriteHeader(response.status)
		_, _ = w.Write(response.body.Bytes())
		return
	}
	writeEvent := newCLINDJSONEventWriter[CLIRunEvent](w)
	if err := writeEvent(CLIRunEvent{Type: "stdout", Data: response.body.String()}); err != nil {
		return
	}
	_ = writeEvent(CLIRunEvent{Type: cliStreamEventTypeComplete})
}

type agentReadResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *agentReadResponse) Header() http.Header    { return w.header }
func (w *agentReadResponse) WriteHeader(status int) { w.status = status }
func (w *agentReadResponse) Write(data []byte) (int, error) {
	// bytes.Buffer.Write always accepts the entire slice and returns nil.
	_, _ = w.body.Write(data)
	return len(data), nil
}
