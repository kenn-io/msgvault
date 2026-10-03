<script lang="ts">
  import { listMessageRecordings } from '../../api/generated/api/api';
  import type { MessageRecording, MessageRecordingState } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';
  import { formatBytes } from '../../util/format';

  let { client, messageId }: { client: APIClient; messageId: number } = $props();

  let recordings = $state<MessageRecording[]>([]);
  let error = $state('');

  const stateLabels: Record<Exclude<MessageRecordingState, 'ready'>, string> = {
    processing: 'Transcript is still processing.',
    missing: 'No transcript for this recording.',
    failed: 'Transcription failed.',
    unsupported: "This audio format can't be transcribed.",
    media_missing: "The recording's audio is missing from the archive.",
    unavailable: 'Transcript unavailable.',
  };

  $effect(() => {
    const requestedMessage = messageId;
    const controller = new AbortController();
    recordings = [];
    error = '';
    void load(requestedMessage, controller);
    return () => controller.abort();
  });

  async function load(requestedMessage: number, controller: AbortController): Promise<void> {
    try {
      const { data, response } = await listMessageRecordings(
        { id: requestedMessage },
        { ...client, signal: controller.signal },
      );
      if (controller.signal.aborted) return;
      if (!data) {
        error = `Could not load recordings (${response.status})`;
        return;
      }
      recordings = data.recordings ?? [];
    } catch {
      if (controller.signal.aborted) return;
      error = 'Could not load recordings';
    }
  }

  function summary(recording: MessageRecording): string {
    if (recording.state !== 'ready') return stateLabels[recording.state];
    const origin = recording.transcript?.origin === 'generated' ? 'Generated transcript' : 'Provider transcript';
    return recording.transcript?.partial ? `${origin} · Partial` : origin;
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
          <div class="recording-file">
            <strong>{recording.filename || '(unnamed recording)'}</strong>
            <span>{formatBytes(recording.size_bytes)}</span>
          </div>
          <p class="recording-state">{summary(recording)}</p>
          {#if recording.state === 'ready' && recording.transcript}
            <div class="transcript">
              {#each recording.transcript.units as unit, index (index)}
                <p>{#if unit.start_ms !== undefined}<time data-mono>{offset(unit.start_ms)}</time>{' '}{/if}{#if unit.speaker}<strong>{unit.speaker}</strong>{' '}{/if}{unit.text}</p>
              {:else}
                <p class="transcript-empty">The transcript has no text.</p>
              {/each}
            </div>
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
    grid-template-columns: minmax(0, 1fr);
    gap: var(--space-1);
    padding: var(--space-3) 0;
    border-top: 1px solid var(--border-muted);
  }

  .recording-file {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
  }

  .recording-file strong {
    min-width: 0;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .recording-file span {
    flex: none;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  p {
    margin: 0;
  }

  .recording-state {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  /* Transcript lines use the same reading type and measure as the message body. */
  .transcript {
    display: grid;
    max-width: 680px;
    gap: var(--space-1);
    margin-top: var(--space-2);
    overflow-wrap: break-word;
    color: var(--text-primary);
    font-family: var(--font-sans);
    font-size: 14px;
    line-height: 1.55;
    white-space: pre-wrap;
  }

  .transcript time,
  .transcript-empty {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }
</style>
