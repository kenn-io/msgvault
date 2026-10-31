<script lang="ts">
  import { Button, Card, Chip } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import { listLoops } from '../../api/generated/api/api';
  import type { LoopItem } from '../../api/generated/models';

  interface Props { client: APIClient; personID: number }
  let { client, personID }: Props = $props();
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

  function kindLabel(kind: string): string {
    return kind === 'reply_owed' ? 'Waiting on you' : 'Waiting on them';
  }

  function ageLabel(since: string): string {
    const seconds = Math.max(0, (Date.now() - Date.parse(since)) / 1000);
    if (seconds >= 86400) return `${Math.floor(seconds / 86400)}d`;
    if (seconds >= 3600) return `${Math.floor(seconds / 3600)}h`;
    return `${Math.floor(seconds / 60)}m`;
  }
</script>

<section aria-label="Open loops">
  <Card level="default" padding="sm">
    <div class="loops-card">
      <header>
        <h3>Open loops</h3>
        {#if !loading && !failed && items.length > 0}<span>{items.length}{truncated ? '+' : ''} open</span>{/if}
      </header>
      {#if loading}<p role="status">Loading open loops…</p>
      {:else if failed}<p role="alert">Couldn't load open loops.</p>
      {:else if items.length === 0 && coverageIncomplete}<p>No loops found in the messages checked.</p>
      {:else if items.length === 0}<p>Nothing open with this person.</p>
      {:else}
        <ul>
          {#each items as loop (loop.anchor_message_id)}
            <li>
              <div class="loop-meta">
                <Chip size="xs" tone="muted" uppercase={false}>{kindLabel(loop.kind)}</Chip>
                <span title={`Since ${loop.owed_since_at}`}>{ageLabel(loop.owed_since_at)}</span>
              </div>
              <a href={`/messages/${loop.anchor_message_id}`}>{loop.summary || '(no subject)'}</a>
            </li>
          {/each}
        </ul>
      {/if}
      {#if !loading && !failed && unreadable > 0}
        <p class="note">
          {unreadable === 1 ? '1 message' : `${unreadable} messages`} could not be read, so a reply in {unreadable === 1 ? 'it' : 'one'} may have settled a loop.
        </p>
      {/if}
      {#if !loading && !failed && coverageIncomplete}
        <p class="note">More archive history remains.</p>
        {#if checkMoreFailed}<p role="alert">Couldn't check more archive history.</p>{/if}
        <Button size="sm" surface="outline" label={checkingMore ? 'Checking…' : 'Check more'} disabled={checkingMore} onclick={checkMore} />
      {/if}
    </div>
  </Card>
</section>

<style>
  .loops-card { display: grid; gap: var(--space-3); min-width: 0; }
  header, .loop-meta { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); }
  h3, p, ul { margin: 0; }
  header > span, .loop-meta > span { color: var(--text-muted); font-size: var(--font-size-sm); }
  .note { color: var(--text-muted); font-size: var(--font-size-sm); }
  ul { display: grid; gap: var(--space-3); padding: 0; list-style: none; }
  li { display: grid; gap: var(--space-1); overflow-wrap: anywhere; }
</style>
