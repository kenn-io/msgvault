<script lang="ts">
  import { Button, Chip, Notice, Spinner } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import { getEmailFamily } from '../../api/generated/api/api';
  import type { EmailFamily, EmailFamilyMember } from '../../api/generated/models';
  import { createStaleRequestGuard } from '../../archive/stale-request';
  import { formatDateTime } from '../../util/format';

  // loadedIDs are the messages the conversation view already shows; those have no row here.
  let { client, messageID, loadedIDs = [] }: { client: APIClient; messageID: number; loadedIDs?: number[] } = $props();

  let open = $state(false);
  let loading = $state(false);
  let failed = $state(false);
  let family = $state<EmailFamily>();

  const freshness = createStaleRequestGuard();
  let controller: AbortController | undefined;

  const evidenceLabels: Record<string, string> = {
    copy: 'Same message',
    reply: 'Reply',
    replied_to: 'Replied to',
    forward: 'Forwarded',
    forwarded_original: 'Forwarded original',
    references: 'Shared references',
    indirect: 'Related through another copy',
    thread: 'Same thread'
  };

  // Another message opens a fresh panel, and leaving cancels any request.
  $effect(() => {
    void messageID;
    void client;
    open = false;
    loading = false;
    failed = false;
    family = undefined;
    return () => {
      freshness.invalidate();
      controller?.abort();
      controller = undefined;
    };
  });

  async function load(): Promise<void> {
    const generation = freshness.begin();
    controller?.abort();
    const request = new AbortController();
    controller = request;
    loading = true;
    failed = false;
    family = undefined;
    try {
      const { data, response } = await getEmailFamily({ id: messageID }, { ...client, signal: request.signal });
      freshness.commit(generation, () => {
        if (data && response.ok) family = data;
        else failed = true;
      });
    } catch {
      freshness.commit(generation, () => {
        failed = true;
      });
    } finally {
      freshness.commit(generation, () => {
        loading = false;
        controller = undefined;
      });
    }
  }

  // Every open asks again, so the list never shows what an earlier sync has since changed.
  function toggle(): void {
    open = !open;
    if (open) void load();
    else {
      freshness.invalidate();
      controller?.abort();
      controller = undefined;
      loading = false;
    }
  }

  const anchor = $derived(family?.members.find((member) => member.message_id === family?.anchor_message_id));
  const anchorOutcome = $derived(anchor?.linkage_outcome ?? 'complete');
  const anchorIncomplete = $derived(anchorOutcome !== 'complete');
  // The anchor's own pending note already covers it; this is about other emails.
  const othersPending = $derived(!!family?.pending_linkage && anchorOutcome !== 'pending');
  // A member the conversation view shows has no row here, so its unreadable
  // headers need a note of their own.
  const hiddenUnreadable = $derived(
    (family?.members ?? []).some(
      (member) =>
        member.message_id !== family?.anchor_message_id &&
        loadedIDs.includes(member.message_id) &&
        member.linkage_outcome !== 'complete' &&
        member.linkage_outcome !== 'pending'
    )
  );
  // Every returned member counts, hidden or not.
  const searchIncomplete = $derived(
    !!family?.pending_linkage || (family?.members ?? []).some((member) => member.linkage_outcome !== 'complete')
  );

  const truncationNotice = $derived.by(() => {
    if (family?.truncated_reason === 'members') return 'The lookup stopped at its 500-email limit, so some related emails may be missing.';
    if (family?.truncated_reason === 'identifiers') {
      return 'This email links to too many identifiers to follow them all, so some related emails may be missing.';
    }
    return 'Some related emails may be missing.';
  });

  function timeOf(member: EmailFamilyMember): number {
    const time = member.sent_at ? Date.parse(member.sent_at) : NaN;
    return Number.isFinite(time) ? time : Infinity;
  }

  // Messages the conversation view has loaded are already on screen.
  const shown = $derived((family?.members ?? []).filter((member) => member.message_id !== family?.anchor_message_id && loadedIDs.includes(member.message_id)));

  // One account can hold copies from several sources, so each source keeps its own group and badge.
  const groups = $derived.by(() => {
    const byAccount = new Map<string, { key: string; label: string; sourceType: string; members: EmailFamilyMember[] }>();
    for (const member of family?.members ?? []) {
      if (member.message_id === family?.anchor_message_id) continue;
      if (loadedIDs.includes(member.message_id)) continue;
      const label = member.account_address || member.source_identifier;
      const key = `${label}\n${member.source_type}`;
      const group = byAccount.get(key) ?? { key, label, sourceType: member.source_type, members: [] };
      group.members.push(member);
      byAccount.set(key, group);
    }
    return [...byAccount.values()]
      .sort((a, b) => a.label.localeCompare(b.label) || a.sourceType.localeCompare(b.sourceType))
      .map((group) => ({
        ...group,
        members: group.members.sort((a, b) => timeOf(a) - timeOf(b) || a.message_id - b.message_id)
      }));
  });

  // Where the original message is missing or in an unreadable format, say so; later raw storage fixes it.
  function unreadableNote(outcome: string, what: 'copy' | 'email'): string {
    if (outcome === 'missing_raw') return `The original message isn't stored for this ${what}`;
    if (outcome === 'unsupported_raw') return `This ${what} is stored in a format that can't be read`;
    return `Some identifiers in this ${what} couldn't be read`;
  }

  function direction(member: EmailFamilyMember): string {
    if (member.draft) return 'Draft';
    if (member.account_path === 'sent') return 'Sent';
    if (member.account_path === 'inbound') return 'Received';
    return '';
  }
