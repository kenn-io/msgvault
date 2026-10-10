<script lang="ts">
  import { Button, Chip } from '@kenn-io/kit-ui';
  import FileVolume from '@lucide/svelte/icons/file-volume';
  import type { APIClient } from '../../api/client';
  import { searchMedia } from '../../api/generated/api/api';
  import type { MediaSearchResponse, MediaSearchResult } from '../../api/generated/models';
  import { createStaleRequestGuard } from '../../archive/stale-request';
  import { formatDateTime, formatOffset, formatShortDate } from '../../util/format';

  let { client, query, supported, selectedMessageID, onOpen }: {
    client: APIClient; query: string; supported: boolean; selectedMessageID?: number; onOpen?: (hit: MediaSearchResult) => void;
  } = $props();
  const id = $props.id();
  let result = $state<MediaSearchResponse>();
  const admitted = $derived(query.trim().split(/\s+/u).every(term => /^[\p{L}\p{M}\p{N}]+$/u.test(term) && /[\p{L}\p{N}]/u.test(term) && !/^(AND|OR|NOT)$/i.test(term)));
  const incomplete = $derived(Boolean(result && (result.partial || result.truncated)));
  let notice = $state('');
  let expired = $state(false);
  let retryable = $state(false);
  let retry = $state(0);

  $effect(() => {
    const requestedQuery = query.trim();
    const requestedClient = client;
    const allowed = supported && admitted;
    void retry;
    result = undefined;
    notice = '';
    expired = false;
    retryable = false;
    if (!allowed || !requestedQuery) return;

    const freshness = createStaleRequestGuard();
    let controller: AbortController | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let disposed = false;
    let blocked = false;

    function stop(): void {
      freshness.invalidate();
      controller?.abort();
      controller = undefined;
      if (timer !== undefined) clearTimeout(timer);
      timer = undefined;
    }

    function expire(): void {
      expired = Boolean(result);
      stop();
      result = undefined;
      notice = expired ? '' : 'Recording search timed out.';
      retryable = !expired;
    }

    async function load(): Promise<void> {
      if (disposed || document.hidden || controller) return;
      stop();
      result = undefined;
      notice = '';
      expired = false;
      retryable = false;
      const generation = freshness.begin();
      controller = new AbortController();
      const deadline = Date.now() + 30_000;
      timer = setTimeout(expire, 30_000);
      const signal = controller.signal;
      try {
        const { data, error, response } = await searchMedia(
          { q: requestedQuery, mode: 'lexical', limit: 20 },
          { ...requestedClient, signal },
        );
        if (!freshness.isCurrent(generation) || disposed) return;
        if (Date.now() >= deadline) {
          expire();
          return;
        }
        if (response.status >= 400 && response.status < 500 && response.status !== 429) {
          blocked = true;
          notice = error?.error === 'media_search_scope_limit'
            ? 'This archive exceeds browser recording-search limits. Use person-scoped recording search in the CLI or API.'
            : error?.error === 'invalid_media_search'
              ? 'Recording search rejected this query. Try different plain words.'
              : error?.error === 'invalid_parameter'
                ? 'Recording search query is too long. Use fewer words.'
              : 'Recording search cannot read this query. Check archive access or change the query.';
          return;
        }
        if (error?.error === 'media_search_unavailable') {
          notice = 'Recording search unavailable.';
          retryable = true;
          return;
        }
        if (signal.aborted || !data?.results || !data.coverage) throw new Error('Transcript search unavailable');
        result = data;
      } catch {
        if (!freshness.isCurrent(generation) || disposed) return;
        result = undefined;
        notice = 'Could not load recording matches.';
        retryable = true;
      } finally {
        if (freshness.isCurrent(generation) && !disposed) {
          controller = undefined;
          if (!result && timer !== undefined) clearTimeout(timer);
        }
      }
    }

    function refresh(): void {
      if (document.hidden) {
        stop();
        result = undefined;
      } else if (!blocked) {
        void load();
      }
    }
    document.addEventListener('visibilitychange', refresh);
    timer = setTimeout(() => void load(), 300);
    return () => {
      disposed = true;
      stop();
      document.removeEventListener('visibilitychange', refresh);
    };
  });

</script>

