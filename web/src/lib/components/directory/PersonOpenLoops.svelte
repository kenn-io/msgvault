<script lang="ts">
  import { Card, Chip } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import { listPersonLoops } from '../../api/generated/api/api';
  import type { Loop } from '../../api/generated/models';

  interface Props { client: APIClient; personID: number }
  let { client, personID }: Props = $props();
  let items = $state<Loop[]>([]);
  let loading = $state(true);
  let stale = $state(false);
  let truncated = $state(false);
  let error = $state('');
  let generation = 0;

  $effect(() => {
    const selectedID = personID;
    const selectedClient = client;
    const current = ++generation;
    const controller = new AbortController();
    items = [];
    stale = false;
    truncated = false;
    error = '';
    loading = true;
    void (async () => {
      try {
        const result = await listPersonLoops({ id: selectedID }, { limit: 5 }, { ...selectedClient, signal: controller.signal });
        if (current !== generation || controller.signal.aborted) return;
        if (!result.data) throw new Error(result.error?.message ?? 'Open loops are unavailable.');
        items = result.data.items ?? [];
        stale = result.data.freshness.stale;
        truncated = Boolean(result.data.next_cursor);
      } catch (cause) {
        if (current !== generation || controller.signal.aborted) return;
        error = cause instanceof Error ? cause.message : 'Open loops are unavailable.';
      } finally {
        if (current === generation && !controller.signal.aborted) loading = false;
      }
    })();
    return () => controller.abort();
  });

  function kindLabel(kind: string): string {
    if (kind === 'reply_owed') return 'Waiting on you';
    if (kind === 'awaiting_reply') return 'Waiting on them';
    return kind.replaceAll('_', ' ');
  }

  function ageLabel(seconds: number): string {
    if (seconds >= 86400) return `${Math.floor(seconds / 86400)}d`;
    if (seconds >= 3600) return `${Math.floor(seconds / 3600)}h`;
    return `${Math.max(0, Math.floor(seconds / 60))}m`;
  }

  function safeIssueURL(value: string | undefined): string | undefined {
    if (!value) return undefined;
    try {
      const url = new URL(value);
      return ['https:', 'http:'].includes(url.protocol) && !url.username && !url.password ? url.href : undefined;
    } catch {
      return undefined;
    }
  }
</script>

<section aria-label="Open loops">
  <Card level="default" padding="sm">
    <div class="loops-card">
      <header><h3>Open loops</h3>
        {#if !loading && !error}<span>{items.length}{truncated ? '+' : ''} open {items.length === 1 && !truncated ? 'loop' : 'loops'}</span>{/if}
      </header>
      {#if loading}<p>Loading open loops…</p>
      {:else if error}<p role="alert">{error}</p>
      {:else}
        {#if stale}<p class="stale" role="status">Archive may be stale. Check source sync and loop progress before acting.</p>{/if}
        {#if items.length === 0}<p>No open loops with this person.</p>
        {:else}<ul>
          {#each items as loop (loop.id)}
            <li>
              <div class="loop-meta"><Chip size="xs" tone="muted" uppercase={false}>{kindLabel(loop.kind)}</Chip>
                <span title={`Owed since ${loop.owed_since_at}`}>{ageLabel(loop.age_seconds)}</span></div>
              {#if loop.anchor_message_id != null}<a href={`/messages/${loop.anchor_message_id}`}>{loop.summary || 'Open archived conversation'}</a>
              {:else}<span>{loop.summary || 'Archived conversation'}</span>{/if}
              {#if loop.kata}
                {@const issueURL = safeIssueURL(loop.kata.web_url)}
                <div class="kata" title={loop.kata.checked_at ? `Kata checked ${loop.kata.checked_at}` : 'Kata never checked'}>
                  {#if issueURL}<a href={issueURL} target="_blank" rel="noreferrer">{loop.kata.qualified_ref}</a>
                  {:else}<span>{loop.kata.qualified_ref}</span>{/if}
                  <span>{loop.kata.status}</span>
                </div>
              {/if}
            </li>
          {/each}
        </ul>{/if}
      {/if}
    </div>
  </Card>
</section>

<style>
  .loops-card { display: grid; gap: var(--space-3); min-width: 0; }
  header, .loop-meta { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); }
  h3, p, ul { margin: 0; }
  header > span, .loop-meta > span { color: var(--text-muted); font-size: var(--font-size-sm); }
  ul { display: grid; gap: var(--space-3); padding: 0; list-style: none; }
  li { display: grid; gap: var(--space-1); overflow-wrap: anywhere; }
  .stale { color: var(--status-warning-ink); }
  .kata { display: flex; align-items: center; gap: var(--space-2); font-size: var(--font-size-sm); }
  .kata > span { color: var(--text-muted); }
</style>
