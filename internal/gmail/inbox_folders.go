package gmail

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func (p *InboxProvider) observeFolders(ctx context.Context, r inboxcontrol.Request) (inboxcontrol.State, error) {
	if r.Source == nil || *r.Source != p.source || r.Target != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrDenied
	}
	folders, err := p.Folders(ctx, *r.Source)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	state := inboxcontrol.State{Source: p.source, Folders: folders, ObservedAt: time.Now().UTC()}
	if r.ResolvedFolder != nil {
		if !slices.Contains(folders, *r.ResolvedFolder) {
			return state, inboxcontrol.ErrOutcomeUnknown
		}
		resolved := *r.ResolvedFolder
		state.ProvisionedFolder = &resolved
	} else if r.Destination != nil {
		for _, folder := range folders {
			if folder.Name == r.Destination.Name {
				resolvedFolder := folder
				state.ProvisionedFolder = &resolvedFolder
				break
			}
		}
	}
	return state, nil
}

func (p *InboxProvider) previewFolder(r inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	if r.Source == nil || *r.Source != p.source || before.Source != p.source || before.Target != (inboxcontrol.Target{}) || r.Destination == nil || r.Destination.Name == "" || r.Destination.ID != "" || r.Destination.ParentID != "" {
		return inboxcontrol.State{}, inboxcontrol.ErrInvalid
	}
	// Provisioning cannot create or shadow a protected system label.
	if _, err := emailtags.Normalize(emailtags.Change{Add: []string{r.Destination.Name}}, false); err != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrInvalid
	}
	projected := before
	if projected.ProvisionedFolder == nil {
		projected.ProvisionedFolder = &inboxcontrol.Folder{Name: r.Destination.Name}
	}
	if projected.ProvisionedFolder.Name != r.Destination.Name {
		return inboxcontrol.State{}, inboxcontrol.ErrPlanChanged
	}
	return projected, nil
}

func (p *InboxProvider) createFolder(ctx context.Context, r inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	projected, err := p.previewFolder(r, before)
	if err != nil {
		return inboxcontrol.DispatchResult{}, errors.Join(inboxcontrol.ErrNoWrite, err)
	}
	if projected.ProvisionedFolder.ID != "" {
		folder := *projected.ProvisionedFolder
		return inboxcontrol.DispatchResult{Folder: &folder}, nil
	}
	body, err := json.Marshal(struct {
		Name string `json:"name"`
	}{r.Destination.Name})
	if err != nil {
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	data, err := p.client.request(ctx, OpLabelsCreate, http.MethodPost, fmt.Sprintf("/users/%s/labels", url.PathEscape(p.client.userID)), body)
	if err != nil {
		if errors.Is(err, errWriteOutcomeUnknown) {
			return inboxcontrol.DispatchResult{}, inboxcontrol.ErrOutcomeUnknown
		}
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &created) != nil || !validFolderValue(created.ID) || created.Name != r.Destination.Name || created.Type != "user" {
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrOutcomeUnknown
	}
	folder := inboxcontrol.Folder{ID: created.ID, Name: created.Name}
	return inboxcontrol.DispatchResult{Folder: &folder}, nil
}

func (p *InboxProvider) verifyFolder(r inboxcontrol.Request, before, projected, after inboxcontrol.State) error {
	if before.Source != p.source || after.Source != p.source || after.Target != (inboxcontrol.Target{}) || projected.ProvisionedFolder == nil || after.ProvisionedFolder == nil || after.ProvisionedFolder.ID == "" || after.ProvisionedFolder.Name != projected.ProvisionedFolder.Name || r.ResolvedFolder == nil || *r.ResolvedFolder != *after.ProvisionedFolder {
		return inboxcontrol.ErrOutcomeUnknown
	}
	if projected.ProvisionedFolder.ID != "" && projected.ProvisionedFolder.ID != after.ProvisionedFolder.ID {
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

// Catalog absence is unknown, never evidence that a label can be provisioned.
func (p *InboxProvider) folderCatalog(ctx context.Context) ([]inboxcontrol.Folder, error) {
	data, err := p.client.request(ctx, OpLabelsList, http.MethodGet, fmt.Sprintf("/users/%s/labels", url.PathEscape(p.client.userID)), nil)
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	var response struct {
		Labels *[]struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"labels"`
	}
	if json.Unmarshal(data, &response) != nil || response.Labels == nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	folders := []inboxcontrol.Folder{}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, label := range *response.Labels {
		if !validFolderValue(label.ID) || !validFolderValue(label.Name) || (label.Type != "user" && label.Type != "system") || ids[label.ID] {
			return nil, inboxcontrol.ErrUnavailable
		}
		ids[label.ID] = true
		if label.Type == "user" {
			if names[label.Name] {
				return nil, inboxcontrol.ErrUnavailable
			}
			names[label.Name] = true
			folders = append(folders, inboxcontrol.Folder{ID: label.ID, Name: label.Name})
		}
	}
	return folders, nil
}

func validFolderValue(value string) bool {
	if len(value) > 4096 || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
