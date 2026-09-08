package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
)

// Keep save completion separate from actions that own the view's loading state.
type saveMessageResultMsg ExportResultMsg

// SaveMessage saves the archived email, including MIME attachments, in the
// current directory. Capture the destination and identity before queuing I/O.
func (c *ActionController) SaveMessage(detail *query.MessageDetail) tea.Cmd {
	if detail == nil {
		return nil
	}
	id, messageType := detail.ID, detail.MessageType
	dir, dirErr := os.Getwd()
	return func() tea.Msg {
		fail := func(err error) saveMessageResultMsg {
			return saveMessageResultMsg{Title: "Save Failed", Err: err}
		}
		if dirErr != nil {
			return fail(fmt.Errorf("get current directory: %w", dirErr))
		}
		if messageType != "" && messageType != "email" {
			return fail(errors.New("saving as .eml is only available for email messages"))
		}
		raw, err := c.queries.GetMessageRaw(context.Background(), id)
		if err != nil {
			return fail(fmt.Errorf("read message: %w", err))
		}
		if len(raw) == 0 {
			return fail(errors.New("raw email is not available in the archive"))
		}
		// Use only the local numeric ID, never untrusted headers, in the name.
		name := filepath.Join(dir, fmt.Sprintf("message-%d.eml", id))
		file, path, err := export.CreateExclusiveFile(name, 0o600)
		if err != nil {
			return fail(fmt.Errorf("create message file: %w", err))
		}
		_, writeErr := io.Copy(file, bytes.NewReader(raw))
		if err := errors.Join(writeErr, file.Close()); err != nil {
			removeErr := os.Remove(path)
			return fail(fmt.Errorf("write message file: %w", errors.Join(err, removeErr)))
		}
		return saveMessageResultMsg{Title: "Message Saved", Result: "Saved to:\n" + path}
	}
}