<section class="transcript-hits" aria-label="Spoken in recordings" data-scroll>
  <header>
    <h2>Recordings{#if result && result.results.length > 0}<span class="count">{result.results.length}</span>{/if}</h2>
    {#if result || expired}<Button label="Refresh" size="sm" surface="soft" onclick={() => retry += 1} />{/if}
  </header>
  {#if !supported}
    <p role="status">Recording search requires Full text with no filters or grouping.</p>
  {:else if !admitted}
    <p role="status">Recording search supports plain words only. Operators, punctuation and quoted phrases are unavailable.</p>
  {:else if expired}
    <p role="status">Recording results expired.</p>
  {:else if notice}
    <div class="unavailable" role="status">
      <span>{notice}</span>
      {#if retryable}<Button label="Retry" size="sm" surface="soft" onclick={() => retry += 1} />{/if}
    </div>
  {:else if result}
    {#if incomplete}
      <p class="coverage" role="status">
        {#if result.results.length === 0}No matches in searched transcripts.{:else if result.partial}Coverage incomplete.{/if}
        {#if result.pending_occurrences}<span>{result.pending_occurrences} pending</span>{/if}
        {#if result.unavailable_occurrences}<span>{result.unavailable_occurrences} unavailable</span>{/if}
        {#if result.attribution_unavailable}<span>{result.attribution_unavailable} unattributed</span>{/if}
        {#if result.truncated}<span>More may match</span>{/if}
      </p>
    {/if}
    <ol>
      {#each result.results as hit, index (index)}
        <li>
          <a href={`/messages/${hit.message_id}`} aria-current={selectedMessageID === hit.message_id ? 'true' : undefined}
            aria-labelledby={`${id}-title-${index}`} aria-describedby={`${id}-meta-${index} ${id}-excerpt-${index}`}
            onclick={(event) => {
              if (!onOpen || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
              event.preventDefault();
              onOpen(hit);
            }}>
            <span class="recording-icon" aria-hidden="true"><FileVolume size={16} /></span>
            <strong id={`${id}-title-${index}`} title={hit.containing_title}>{hit.containing_title || 'Untitled message'}</strong>
            {#if hit.occurred_at}<time class="date" datetime={hit.occurred_at} title={formatDateTime(hit.occurred_at)}>{formatShortDate(hit.occurred_at)}</time>{/if}
            <span class="provenance" id={`${id}-meta-${index}`}>
              <span class="filename" title={hit.filename}>{hit.filename || 'Unnamed recording'}</span>
              <Chip size="xs" tone="neutral" uppercase={false}>{hit.origin === 'supplied' ? 'Provider transcript' : 'Generated transcript'}</Chip>
            </span>
            <span class="transcript" class:timed={hit.start_ms !== undefined}>
              {#if hit.start_ms !== undefined}<time datetime={`PT${hit.start_ms / 1000}S`} title={hit.end_ms === undefined ? undefined : `Until ${formatOffset(hit.end_ms)}`}>{formatOffset(hit.start_ms)}</time>{/if}
              <span class="excerpt" id={`${id}-excerpt-${index}`}>{hit.excerpt}</span>
            </span>
          </a>
        </li>
      {/each}
    </ol>
    {#if result.results.length === 0 && !incomplete}<p>No spoken matches.</p>{/if}
  {:else}
    <p role="status">Searching recordings…</p>
  {/if}
</section>

<style>
  .transcript-hits {
    flex: 0 1 auto;
    min-height: 80px;
    max-height: 40%;
    overflow: auto;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
  }

  header, h2, .provenance, .unavailable {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  header { justify-content: space-between; min-height: 28px; }
  h2 { margin: 0; font-size: var(--font-size-sm); font-weight: var(--font-weight-semibold); }
  .count, time, p, .provenance { color: var(--text-secondary); font-size: var(--font-size-xs); }
  .count { font-weight: var(--font-weight-normal); font-variant-numeric: tabular-nums; }
  p { margin: var(--space-2) 0 0; }
  ol { margin: var(--space-2) 0 0; padding: 0; list-style: none; }
  li + li { border-top: 1px solid var(--border-muted); }
  a { display: grid; grid-template-columns: 28px minmax(0, 1fr) auto; column-gap: var(--space-3); row-gap: var(--space-1); padding: var(--space-2); color: var(--text-primary); text-decoration: none; border-radius: var(--radius-sm); }
  a:hover strong { text-decoration: underline; }
  a:focus-visible { outline: 2px solid var(--text-primary); outline-offset: -2px; }
  a[aria-current='true'] { background: var(--selected-bg); box-shadow: inset 2px 0 var(--accent-blue); }
  strong { align-self: center; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: var(--font-size-sm); font-weight: var(--font-weight-semibold); }
  .recording-icon { display: grid; width: 28px; height: 28px; place-items: center; border-radius: var(--radius-md); background: var(--bg-inset); color: var(--text-secondary); }
  .date { align-self: center; white-space: nowrap; }
  .provenance { grid-column: 2 / -1; flex-wrap: wrap; min-width: 0; }
  .filename { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .transcript { grid-column: 2 / -1; display: grid; grid-template-columns: minmax(0, 1fr); max-width: 72ch; padding-left: var(--space-3); border-left: 2px solid var(--border-muted); margin-top: var(--space-1); }
  .transcript.timed { grid-template-columns: 3.5rem minmax(0, 1fr); column-gap: var(--space-3); }
  .transcript time { padding-top: 0.2em; }
  .excerpt { display: -webkit-box; line-clamp: 3; -webkit-line-clamp: 3; -webkit-box-orient: vertical; overflow: hidden; overflow-wrap: anywhere; font-size: var(--font-size-sm); line-height: 1.5; white-space: pre-wrap; }
  time { font-variant-numeric: tabular-nums; }
  .coverage { display: flex; flex-wrap: wrap; gap: var(--space-1) var(--space-2); }
  .coverage span + span::before { content: '·'; margin-right: var(--space-2); }
  @media (max-width: 640px) { a { column-gap: var(--space-2); padding-inline: 0; } .transcript { grid-column: 1 / -1; } .transcript.timed { grid-template-columns: 3rem minmax(0, 1fr); } }
</style>