</script>

<Button size="sm" surface="soft" label="Related copies" ariaExpanded={open} onclick={toggle} />

{#if open}
  <section class="related-copies" aria-label="Copies of this email in other conversations">
    {#if loading}
      <p class="state" role="status"><span aria-hidden="true"><Spinner size={14} label="" /></span> Loading related copies…</p>
    {:else if failed}
      <div class="state" role="alert">
        <span>Couldn't load related copies.</span>
        <Button size="sm" surface="soft" label="Retry" onclick={() => void load()} />
      </div>
    {:else if family}
      {#if family.truncated}
        <div class="notice"><Notice tone="info" message={truncationNotice} /></div>
      {/if}
      {#if groups.length === 0}
        <p class="state">
          {shown.length > 0
            ? `Related emails are already shown in this conversation.${searchIncomplete ? ' The search may be incomplete.' : ''}`
            : searchIncomplete
              ? 'No related copies found, but the search may be incomplete.'
              : 'No related copies found.'}
        </p>
      {/if}
      {#each groups as group (group.key)}
        <section class="group" aria-label={group.label}>
          <h4 class="group-heading">
            <span class="account">{group.label}</span>
            <Chip size="xs" tone="neutral" uppercase={false}>{group.sourceType}</Chip>
          </h4>
          <ul>
            {#each group.members as member (member.message_id)}
              <li>
                <div class="row">
                  <a class="subject" href={`/messages/${member.message_id}`}>{member.subject || '(no subject)'}</a>
                  {#if member.sent_at}<time datetime={member.sent_at} data-mono>{formatDateTime(member.sent_at)}</time>{/if}
                  {#if direction(member)}<Chip size="xs" tone="neutral" uppercase={false}>{direction(member)}</Chip>{/if}
                  {#if evidenceLabels[member.evidence]}
                    <Chip size="xs" tone="info" uppercase={false}>{evidenceLabels[member.evidence]}</Chip>
                  {/if}
                </div>
                {#if member.linkage_outcome !== 'complete'}
                  <p class="incomplete">
                    {member.linkage_outcome === 'pending' ? "This copy hasn't been indexed yet." : `${unreadableNote(member.linkage_outcome, 'copy')}.`}
                  </p>
                {/if}
              </li>
            {/each}
          </ul>
        </section>
      {/each}
      {#if anchorOutcome === 'pending'}
        <p class="incomplete">
          This email hasn't been indexed yet. The next sync indexes it; for a source that no longer syncs, run <code>msgvault repair-derived</code>.
        </p>
      {:else if anchorIncomplete}
        <p class="incomplete">{unreadableNote(anchorOutcome, 'email')}, so related copies may be missing.</p>
      {/if}
      {#if hiddenUnreadable}
        <p class="incomplete">Some identifiers in this conversation couldn't be read, so related copies may be missing.</p>
      {/if}
      {#if othersPending}
        <p class="incomplete">
          Some archived emails haven't been indexed yet, so related copies may be missing. The next sync indexes them; for a source that no longer syncs, run <code>msgvault repair-derived</code>.
        </p>
      {/if}
    {/if}
  </section>
{/if}

<style>
  .related-copies {
    flex: 1 1 100%;
    min-width: 0;
    max-height: 320px;
    margin-top: calc(var(--space-3) - var(--space-2));
    padding: var(--space-3) 0;
    overflow-y: auto;
    border-block: 1px solid var(--border-muted);
    font-size: var(--font-size-sm);
  }

  .state {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin: 0;
    color: var(--text-secondary);
  }

  .notice {
    margin-bottom: var(--space-3);
  }

  .group + .group {
    margin-top: var(--space-3);
  }

  .group-heading {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin: 0 0 var(--space-2);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-semibold);
  }

  .account {
    min-width: 0;
    overflow-wrap: anywhere;
  }

  ul {
    display: grid;
    gap: var(--space-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .row {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    column-gap: var(--space-3);
    row-gap: var(--space-1);
  }

  /* The basis keeps the subject on a line of its own at phone width. */
  .subject {
    flex: 1 1 16rem;
    min-width: 0;
    overflow-wrap: anywhere;
  }

  time {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .incomplete {
    margin: var(--space-1) 0 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .related-copies > .incomplete {
    margin-top: var(--space-3);
  }
</style>
