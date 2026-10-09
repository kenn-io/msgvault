<script lang="ts">
  import { Button, Chip } from '@kenn-io/kit-ui';
  import FileVolume from '@lucide/svelte/icons/file-volume';
  import type { APIClient } from '../../api/client';
  import { searchMedia } from '../../api/generated/api/api';
  import type { MediaSearchResponse } from '../../api/generated/models';
  import { createStaleRequestGuard } from '../../archive/stale-request';
  import { formatDateTime, formatOffset } from '../../util/format';

  let { client, query, supported }: { client: APIClient; query: string; supported: boolean } = $props();
  let result = $state<MediaSearchResponse>();
  const admitted = $derived(query.trim().split(/\s+/u).every(term => /^[\p{L}\p{M}\p{N}]+$/u.test(term) && /[\p{L}\p{N}]/u.test(term) && !/^(AND|OR|NOT)$/i.test(term)));
  const incomplete = $derived(Boolean(result && (result.partial || result.coverage.state !== 'complete' || result.coverage.binding_required || result.pending_occurrences || result.unavailable_occurrences || result.attribution_unavailable || result.truncated)));
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
    window.addEventListener('focus', refresh);
    timer = setTimeout(() => void load(), 300);
    return () => {
      disposed = true;
      stop();
      document.removeEventListener('visibilitychange', refresh);
      window.removeEventListener('focus', refresh);
    };
  });

</script>

<section class="transcript-hits" aria-label="Spoken in recordings" data-scroll>
  <header>
    <h2><FileVolume size={16} aria-hidden="true" />Spoken in recordings</h2>
    {#if result}<span class="count">{result.results.length} {result.results.length === 1 ? 'match' : 'matches'}</span>{/if}
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
        {#if result.partial || result.coverage.state !== 'complete' || result.coverage.binding_required}Coverage incomplete.{/if}
        {#if result.pending_occurrences}{' '}{result.pending_occurrences} pending.{/if}
        {#if result.unavailable_occurrences}{' '}{result.unavailable_occurrences} unavailable.{/if}
        {#if result.attribution_unavailable}{' '}{result.attribution_unavailable} with unavailable attribution.{/if}
        {#if result.truncated}{' '}More matches may exist.{/if}
      </p>
    {/if}
    <ol>
      {#each result.results as hit, index (index)}
        <li>
          <a href={`/messages/${hit.message_id}`}>
            <span class="source">
              <strong>{hit.containing_title || `Message ${hit.message_id}`}</strong>
              {#if hit.occurred_at}<time datetime={hit.occurred_at}>{formatDateTime(hit.occurred_at)}</time>{/if}
            </span>
            <span class="provenance">
              <span class="filename">{hit.filename || 'Unnamed recording'}</span>
              <Chip size="xs" tone="neutral" uppercase={false}>{hit.origin === 'supplied' ? 'Provider transcript' : 'Generated transcript'}</Chip>
              {#if hit.start_ms !== undefined}<time datetime={`PT${hit.start_ms / 1000}S`}>{formatOffset(hit.start_ms)}{#if hit.end_ms !== undefined}{' to '}{formatOffset(hit.end_ms)}{/if}</time>{/if}
            </span>
            <span class="excerpt">{hit.excerpt}</span>
          </a>
        </li>
      {/each}
    </ol>
    {#if result.results.length === 0}<p>{incomplete ? 'No matching excerpts returned.' : 'No spoken matches.'}</p>{/if}
  {:else}
    <p role="status">Searching recordings…</p>
  {/if}
</section>

<style>
  .transcript-hits {
    flex: none;
    max-height: 30vh;
    overflow: auto;
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-surface);
  }

  header, h2, .source, .provenance, .unavailable {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  header, .source { justify-content: space-between; }
  h2 { margin: 0; font-size: var(--font-size-sm); font-weight: var(--font-weight-semibold); }
  .count, time, p, .provenance { color: var(--text-secondary); font-size: var(--font-size-xs); }
  p { margin: var(--space-2) 0 0; }
  ol { margin: var(--space-2) 0 0; padding: 0; list-style: none; }
  li + li { border-top: 1px solid var(--border-muted); }
  a { display: grid; gap: var(--space-1); padding: var(--space-2) 0; color: var(--text-primary); text-decoration: none; }
  a:hover strong { text-decoration: underline; }
  a:focus-visible { outline: 2px solid var(--text-primary); outline-offset: 2px; }
  strong { font-size: var(--font-size-sm); font-weight: var(--font-weight-semibold); }
  .source, .provenance { flex-wrap: wrap; }
  .filename, strong, .excerpt { overflow-wrap: anywhere; }
  .excerpt { font-size: var(--font-size-sm); line-height: 1.5; white-space: pre-wrap; }
  time { font-variant-numeric: tabular-nums; }
</style>
