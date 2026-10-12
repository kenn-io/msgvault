package imap

import (
	"context"
	"errors"
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
		if request.ResolvedFolder.ID == "" || request.ResolvedFolder.UIDValidity == 0 || !slices.Contains(folders, *request.ResolvedFolder) {
			return state, inboxcontrol.ErrOutcomeUnknown
		}
		folder := *request.ResolvedFolder
		state.ProvisionedFolder = &folder
	} else if request.Destination != nil {
		for _, folder := range folders {
			if folder.Name == request.Destination.Name {
				folder := folder
				state.ProvisionedFolder = &folder
				break
			}
		}
	}
	return state, nil
}

func (p *InboxProvider) previewFolder(request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	probe := inboxcontrol.Request{Operation: inboxcontrol.OpCreateFolder, Source: request.Source, Destination: request.Destination, DryRun: true}
	if probe.Validate() != nil || !p.validSource(*request.Source) || before.Source != p.source || before.Target != (inboxcontrol.Target{}) || before.ObservedAt.IsZero() || before.Folders == nil || request.Destination.ParentID != "" {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	projected := before
	if before.ProvisionedFolder != nil {
		if before.ProvisionedFolder.Name != request.Destination.Name || before.ProvisionedFolder.ID != request.Destination.Name || before.ProvisionedFolder.UIDValidity == 0 || !slices.Contains(before.Folders, *before.ProvisionedFolder) {
			return inboxcontrol.State{}, inboxcontrol.ErrPlanChanged
		}
	} else {
		projected.ProvisionedFolder = &inboxcontrol.Folder{Name: request.Destination.Name}
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
	result := inboxcontrol.DispatchResult{}
	err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		dispatched = true
		if err := conn.Create(request.Destination.Name, nil).Wait(); err != nil {
			if response, ok := errors.AsType[*imapapi.Error](err); ok && (response.Type == imapapi.StatusResponseTypeNo || response.Type == imapapi.StatusResponseTypeBad) {
				dispatched = false
			}
			return err
		}
		// CREATE itself returns no epoch. Losing STATUS evidence leaves the receipt
		// unknown; a later mailbox with the same name is not dispatch proof.
		status, err := conn.Status(request.Destination.Name, &imapapi.StatusOptions{UIDValidity: true}).Wait()
		if err != nil || status == nil || status.Mailbox != request.Destination.Name || status.UIDValidity == 0 {
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
	if request.Source == nil || *request.Source != p.source || before.Source != p.source || after.Source != p.source || after.Target != (inboxcontrol.Target{}) || projected.ProvisionedFolder == nil || after.ProvisionedFolder == nil || request.ResolvedFolder == nil || *request.ResolvedFolder != *after.ProvisionedFolder || after.ProvisionedFolder.ID == "" || after.ProvisionedFolder.UIDValidity == 0 || after.ProvisionedFolder.Name != projected.ProvisionedFolder.Name || !slices.Contains(after.Folders, *after.ProvisionedFolder) {
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
	return nil
}
