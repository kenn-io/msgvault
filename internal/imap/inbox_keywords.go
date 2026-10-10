package imap

import (
	"context"
	"fmt"
	"slices"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func (p *InboxProvider) previewKeywords(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	if request.Tags == nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	change, err := emailtags.Normalize(*request.Tags, true)
	if err != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	keywords := append(slices.Clone(change.Add), change.Remove...)
	for _, keyword := range keywords {
		if validateKeyword(keyword) != nil {
			return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
		}
	}
	err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		selected, err := p.selectTarget(conn, before.Target, true)
		if err != nil {
			return err
		}
		permanent := make([]string, len(selected.PermanentFlags))
		for i, flag := range selected.PermanentFlags {
			permanent[i] = string(flag)
		}
		available := make([]string, len(selected.Flags))
		for i, flag := range selected.Flags {
			available[i] = string(flag)
		}
		for _, keyword := range keywords {
			namedPersistent := emailtags.Contains(permanent, keyword, true)
			canPersist := selected.PermanentFlags == nil || emailtags.Contains(permanent, string(imapapi.FlagWildcard), true) || namedPersistent
			// Wildcard persistence is not permission to provision a new keyword.
			if !canPersist || (!namedPersistent && !emailtags.Contains(available, keyword, true)) {
				return inboxcontrol.ErrUnavailable
			}
		}
		return nil
	})
	if err != nil {
		return inboxcontrol.State{}, err
	}
	projected := before
	projected.Tags = emailtags.Project(before.Tags, change, true)
	projected.Flags = slices.DeleteFunc(slices.Clone(before.Flags), func(flag string) bool { return emailtags.Contains(change.Remove, flag, true) })
	for _, keyword := range change.Add {
		if !containsIMAPFlag(projected.Flags, keyword) {
			projected.Flags = append(projected.Flags, keyword)
		}
	}
	slices.Sort(projected.Flags)
	return projected, nil
}

func (p *InboxProvider) dispatchKeywords(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	change, err := emailtags.Normalize(*request.Tags, true)
	if err != nil {
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	add, remove := emailtags.Delta(before.Tags, change, true)
	dispatched := false
	err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		if _, err := p.selectTarget(conn, before.Target, false); err != nil {
			return err
		}
		for _, delta := range []struct {
			keywords []string
			op       imapapi.StoreFlagsOp
		}{{add, imapapi.StoreFlagsAdd}, {remove, imapapi.StoreFlagsDel}} {
			if len(delta.keywords) == 0 {
				continue
			}
			flags := make([]imapapi.Flag, len(delta.keywords))
			for i, keyword := range delta.keywords {
				flags[i] = imapapi.Flag(keyword)
			}
			dispatched = true
			if _, err := conn.Store(imapapi.UIDSetNum(imapapi.UID(before.Target.UID)), &imapapi.StoreFlags{Op: delta.op, Silent: true, Flags: flags}, nil).Collect(); err != nil {
				return fmt.Errorf("store IMAP keywords: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		if dispatched {
			return inboxcontrol.DispatchResult{}, inboxcontrol.ErrOutcomeUnknown
		}
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	return inboxcontrol.DispatchResult{}, nil
}
