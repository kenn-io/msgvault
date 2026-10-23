<script lang="ts">
  import { untrack } from 'svelte';
  import { admitsMediaQuery, attachmentScopeNotice, refusedMediaSearchNotice } from '../../search/media-query';
  import { Button, Chip } from '@kenn-io/kit-ui';
  import FileText from '@lucide/svelte/icons/file-text';
  import FileVolume from '@lucide/svelte/icons/file-volume';
  import type { APIClient } from '../../api/client';
  import { searchMedia } from '../../api/generated/api/api';
  import type { MediaSearchResponse, MediaSearchResult, MediaAttachmentResult } from '../../api/generated/models';
  import { createStaleRequestGuard } from '../../archive/stale-request';
  import { formatDateTime, formatOffset, formatShortDate, formatTranscriptOrigin } from '../../util/format';

  let { client, query, supported, participantID, selectedMessageID, missingMessageID, onOpen }: {
    client: APIClient; query: string; supported: boolean; participantID?: number; missingMessageID?: number; selectedMessageID?: number; onOpen?: (hit: MediaSearchResult | MediaAttachmentResult) => void;
  } = $props();
  const id = $props.id();
  let result = $state<MediaSearchResponse>();
  const admitted = $derived(admitsMediaQuery(query));
  const incomplete = $derived(Boolean(result && (result.partial || result.truncated)));
  const recordingNotice = $derived(result?.recordings_error ? refusedMediaSearchNotice(result.recordings_error.error, result.recordings_error.message, query, participantID) : '');
  const recordingRetryable = $derived(result?.recordings_error?.error === 'media_search_unavailable');
  const attachmentRetryable = $derived(['transport', 'server_error', 'timeout', 'rate_limited'].includes(result?.attachments?.error?.error ?? ''));
  let notice = $state('');
  let expired = $state(false);
  let retryable = $state(false);
  let retry = $state(0);
  let previousRetry = 0;

  function openHit(event: MouseEvent, hit: MediaSearchResult | MediaAttachmentResult): void {
    if (!onOpen || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    onOpen(hit);
  }

  $effect(() => {
    const missingID = missingMessageID;
    const current = untrack(() => result);
    if (missingID !== undefined && current) result = { ...current, results: current.results.filter(hit => hit.message_id !== missingID), attachments: current.attachments ? { ...current.attachments, results: current.attachments.results.filter(hit => hit.message_id !== missingID) } : undefined };
  });

  $effect(() => {
    const requestedQuery = query.trim();
    const requestedClient = client;
    const requestedParticipantID = participantID;
    const allowed = supported && admitted && Boolean(requestedQuery);
    const retryRequested = retry !== previousRetry;
    previousRetry = retry;
    result = undefined;
    notice = '';
    expired = false;
    retryable = false;
    if (!allowed) return;

    const freshness = createStaleRequestGuard();
    let controller: AbortController | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
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
      if (document.hidden || controller) return;
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
          { q: requestedQuery, mode: 'lexical', limit: 20, participant_id: requestedParticipantID },
          { ...requestedClient, signal },
        );
        if (!freshness.isCurrent(generation)) return;
        if (Date.now() >= deadline) {
          expire();
          return;
        }
        if (response.status >= 400 && response.status < 500 && response.status !== 429) {
          blocked = true;
          notice = refusedMediaSearchNotice(error?.error, error?.message, requestedQuery, requestedParticipantID);
          return;
        }
        if (error?.error === 'media_search_unavailable') {
          notice = 'Recording search unavailable.';
          retryable = true;
          return;
        }
        if (!data?.results || !data.coverage) throw new Error('Transcript search unavailable');
        const missingID = untrack(() => missingMessageID);
        result = { ...data, results: data.results.filter(hit => hit.message_id !== missingID), attachments: data.attachments ? { ...data.attachments, results: data.attachments.results.filter(hit => hit.message_id !== missingID) } : undefined };
      } catch {
        if (!freshness.isCurrent(generation)) return;
        result = undefined;
        notice = 'Could not load recording matches.';
        retryable = true;
      } finally {
        if (freshness.isCurrent(generation)) {
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
    if (retryRequested) void load();
    else timer = setTimeout(() => { timer = undefined; void load(); }, 300);
    return () => {
      stop();
      document.removeEventListener('visibilitychange', refresh);
    };
  });

</script>

<section class="transcript-hits" aria-label="Media search" tabindex="-1" data-recording-hits data-scroll>
  <section aria-label="Recordings">
    <header>
      <h2>Recordings{#if result && admitted && result.results.length > 0}<span class="count">{result.results.length}</span>{/if}</h2>
      {#if result || expired}<Button label="Refresh" size="sm" surface="soft" onclick={() => retry += 1} />{/if}
    </header>
    {#if !supported}
      <p role="status">Media search requires Full text with no grouping and at most one Person filter.</p>
    {:else if expired}
      <p role="status">Recording results expired.</p>
    {:else if notice}
      <div class="unavailable" role="status">
        <span>{notice}</span>
        {#if retryable}<Button label="Retry" size="sm" surface="soft" onclick={() => retry += 1} />{/if}
      </div>
    {:else if !admitted}
      <p role="status">Media search supports plain words only. Punctuation and quoted phrases are unavailable.</p>
    {:else if result}
      {#if incomplete && !recordingNotice}
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
              onclick={(event) => openHit(event, hit)}>
              <span class="recording-icon" aria-hidden="true"><FileVolume size={16} /></span>
              <strong id={`${id}-title-${index}`} title={hit.containing_title}>{hit.containing_title || 'Untitled message'}</strong>
              {#if hit.occurred_at}<time class="date" datetime={hit.occurred_at} title={formatDateTime(hit.occurred_at)}>{formatShortDate(hit.occurred_at)}</time>{/if}
              <span class="provenance" id={`${id}-meta-${index}`}>
                <span class="filename" title={hit.filename}>{hit.filename || 'Unnamed recording'}</span>
                <Chip size="xs" tone="neutral" uppercase={false}>{formatTranscriptOrigin(hit.origin)}</Chip>
              </span>
              <span class="transcript" class:timed={hit.start_ms !== undefined}>
                {#if hit.start_ms !== undefined}<time datetime={`PT${hit.start_ms / 1000}S`} title={hit.end_ms === undefined ? undefined : `Until ${formatOffset(hit.end_ms)}`}>{formatOffset(hit.start_ms)}</time>{/if}
                <span class="excerpt" id={`${id}-excerpt-${index}`}>{hit.excerpt}</span>
              </span>
            </a>
          </li>
        {/each}
      </ol>
      {#if recordingNotice}<div class="unavailable" role="status">
        <span>{recordingNotice}</span>
        {#if recordingRetryable}<Button label="Retry" size="sm" surface="soft" onclick={() => retry += 1} />{/if}
      </div>
      {:else if result.results.length === 0 && !incomplete}<p>No spoken matches.</p>{/if}
    {:else}
      <p role="status">Searching recordings…</p>
    {/if}
  </section>
  {#if result?.attachments}
    <section class="attachments" aria-label="Attachments">
      <h2>Attachments{#if !result.attachments.error || result.attachments.results.length > 0}<span class="count">{result.attachments.results.length}</span>{/if}</h2>
      {#if result.attachments.error}<div class="unavailable" role="status"><span>{result.attachments.error.error === 'media_search_scope_limit' ? attachmentScopeNotice(participantID) : result.attachments.error.message || 'Attachment search is unavailable.'}</span>{#if attachmentRetryable}<Button label="Retry" size="sm" surface="soft" onclick={() => retry += 1} />{/if}</div>{/if}
      <ol>
        {#each result.attachments.results as hit, index (index)}
          <li>
            <a href={`/messages/${hit.message_id}`} aria-current={selectedMessageID === hit.message_id && !result.results.some(recording => recording.message_id === hit.message_id) ? 'true' : undefined}
              aria-labelledby={`${id}-attachment-title-${index}`} aria-describedby={`${id}-attachment-meta-${index} ${id}-attachment-excerpt-${index}`}
              onclick={(event) => openHit(event, hit)}>
              <span class="recording-icon" aria-hidden="true"><FileText size={16} /></span>
              <strong id={`${id}-attachment-title-${index}`} title={hit.containing_title}>{hit.containing_title || 'Untitled message'}</strong>
              {#if hit.occurred_at}<time class="date" datetime={hit.occurred_at} title={formatDateTime(hit.occurred_at)}>{formatShortDate(hit.occurred_at)}</time>{/if}
              <span class="provenance" id={`${id}-attachment-meta-${index}`}><span class="filename" title={hit.filename}>{hit.filename || 'Unnamed attachment'}</span></span>
              <span class="transcript"><span class="excerpt" id={`${id}-attachment-excerpt-${index}`}>{hit.excerpt}</span></span>
            </a>
          </li>
        {/each}
      </ol>
      {#if result.attachments.results.length === 0 && !result.attachments.error}<p>No matches in text Docbank has extracted from {result.attachments.truncated ? 'searched' : 'mirrored'} attachments.</p>{/if}
      {#if result.attachments.truncated}<p class="coverage" role="status">More may match</p>{/if}
    </section>
  {/if}
</section>

<style>
  .transcript-hits {
    container-type: inline-size;
    flex: 0 1 auto;
    min-height: 0;
    max-height: min(40%, 320px);
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
  .attachments { margin-top: var(--space-3); padding-top: var(--space-3); border-top: 1px solid var(--border-default); }
  .unavailable { margin-top: var(--space-2); color: var(--text-danger); font-size: var(--font-size-xs); }
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
  @container (max-width: 640px) { a { column-gap: var(--space-2); padding-inline: 0; } .transcript { grid-column: 1 / -1; } .transcript.timed { grid-template-columns: 3rem minmax(0, 1fr); } }
  @container results (max-height: 200px) { .transcript-hits { display: none; } }
</style>
