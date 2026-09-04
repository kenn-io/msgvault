<script lang="ts" module>
  export const MAX_THUMBNAIL_BYTES = 5 * 1024 * 1024;
</script>

<script lang="ts">
  import { getFileContent as generatedGetFileContent } from '../../api/generated/api/api';
  import type { APIClient } from '../../api/client';
  import type { PersonFileSearchRow } from '../../explore/models';
  import { isSupportedThumbnailMIME, readBoundedStream, validatedThumbnailBlob } from './preview-bytes';
  import { withThumbnailSlot } from './thumbnail-queue';

  interface Props {
    client: APIClient;
    file: PersonFileSearchRow;
  }

  let { client, file }: Props = $props();
  let imageURL = $state('');
  let thumbnailState = $state<'loading' | 'image' | 'video' | 'unavailable'>('loading');
  let activeObjectURL: string | undefined;
  let host = $state<HTMLDivElement>();
  let visible = $state(typeof IntersectionObserver === 'undefined');

  $effect(() => {
    const element = host;
    if (!element || typeof IntersectionObserver === 'undefined') {
      visible = true;
      return;
    }
    const observer = new IntersectionObserver(
      (entries) => {
        visible = entries.some((entry) => entry.target === element && entry.isIntersecting);
      },
      { rootMargin: '240px' },
    );
    observer.observe(element);
    return () => observer.disconnect();
  });

  $effect(() => {
    const id = file.id;
    const family = file.mime_family;
    const mimeType = file.mime_type;
    const size = file.size_bytes;
    const available = file.content_available && file.content_state === 'local_content';
    releaseImage();

    if (family === 'video') {
      thumbnailState = 'video';
      return;
    }
    if (family !== 'image' || !available || !isSupportedThumbnailMIME(mimeType) || size > MAX_THUMBNAIL_BYTES) {
      thumbnailState = 'unavailable';
      return;
    }
    if (!visible) {
      thumbnailState = 'loading';
      return;
    }

    thumbnailState = 'loading';
    const controller = new AbortController();
    let current = true;
    void withThumbnailSlot(controller.signal, () => loadImage(id, mimeType, controller.signal))
      .then((url) => {
        if (!current || controller.signal.aborted) {
          URL.revokeObjectURL(url);
          return;
        }
        activeObjectURL = url;
        imageURL = url;
        thumbnailState = 'image';
      })
      .catch(() => {
        if (current && !controller.signal.aborted) thumbnailState = 'unavailable';
      });

    return () => {
      current = false;
      controller.abort();
      releaseImage();
    };
  });

  async function loadImage(id: number, mimeType: string, signal: AbortSignal): Promise<string> {
    const { data, response } = await generatedGetFileContent({ id }, { ...client, signal, parseAs: 'stream' });
    if (!response.ok || !(data instanceof ReadableStream)) {
      throw new Error('Archived thumbnail content could not be loaded.');
    }
    const bytes = await readBoundedStream(data, response.headers, signal, MAX_THUMBNAIL_BYTES);
    const blob = validatedThumbnailBlob(bytes, mimeType, response.headers.get('Content-Type'));
    return URL.createObjectURL(blob);
  }

  function releaseImage(): void {
    if (activeObjectURL) URL.revokeObjectURL(activeObjectURL);
    activeObjectURL = undefined;
    imageURL = '';
  }

  function imageFailed(): void {
    releaseImage();
    thumbnailState = 'unavailable';
  }
</script>

<div class="thumbnail" bind:this={host}>
  {#if thumbnailState === 'image'}
    <img src={imageURL} alt={`Thumbnail ${file.filename || `attachment ${file.id}`}`} onerror={imageFailed} />
  {:else if thumbnailState === 'video'}
    <span class="placeholder" data-kind="video"><strong>Video</strong><small>{file.mime_type}</small></span>
  {:else if thumbnailState === 'loading'}
    <span class="placeholder" role="status">Loading thumbnail…</span>
  {:else}
    <span class="placeholder" data-kind="unavailable">Image preview unavailable</span>
  {/if}
</div>

<style>
  .thumbnail {
    display: grid;
    width: 100%;
    aspect-ratio: 4 / 3;
    place-items: center;
    overflow: hidden;
    background: var(--bg-inset);
  }
  img {
    width: 100%;
    height: 100%;
    object-fit: cover;
  }
  .placeholder {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-1);
    flex-direction: column;
    padding: var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    text-align: center;
  }
  .placeholder strong {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  .placeholder small {
    font-size: var(--font-size-2xs);
  }
</style>
