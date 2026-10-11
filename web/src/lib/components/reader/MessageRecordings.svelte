<script lang="ts">
  import { Chip } from '@kenn-io/kit-ui';
  import FileVolume from '@lucide/svelte/icons/file-volume';
  import Hourglass from '@lucide/svelte/icons/hourglass';
  import CircleAlert from '@lucide/svelte/icons/circle-alert';
  import Info from '@lucide/svelte/icons/info';
  import { listMessageRecordings } from '../../api/generated/api/api';
  import type { MessageRecording, MessageRecordingState } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';
  import { createStaleRequestGuard } from '../../archive/stale-request';
  import { formatBytes } from '../../util/format';

  let { client, messageId, message }: { client: APIClient; messageId: number; message?: object } = $props();

  let recordings = $state<MessageRecording[]>([]);
  let error = $state('');

  const stateLabels: Record<Exclude<MessageRecordingState, 'ready'>, string> = {
    processing: 'Transcript is still processing.',
    missing: 'No transcript for this recording.',
    failed: 'Transcription failed.',
    unsupported: "This recording isn't supported for transcription.",
    media_missing: "The recording's audio is missing from the archive.",
    unavailable: 'Transcript unavailable.',
  };

  $effect(() => {
    const requestedMessage = messageId;
    const requestedClient = client;
    void message;
    let controller: AbortController | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const freshness = createStaleRequestGuard();
    let delay = 2000;
    let disposed = false;
    recordings = [];
    error = '';

    function stop(): void {
      freshness.invalidate();
      controller?.abort();
      controller = undefined;
      if (timer !== undefined) clearTimeout(timer);
      timer = undefined;
    }

    async function load(): Promise<void> {
      if (disposed || document.hidden || controller) return;
      if (timer !== undefined) clearTimeout(timer);
      timer = undefined;
      const requestGeneration = freshness.begin();
      const requestController = new AbortController();
      controller = requestController;
      const signal = AbortSignal.any([requestController.signal, AbortSignal.timeout(30_000)]);
      try {
        const { data, response } = await listMessageRecordings(
          { id: requestedMessage },
          { ...requestedClient, signal },
        );
        if (!freshness.isCurrent(requestGeneration) || disposed) return;
        if (signal.aborted) throw new Error('Request timed out');
        if (!data) {
          recordings = [];
          error = `Could not load recordings (${response.status})`;
          delay = Math.min(30_000, delay * 2);
          return;
        }
        const next = data.recordings ?? [];
        delay = JSON.stringify(next) === JSON.stringify(recordings) ? Math.min(30_000, delay * 2) : 2000;
        recordings = next;
        error = '';
      } catch {
        if (!freshness.isCurrent(requestGeneration) || disposed) return;
        recordings = [];
        error = 'Could not load recordings';
        delay = Math.min(30_000, delay * 2);
      } finally {
        if (freshness.isCurrent(requestGeneration) && !disposed) {
          controller = undefined;
          if (!document.hidden) timer = setTimeout(() => void load(), delay);
        }
      }
    }

    function refresh(): void {
      if (document.hidden) {
        stop();
      } else {
        delay = 2000;
        void load();
      }
    }
    document.addEventListener('visibilitychange', refresh);
    window.addEventListener('focus', refresh);
    void load();
    return () => {
      disposed = true;
      stop();
      document.removeEventListener('visibilitychange', refresh);
      window.removeEventListener('focus', refresh);
    };
  });

  function summary(recording: MessageRecording): string {
    return recording.transcript?.origin === 'generated' ? 'Generated transcript' : 'Provider transcript';
  }

  function offset(ms: number): string {
    const seconds = Math.floor(ms / 1000);
    return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
  }
</script>

