<script lang="ts">
  import type { ArchiveAttachment } from '../../archive/types';
  import { formatBytes } from '../../util/format';

  let { attachments }: { attachments: ArchiveAttachment[] } = $props();
  let failedAudio = $state<Record<string, boolean>>({});

  function contentURL(attachment: ArchiveAttachment): string | undefined {
    // Only local, content-addressed bytes are playable. Provider URLs can
    // expire or send the browser to a third party and are never used here.
    if (!/^[a-fA-F0-9]{64}$/.test(attachment.contentHash ?? '')) return undefined;
    return `/api/v1/attachments/${attachment.contentHash}/content`;
  }

  function playable(attachment: ArchiveAttachment): boolean {
    return ['audio/mpeg', 'audio/mp3', 'audio/wav', 'audio/x-wav', 'audio/wave',
      'audio/ogg', 'audio/mp4', 'audio/flac'].includes(attachment.mimeType.toLowerCase().split(';')[0]!.trim());
  }
</script>

{#if attachments.length > 0}
  <section class="message-attachments" aria-label="Attachments">
    <h3>Attachments</h3>
    <ul>
      {#each attachments as attachment}
        {@const url = contentURL(attachment)}
        {@const filename = attachment.filename || 'Unnamed attachment'}
        <li>
          <div class="attachment-heading">
            {#if url}
              <a href={url} download={filename} aria-label={`Download ${filename}`}>{filename}</a>
            {:else}
              <span>{filename}</span>
            {/if}
            <span class="attachment-size">{formatBytes(attachment.sizeBytes)}</span>
          </div>
          {#if url && playable(attachment)}
            <!-- svelte-ignore a11y_media_has_caption -- The archived call transcript, when available, is displayed in the message body; no synchronized captions are fabricated. -->
            <audio controls preload="none" src={url} aria-label={`Play ${filename}`}
              onerror={() => { failedAudio = { ...failedAudio, [url]: true }; }}>
              Your browser does not support audio playback. Download the recording to listen.
            </audio>
            {#if failedAudio[url]}
              <p role="alert">This recording could not be played. Try downloading it or syncing the source again.</p>
            {/if}
          {:else if !url}
            <p>Content has not been archived locally.</p>
          {/if}
        </li>
      {/each}
    </ul>
  </section>
{/if}

<style>
  .message-attachments { padding: var(--space-4); border-top: 1px solid var(--border-muted); }
  h3 { margin: 0 0 var(--space-3); font-size: var(--font-size-sm); }
  ul { list-style: none; padding: 0; margin: 0; display: grid; gap: var(--space-3); }
  li { min-width: 0; }
  .attachment-heading { display: flex; flex-wrap: wrap; gap: var(--space-2); align-items: baseline; }
  a { color: var(--link-ink); overflow-wrap: anywhere; }
  .attachment-size, p { color: var(--text-secondary); font-size: var(--font-size-xs); }
  p { margin: var(--space-2) 0 0; }
  audio { display: block; width: 100%; max-width: 32rem; margin-top: var(--space-2); }
</style>
