import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { EmailFamily, EmailFamilyMember } from '../../api/generated/models';
import RelatedCopies from './RelatedCopies.svelte';

function member(id: number, overrides: Partial<EmailFamilyMember> = {}): EmailFamilyMember {
  return {
    message_id: id,
    source_id: id,
    source_type: 'imap',
    source_identifier: `account-${id}@example.test`,
    conversation_id: id,
    draft: false,
    evidence: 'copy',
    linkage_outcome: 'complete',
    subject: `Copy ${id}`,
    ...overrides
  };
}

function family(members: EmailFamilyMember[], overrides: Partial<EmailFamily> = {}): EmailFamily {
  return { anchor_message_id: 1, members, truncated: false, pending_linkage: false, ...overrides };
}

const anchor = member(1, { evidence: 'anchor', conversation_id: 100 });

function serve(responses: (path: string) => Response | Promise<Response>) {
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    return responses(new URL(request.url).pathname);
  });
  return { fetchFn, client: createAPIClient(fetchFn) };
}

async function openPanel() {
  await fireEvent.click(screen.getByRole('button', { name: 'Related copies' }));
}

describe('RelatedCopies', () => {
  it('asks the daemon on every open and never while closed', async () => {
    const { fetchFn, client } = serve(() => Response.json(family([anchor, member(2)])));
    render(RelatedCopies, { props: { client, messageID: 1 } });

    expect(fetchFn).not.toHaveBeenCalled();
    const toggle = screen.getByRole('button', { name: 'Related copies' });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');

    await fireEvent.click(toggle);
    expect(screen.getByRole('status').textContent).toContain('Loading related copies');
    await screen.findByRole('link', { name: 'Copy 2' });
    expect(fetchFn).toHaveBeenCalledTimes(1);
    expect(new URL(fetchFn.mock.calls[0][0] instanceof Request ? fetchFn.mock.calls[0][0].url : String(fetchFn.mock.calls[0][0])).pathname)
      .toBe('/api/v1/messages/1/family');

    await openPanel();
    expect(screen.queryByRole('link', { name: 'Copy 2' })).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(1);
    await openPanel();
    await screen.findByRole('link', { name: 'Copy 2' });
    expect(fetchFn, 'reopening reads what the archive holds now').toHaveBeenCalledTimes(2);
  });

  it('leaves out the opened email and the messages the conversation view shows', async () => {
    const { client } = serve(() =>
      Response.json(family([anchor, member(2, { conversation_id: 100, subject: 'On screen' }), member(3, { subject: 'Elsewhere' })]))
    );
    render(RelatedCopies, { props: { client, messageID: 1, loadedIDs: [1, 2] } });
    await openPanel();

    await screen.findByRole('link', { name: 'Elsewhere' });
    expect(screen.queryByRole('link', { name: 'On screen' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Copy 1' })).toBeNull();
  });

  // The conversation view loads 25 messages on either side of the opened one,
  // so a reply further along has no card and needs its link here.
  it('links a conversation member the view has not loaded', async () => {
    const members = [member(26, { evidence: 'anchor', conversation_id: 100 })];
    for (let id = 1; id <= 52; id++) {
      if (id !== 26) members.push(member(id, { evidence: 'thread', conversation_id: 100, subject: `Message ${id}` }));
    }
    const { client } = serve(() => Response.json({ ...family(members), anchor_message_id: 26 }));
    const loaded = Array.from({ length: 51 }, (_, i) => i + 1);
    render(RelatedCopies, { props: { client, messageID: 26, loadedIDs: loaded } });
    await openPanel();

    await screen.findByRole('link', { name: 'Message 52' });
    expect(screen.queryByRole('link', { name: 'Message 51' })).toBeNull();
    expect(screen.queryByText(/No related copies found/)).toBeNull();
  });

  it('groups copies by account with direction and evidence', async () => {
    const { client } = serve(() =>
      Response.json(
        family([
          anchor,
          member(2, { account_address: 'work@example.test', account_path: 'sent', evidence: 'reply', subject: 'Re: Quote', sent_at: '2026-03-02T10:00:00Z' }),
          member(3, { account_address: 'work@example.test', account_path: 'inbound', evidence: 'copy', subject: 'Quote', sent_at: '2026-03-01T10:00:00Z' }),
          member(4, { account_address: 'home@example.test', source_type: 'gmail', account_path: 'sent', draft: true, evidence: 'forwarded_original', subject: '' }),
          member(5, { source_identifier: 'archive-5', evidence: 'indirect', subject: 'Later' }),
          member(6, { account_address: 'home@example.test', source_type: 'imap', subject: 'Via IMAP' })
        ])
      )
    );
    render(RelatedCopies, { props: { client, messageID: 1 } });
    await openPanel();
    await screen.findByRole('link', { name: 'Quote' });

    const groups = screen.getAllByRole('region').filter((region) => region.classList.contains('group'));
    // One account holding copies from two sources keeps a group, and a badge, for each.
    expect(groups.map((group) => group.getAttribute('aria-label'))).toEqual(['archive-5', 'home@example.test', 'home@example.test', 'work@example.test']);

    const work = within(groups[3]);
    const rows = work.getAllByRole('listitem');
    expect(rows.map((row) => row.textContent)).toEqual([
      expect.stringContaining('Quote'),
      expect.stringContaining('Re: Quote')
    ]);
    expect(rows[0].textContent).toContain('Received');
    expect(rows[0].textContent).toContain('Same message');
    expect(rows[1].textContent).toContain('Sent');
    expect(rows[1].textContent).toContain('Reply');

    const home = within(groups[1]);
    expect(home.getByRole('link', { name: '(no subject)' }).getAttribute('href')).toBe('/messages/4');
    expect(groups[1].textContent).toContain('Draft');
    expect(groups[1].textContent).toContain('Forwarded original');
    expect(groups[1].textContent).toContain('gmail');
    expect(groups[1].textContent).not.toContain('imap');
    expect(groups[2].textContent).toContain('imap');
    expect(groups[2].textContent).not.toContain('gmail');
    expect(groups[0].textContent).toContain('Related through another copy');
  });

  it('offers a retry after a failed request', async () => {
    let calls = 0;
    const { fetchFn, client } = serve(() =>
      ++calls === 1 ? Response.json({ error: 'email_family_unavailable', message: 'down' }, { status: 503 }) : Response.json(family([anchor, member(2)]))
    );
    render(RelatedCopies, { props: { client, messageID: 1 } });
    await openPanel();

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain("Couldn't load related copies.");
    await fireEvent.click(within(alert).getByRole('button', { name: 'Retry' }));
    await screen.findByRole('link', { name: 'Copy 2' });
    expect(fetchFn).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  const unreadable = (outcome: string) => [member(1, { evidence: 'anchor', conversation_id: 100, linkage_outcome: outcome })];
  const everySearchNote = 'No related copies found, but the search may be incomplete.';

  it.each([
    {
      name: 'nothing is related elsewhere',
      members: [anchor],
      shown: ['No related copies found.']
    },
    {
      name: 'every related email is already in the conversation',
      members: [anchor, member(2, { conversation_id: 100 })],
      loadedIDs: [1, 2],
      shown: ['Related emails are already shown in this conversation.'],
      hidden: [/No related copies found/]
    },
    {
      name: 'pending and unreadable copies and a truncated lookup',
      members: [
        member(1, { evidence: 'anchor', conversation_id: 100, linkage_outcome: 'pending' }),
        member(2, { linkage_outcome: 'invalid_headers' }),
        member(3, { linkage_outcome: 'pending', subject: 'Not indexed' })
      ],
      options: { truncated: true, truncated_reason: 'members' },
      shown: [
        'The lookup stopped at its 500-email limit, so some related emails may be missing.',
        "Some identifiers in this copy couldn't be read.",
        "This copy hasn't been indexed yet.",
        /This email hasn't been indexed yet\. The next sync indexes it; for a source that no longer syncs, run/,
        'msgvault repair-derived'
      ]
    },
    {
      name: 'a lookup that hit the identifier limit',
      members: [anchor, member(2)],
      options: { truncated: true, truncated_reason: 'identifiers' },
      shown: [/links to too many identifiers/],
      hidden: [/first 500/]
    },
    {
      name: 'other emails awaiting indexing',
      members: [anchor],
      options: { pending_linkage: true },
      shown: [/Some archived emails haven't been indexed yet.*for a source that no longer syncs, run/, everySearchNote],
      hidden: ['No related copies found.']
    },
    {
      name: 'the opened email being the pending one',
      members: [member(1, { evidence: 'anchor', conversation_id: 100, linkage_outcome: 'pending' })],
      options: { pending_linkage: true },
      shown: [/This email hasn't been indexed yet/],
      hidden: [/Some archived emails haven't been indexed yet/]
    },
    ...[
      ['invalid_headers', /Some identifiers in this email couldn't be read/],
      ['size_limit', /Some identifiers in this email couldn't be read/],
      ['missing_raw', /The original message isn't stored for this email/],
      ['unsupported_raw', /This email is stored in a format that can't be read/]
    ].map(([outcome, note]) => ({
      name: `an opened email that is ${outcome}`,
      members: unreadable(outcome as string),
      shown: [note as RegExp, everySearchNote],
      hidden: ['No related copies found.']
    })),
    {
      name: 'an unreadable member of the opened conversation that has no row',
      members: [
        member(1, { evidence: 'anchor', conversation_id: 100 }),
        member(2, { evidence: 'thread', conversation_id: 100, linkage_outcome: 'invalid_headers' })
      ],
      loadedIDs: [1, 2],
      shown: [/Some identifiers in this conversation couldn't be read/, 'Related emails are already shown in this conversation. The search may be incomplete.']
    }
  ] as Array<{
    name: string;
    members: EmailFamilyMember[];
    options?: Partial<EmailFamily>;
    loadedIDs?: number[];
    shown: Array<string | RegExp>;
    hidden?: Array<string | RegExp>;
  }>)('explains $name', async ({ members, options, loadedIDs, shown, hidden = [] }) => {
    const { client } = serve(() => Response.json(family(members, options)));
    render(RelatedCopies, { props: { client, messageID: 1, loadedIDs } });
    await openPanel();

    await screen.findByText(shown[0]);
    for (const text of shown) expect(screen.getByText(text), String(text)).toBeTruthy();
    for (const text of hidden) expect(screen.queryByText(text), String(text)).toBeNull();
  });

  it('drops a response for a message the reader has left', async () => {
    let release: (response: Response) => void = () => undefined;
    const { fetchFn, client } = serve((path) =>
      path === '/api/v1/messages/1/family'
        ? new Promise<Response>((resolve) => (release = resolve))
        : Response.json(family([member(2, { evidence: 'anchor', conversation_id: 100 })], { anchor_message_id: 2 }))
    );
    const { rerender } = render(RelatedCopies, { props: { client, messageID: 1 } });
    await openPanel();
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1));

    await rerender({ client, messageID: 2 });
    expect(screen.getByRole('button', { name: 'Related copies' }).getAttribute('aria-expanded')).toBe('false');

    await openPanel();
    await screen.findByText('No related copies found.');
    expect(fetchFn).toHaveBeenCalledTimes(2);

    // The abandoned response lands after the current one rendered.
    release(Response.json(family([anchor, member(9, { subject: 'Stale answer' })])));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(screen.queryByRole('link', { name: 'Stale answer' })).toBeNull();
    expect(screen.getByText('No related copies found.')).toBeTruthy();
  });
});