{#if error}
  <p class="recordings-error" role="alert">{error}</p>
{:else if recordings.length > 0}
  <section class="recordings" aria-label="Recordings">
    <ol>
      <!-- An attachment can briefly have two live revisions, so the row index is the key. -->
      {#each recordings as recording, row (row)}
        <li>
          <span class="recording-icon" aria-hidden="true"><FileVolume size={16} /></span>
          <div class="recording-file">
            <strong title={recording.filename}>{recording.filename || '(unnamed recording)'}</strong>
            <span>{formatBytes(recording.size_bytes)}</span>
          </div>
          {#if recording.state === 'ready'}
            <p class="recording-state" role="status">{summary(recording)}{#if recording.transcript?.partial}{' · '}<Chip size="xs" tone="warning" uppercase={false}>Partial</Chip>{/if}</p>
            {#if recording.transcript}
              <div class="transcript" class:timed={recording.transcript.units.some(unit => unit.start_ms !== undefined)}>
                {#each recording.transcript.units as unit, index (index)}
                  <p>{#if unit.start_ms !== undefined}<time data-mono datetime={`PT${unit.start_ms / 1000}S`}>{offset(unit.start_ms)}</time>{' '}{:else}<span aria-hidden="true"></span>{/if}<span class="unit-body">{#if unit.speaker}<strong>{unit.speaker}</strong>{/if}{unit.text}</span></p>
                {:else}
                  <p class="transcript-empty">The transcript has no text.</p>
                {/each}
              </div>
            {/if}
          {:else}
            <p class="recording-status" data-state={recording.state} role="status">
              <span class="status-icon" aria-hidden="true">
                {#if recording.state === 'processing'}<Hourglass size={14} />{:else if recording.state === 'failed'}<CircleAlert size={14} />{:else}<Info size={14} />{/if}
              </span>
              {stateLabels[recording.state]}
            </p>
          {/if}
        </li>
      {/each}
    </ol>
  </section>
{/if}

<style>
  .recordings {
    padding: 0 var(--space-4) var(--space-4);
  }

  .recordings-error {
    margin: 0;
    padding: 0 var(--space-4) var(--space-4);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  /* minmax(0, 1fr) lets a long filename truncate instead of widening the card. */
  ol {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    display: grid;
    grid-template-columns: 28px minmax(0, 1fr);
    column-gap: var(--space-3);
    row-gap: var(--space-1);
    padding: var(--space-3) 0;
    border-top: 1px solid var(--border-muted);
  }

  .recording-icon {
    display: grid;
    width: 28px;
    height: 28px;
    place-items: center;
    background: var(--bg-inset);
    border-radius: var(--radius-md);
    color: var(--text-secondary);
  }

  .recording-file {
    grid-column: 2 / -1;
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-4);
  }

  .recording-file strong {
    min-width: 0;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-semibold);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .recording-file span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  p {
    margin: 0;
  }

  .recording-state {
    grid-column: 2 / -1;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  /* Transcript lines use the same reading type and measure as the message body. */
  .transcript {
    display: grid;
    max-width: 680px;
    grid-column: 2 / -1;
    gap: var(--space-2);
    padding-left: var(--space-3);
    border-left: 2px solid var(--border-muted);
    margin-top: var(--space-2);
    overflow-wrap: break-word;
    color: var(--text-primary);
    font-family: var(--font-sans);
    font-size: 14px;
    line-height: 1.55;
    white-space: pre-wrap;
  }

  .transcript p {
    display: grid;
    grid-template-columns: var(--transcript-time, 0) minmax(0, 1fr);
  }

  .transcript.timed {
    --transcript-time: 3.5rem;
  }

  .transcript.timed p {
    column-gap: var(--space-3);
  }

  .transcript time {
    padding-top: 0.2em;
    font-size: var(--font-size-xs);
  }

  .unit-body strong {
    display: block;
    margin-bottom: 2px;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
  }

  .transcript .transcript-empty {
    display: block;
    font-style: italic;
  }

  .recording-status {
    grid-column: 2 / -1;
    display: flex;
    align-items: flex-start;
    gap: var(--space-2);
    margin-top: var(--space-2);
    padding: var(--space-2) var(--space-3);
    background: var(--bg-inset);
    border-radius: var(--radius-md);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .status-icon {
    flex: none;
    margin-top: 1px;
    color: var(--text-muted);
  }

  .recording-status[data-state='failed'] .status-icon {
    color: var(--status-error-ink);
  }

  .transcript time,
  .transcript-empty {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }

  @media (max-width: 640px) {
    li {
      grid-template-columns: 24px minmax(0, 1fr);
      column-gap: var(--space-2);
    }

    .recording-icon {
      width: 24px;
      height: 24px;
    }

    .recording-icon :global(svg) {
      width: 14px;
      height: 14px;
    }

    .transcript,
    .recording-status {
      grid-column: 1 / -1;
    }

    .transcript {
      padding-left: var(--space-2);
    }

    .transcript.timed {
      --transcript-time: 3rem;
    }
  }
</style>
