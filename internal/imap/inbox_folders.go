package imap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func (p *InboxProvider) observeFolders(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.State, error) {
	if request.Source == nil || request.Target != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrDenied
	}
	folders, err := p.Folders(ctx, *request.Source)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	state := inboxcontrol.State{Source: p.source, Folders: folders, ObservedAt: time.Now().UTC()}
	if request.ResolvedFolder != nil {
		if !slices.Contains(folders, *request.ResolvedFolder) {
			return state, inboxcontrol.ErrOutcomeUnknown
		}
		folder := *request.ResolvedFolder
		state.ProvisionedFolder = &folder
	} else if request.Destination != nil {
		for _, folder := range folders {
			if folder.Name == request.Destination.Name {
				state.ProvisionedFolder = &folder
				break
			}
		}
	}
	return state, nil
}

func (p *InboxProvider) previewFolder(request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	if request.Source == nil || *request.Source != p.source || before.Source != p.source || before.Target != (inboxcontrol.Target{}) || request.Destination == nil || request.Destination.Name == "" || request.Destination.ID != "" || request.Destination.ParentID != "" {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	projected := before
	if projected.ProvisionedFolder == nil {
		projected.ProvisionedFolder = &inboxcontrol.Folder{Name: request.Destination.Name}
	}
	if projected.ProvisionedFolder.Name != request.Destination.Name {
		return inboxcontrol.State{}, inboxcontrol.ErrPlanChanged
	}
	return projected, nil
}

func (p *InboxProvider) createFolder(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	projected, err := p.previewFolder(request, before)
	if err != nil {
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	if projected.ProvisionedFolder.ID != "" {
		folder := *projected.ProvisionedFolder
		return inboxcontrol.DispatchResult{Folder: &folder}, nil
	}
	dispatched := false
	var result inboxcontrol.DispatchResult
	err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		dispatched = true
		if err := conn.Create(request.Destination.Name, nil).Wait(); err != nil {
			if response, ok := errors.AsType[*imapapi.Error](err); ok && (response.Type == imapapi.StatusResponseTypeNo || response.Type == imapapi.StatusResponseTypeBad) {
				dispatched = false
			}
			return fmt.Errorf("create IMAP folder: %w", err)
		}
		// CREATE has no epoch in its response. Establish the exact native epoch;
		// losing this evidence requires manual recovery, never a name-based guess.
		status, err := conn.Status(request.Destination.Name, &imapapi.StatusOptions{UIDValidity: true}).Wait()
		if err != nil || status.Mailbox != request.Destination.Name || status.UIDValidity == 0 {
			return inboxcontrol.ErrOutcomeUnknown
		}
		result.Folder = &inboxcontrol.Folder{ID: status.Mailbox, Name: status.Mailbox, UIDValidity: status.UIDValidity}
		return nil
	})
	if err != nil {
		if dispatched {
			return result, inboxcontrol.ErrOutcomeUnknown
		}
		return result, inboxcontrol.ErrNoWrite
	}
	return result, nil
}

func (p *InboxProvider) verifyFolder(request inboxcontrol.Request, before, projected, after inboxcontrol.State) error {
	if before.Source != p.source || after.Source != p.source || after.Target != (inboxcontrol.Target{}) || projected.ProvisionedFolder == nil || after.ProvisionedFolder == nil || request.ResolvedFolder == nil || *request.ResolvedFolder != *after.ProvisionedFolder || after.ProvisionedFolder.ID == "" || after.ProvisionedFolder.UIDValidity == 0 || after.ProvisionedFolder.Name != projected.ProvisionedFolder.Name {
		return inboxcontrol.ErrOutcomeUnknown
	}
	if projected.ProvisionedFolder.ID != "" && *projected.ProvisionedFolder != *after.ProvisionedFolder {
		return inboxcontrol.ErrOutcomeUnknown
	}
	for _, folder := range before.Folders {
		if !slices.Contains(after.Folders, folder) {
			return inboxcontrol.ErrOutcomeUnknown
		}
	}
	if !slices.Contains(after.Folders, *after.ProvisionedFolder) {
		return inboxcontrol.ErrOutcomeUnknown
	}
	return nil
}
