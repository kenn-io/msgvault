<script lang="ts">
  import DetailSection from '../people/DetailSection.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import { Button } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import { formatContactDate } from '../../directory/labels';
  import { listLoops } from '../../api/generated/api/api';
  import type { LoopItem } from '../../api/generated/models';

  // refreshKey changes when the person's identities change, so the list reloads.
  interface Props { client: APIClient; personID: number; refreshKey?: string }
  let { client, personID, refreshKey }: Props = $props();
  const limit = 5;
  let items = $state<LoopItem[]>([]);
  let loading = $state(true);
  let checkingMore = $state(false);
  let checkMoreFailed = $state(false);
  let failed = $state(false);
  let truncated = $state(false);
  let coverageIncomplete = $state(false);
  let unreadable = $state(0);
  let checkMore = $state<() => void>(() => {});

  $effect(() => {
    const selectedID = personID;
    const selectedClient = client;
    void refreshKey;
    const controller = new AbortController();
    checkMoreFailed = false;
    checkingMore = false;
    async function load(more = false) {
      if (more) {
        checkingMore = true;
        checkMoreFailed = false;
      } else {
        loading = true;
        items = [];
        truncated = false;
        coverageIncomplete = false;
        unreadable = 0;
        failed = false;
      }
      try {
        const result = await listLoops(
          { person_id: selectedID, limit },
          { ...selectedClient, signal: controller.signal },
        );
        if (controller.signal.aborted) return;
        if (!result.data) throw new Error('Open loops are unavailable.');
        items = result.data.items ?? [];
        truncated = Boolean(result.data.truncated);
        coverageIncomplete = Boolean(result.data.coverage_incomplete);
        unreadable = result.data.skipped_unparseable ?? 0;
      } catch {
        if (controller.signal.aborted) return;
        if (more) checkMoreFailed = true;
        else failed = true;
      } finally {
        if (!controller.signal.aborted) {
          if (more) checkingMore = false;
          else loading = false;
        }
      }
    }
    checkMore = () => void load(true);
    void load();
    return () => controller.abort();
  });

  const groups = $derived([
    { kind: 'reply_owed', label: 'Waiting on you', items: items.filter((loop) => loop.kind === 'reply_owed') },
    { kind: 'awaiting_reply', label: 'Waiting on them', items: items.filter((loop) => loop.kind !== 'reply_owed') },
  ]);

  function ageLabel(since: string): string {
    const seconds = Math.max(0, (Date.now() - Date.parse(since)) / 1000);
    if (seconds >= 86400) return `${Math.floor(seconds / 86400)}d`;
    if (seconds >= 3600) return `${Math.floor(seconds / 3600)}h`;
    return `${Math.floor(seconds / 60)}m`;
  }
</script>

<DetailSection title="Open loops">
  {#snippet control()}{#if !loading && !failed && items.length > 0}<span class="muted">{items.length}{truncated ? '+' : ''} open</span>{/if}{/snippet}
  <div class="open-loops">
  {#if loading}<p class="muted" role="status">Loading…</p>
  {:else if failed}<InlineNotice message="Couldn't load open loops." />
  {:else if items.length === 0 && coverageIncomplete}<p class="muted">No loops found in the messages checked.</p>
  {:else if items.length === 0}<p class="muted">Nothing open with this person.</p>
  {:else}
    {#each groups as group (group.kind)}
      {#if group.items.length > 0}
        <div class="group">
          <h4>{group.label}</h4>
          <ul>
            {#each group.items as loop (loop.anchor_message_id)}
              <li>
                <a href={`/messages/${loop.anchor_message_id}`} title={loop.summary || '(no subject)'}>{loop.summary || '(no subject)'}</a>
                <span class="muted" title={`Since ${formatContactDate(loop.owed_since_at)}`}>{ageLabel(loop.owed_since_at)}</span>
              </li>
            {/each}
          </ul>
        </div>
      {/if}
    {/each}
  {/if}
  {#if !loading && !failed && unreadable > 0}
    <p class="muted">
      {unreadable === 1 ? '1 message' : `${unreadable} messages`} could not be read, so a reply in {unreadable === 1 ? 'it' : 'one'} may have settled a loop.
    </p>
  {/if}
  {#if !loading && !failed && coverageIncomplete}
    {#if checkMoreFailed}<p role="alert">Couldn't check more archive history.</p>{/if}
    <div><Button size="sm" label={checkingMore ? 'Checking…' : 'Check more'} disabled={checkingMore} onclick={checkMore} /></div>
  {/if}
</div>
</DetailSection>

<style>
  .open-loops { display: grid; gap: var(--space-2); min-width: 0; }
  h4, p, ul { margin: 0; }
  h4 { color: var(--text-muted); font-size: var(--font-size-sm); font-weight: 500; }
  .group { display: grid; gap: var(--space-1); }
  ul { display: grid; gap: var(--space-1); padding: 0; list-style: none; }
  li { display: flex; align-items: baseline; justify-content: space-between; gap: var(--space-3); min-width: 0; }
  a { min-width: 0; overflow: hidden; color: var(--link-ink); text-decoration: none; text-overflow: ellipsis; white-space: nowrap; }
  a:hover { text-decoration: underline; }
  li > span { flex: none; }
  .muted { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  [role="alert"] { color: var(--text-danger); }
</style>
