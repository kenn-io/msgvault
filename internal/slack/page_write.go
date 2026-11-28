package slack

import (
	"context"
	"fmt"
)

// writePage runs a page's message writes in one source write transaction.
//
// processMessage issues about ten store writes per message, and each one
// committed on its own: on a WAL database every commit rewrites every b-tree
// page it touched (the message, body, raw, recipient, FTS and change-journal
// indexes) and fsyncs. A backfill wrote hundreds of kilobytes per message that
// way. One transaction per page writes each touched page once per page.
//
// The transaction must not wait on the network or create participants, so
// preparePage first resolves every participant the page names and downloads
// its files. write receives the transaction's context; store calls made with
// it join the transaction.
func (imp *Importer) writePage(ctx context.Context, cc *convScope, msgs []Message, write func(context.Context) error) error {
	// A file preparePage stored for a message the page then skipped stays
	// pending in its row; it is fetched again when that message is processed.
	defer clear(imp.fetched)
	if err := imp.preparePage(ctx, cc, msgs); err != nil {
		return err
	}
	return imp.store.WithSourceWriteTxContext(ctx, cc.sourceID, write)
}

// processPage persists a fetched page's messages in one transaction.
func (imp *Importer) processPage(ctx context.Context, cc *convScope, msgs []Message, sum *ImportSummary) error {
	return imp.writePage(ctx, cc, msgs, func(ctx context.Context) error {
		for i := range msgs {
			if err := imp.processMessage(ctx, cc, &msgs[i], sum); err != nil {
				return err
			}
		}
		return nil
	})
}

// preparePage does the page's participant and file work that cannot run
// inside the write transaction: the resolver caches every sender, bot,
// mention and reaction user, and fetchPageFiles stores the files to fetch.
func (imp *Importer) preparePage(ctx context.Context, cc *convScope, msgs []Message) error {
	for i := range msgs {
		m := &msgs[i]
		if m.Type != "message" || m.TS == "" {
			continue
		}
		var err error
		if m.User != "" {
			_, err = imp.res.resolveID(ctx, m.User)
		} else if m.BotID != "" {
			_, err = imp.res.resolveBot(ctx, m.BotID, m.Username)
		}
		if err != nil {
			return err
		}
		for _, uid := range m.MentionedUserIDs() {
			if _, err := imp.res.resolveID(ctx, uid); err != nil {
				return err
			}
		}
		for _, rc := range m.Reactions {
			for _, uid := range rc.Users {
				if _, err := imp.res.resolveID(ctx, uid); err != nil {
					return err
				}
			}
		}
	}
	if cc.filesHandledExternally {
		return nil
	}
	if err := imp.fetchPageFiles(ctx, cc, msgs); err != nil {
		return fmt.Errorf("fetch page files: %w", err)
	}
	return nil
}
