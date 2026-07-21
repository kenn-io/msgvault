<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import { buildFrameDocument } from '../../content/frame-document';
  import { sanitizeArchivedHTML } from '../../content/sanitize';
  import {
    createArchivedContentMessageHandler,
    createFrameNonce
  } from './ContentFrame.browser.svelte';
  import { resolveArchivedInlineImages } from './inline-images';

  let {
    client = undefined,
    messageId,
    html,
    title,
    onEscapeContent = undefined
  }: {
    client?: APIClient;
    messageId: number;
    html: string;
    title: string;
    /** Called when Escape is pressed while focus is inside the archived
     * frame — the keyboard path out of the content. Defaults to focusing
     * the nearest scrollable ancestor. */
    onEscapeContent?: () => void;
  } = $props();

  let host = $state<HTMLElement>();
  let frame = $state<HTMLIFrameElement>();
  let frameDocument = $state<{ generation: number; srcdoc: string }>();
  let documentState = $state<'building' | 'loading' | 'ready' | 'failed'>('building');
  let remoteImageCount = $state(0);
  let remoteImagesAllowed = $state(false);
  let frameHeight = $state(96);
  let nonce = $state(createFrameNonce());
  let documentGeneration = 0;
  let messageIdentity = '';
  let inlineController: AbortController | undefined;

  function invalidateDocument(): void {
    documentGeneration += 1;
    inlineController?.abort();
    frameDocument = undefined;
    documentState = 'building';
  }

  $effect(() => {
    const currentMessageID = messageId;
    const currentHTML = html;
    const currentClient = client;
    const nextIdentity = `${currentMessageID}\u0000${currentHTML}`;
    const identityChanged = nextIdentity !== messageIdentity;
    messageIdentity = nextIdentity;
    if (identityChanged && remoteImagesAllowed) remoteImagesAllowed = false;
    const allowRemoteImages = identityChanged ? false : remoteImagesAllowed;
    untrack(invalidateDocument);
    const generation = documentGeneration;
    const buildNonce = createFrameNonce();
    const sanitized = sanitizeArchivedHTML(currentHTML, {
      messageId: currentMessageID,
      allowRemoteImages
    });
    remoteImageCount = sanitized.remoteImages.length;
    inlineController = new AbortController();
    const signal = inlineController.signal;
    void resolveArchivedInlineImages({
      html: sanitized.html,
      inlineImages: sanitized.inlineImages,
      client: currentClient,
      messageId: currentMessageID,
      signal
    }).then((resolvedHTML) => buildFrameDocument({
      html: resolvedHTML,
      nonce: buildNonce,
      targetOrigin: window.location.origin,
      remoteImages: allowRemoteImages ? sanitized.remoteImages : []
    })).then((document) => {
      if (generation !== documentGeneration || signal.aborted) return;
      nonce = buildNonce;
      frameDocument = { generation, srcdoc: document };
      documentState = 'loading';
    }).catch((error) => {
      if (generation !== documentGeneration || signal.aborted) return;
      documentState = 'failed';
      console.error('Could not build archived content frame', error);
    });
  });

  function handleFrameLoad(generation: number): void {
    if (frameDocument?.generation !== generation || documentState !== 'loading') return;
    documentState = 'ready';
  }

  function loadRemoteImages(): void {
    if (remoteImagesAllowed || documentState !== 'ready') return;
    invalidateDocument();
    remoteImagesAllowed = true;
  }

  function nearestScroller(): HTMLElement | undefined {
    let node = host?.parentElement ?? undefined;
    while (node) {
      const style = getComputedStyle(node);
      if ((style.overflowY === 'auto' || style.overflowY === 'scroll') &&
        node.scrollHeight > node.clientHeight) {
        return node;
      }
      node = node.parentElement ?? undefined;
    }
    return undefined;
  }

  function scrollThread(deltaY: number): void {
    nearestScroller()?.scrollBy({ top: deltaY });
  }

  function exitContent(): void {
    if (onEscapeContent) {
      onEscapeContent();
      return;
    }
    // Focus the nearest focusable ancestor (the thread scroller) whether or
    // not it currently overflows — Escape must always lead back out.
    let node = host?.parentElement ?? undefined;
    while (node) {
      if (node.tabIndex >= 0) {
        node.focus();
        return;
      }
      node = node.parentElement ?? undefined;
    }
  }

  function handleFrameKey(key: string): void {
    const scroller = nearestScroller();
    if (key === 'Escape') exitContent();
    else if (key === 'ArrowDown') scrollThread(48);
    else if (key === 'ArrowUp') scrollThread(-48);
    else if (key === 'PageDown') scrollThread((scroller?.clientHeight ?? 480) * 0.9);
    else if (key === 'PageUp') scrollThread(-(scroller?.clientHeight ?? 480) * 0.9);
    else if (key === 'Home') scroller?.scrollTo({ top: 0 });
    else if (key === 'End') scroller?.scrollTo({ top: scroller.scrollHeight });
  }

  const messageHandler = createArchivedContentMessageHandler({
    frameWindow: () => frame?.contentWindow ?? null,
    nonce: () => nonce,
    onKey: handleFrameKey,
    onScroll: scrollThread,
    onHeight: (height) => { frameHeight = height; }
  });

  onMount(() => window.addEventListener('message', messageHandler));
  onDestroy(() => {
    window.removeEventListener('message', messageHandler);
    inlineController?.abort();
    documentGeneration += 1;
  });
</script>

<section class="content-frame" aria-label={title} bind:this={host}>
  {#if remoteImageCount > 0 && !remoteImagesAllowed}
    <p class="remote-notice" role="status">
      <span>{remoteImageCount === 1
        ? '1 remote image is not loaded.'
        : `${remoteImageCount} remote images are not loaded.`}</span>
      <button
        type="button"
        aria-label={`Load ${remoteImageCount} remote ${remoteImageCount === 1 ? 'image' : 'images'}`}
        disabled={documentState !== 'ready'}
        onclick={loadRemoteImages}
      >Load images</button>
    </p>
  {/if}
  {#if documentState === 'failed'}
    <p class="frame-state" role="alert">Archived content unavailable</p>
  {:else if !frameDocument}
    <p class="frame-state" role="status">Preparing message…</p>
  {/if}
  {#if frameDocument}
    {#key frameDocument.generation}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -- the sandboxed
           frame is the keyboard path into archived content; the bridge
           forwards Escape back out to the surrounding scroller. -->
      <iframe
        bind:this={frame}
        title={title}
        sandbox="allow-scripts"
        referrerpolicy="no-referrer"
        tabindex="0"
        srcdoc={frameDocument.srcdoc}
        style:height={`${frameHeight}px`}
        onload={() => handleFrameLoad(frameDocument!.generation)}
      ></iframe>
    {/key}
  {/if}
</section>

<style>
  .content-frame {
    display: flex;
    min-width: 0;
    flex-direction: column;
    gap: var(--space-2);
  }

  .remote-notice {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .remote-notice button {
    padding: 0;
    border: 0;
    background: none;
    color: var(--accent-blue);
    cursor: pointer;
    font: inherit;
  }

  .remote-notice button:disabled {
    color: var(--text-muted);
    cursor: default;
  }

  .remote-notice button:hover:not(:disabled) {
    text-decoration: underline;
  }

  .frame-state {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  /* HTML mail assumes a white canvas; keep it white in both themes but
   * seamless — rounded, borderless, sized to its content by the bridge. */
  iframe {
    display: block;
    width: 100%;
    border: 0;
    border-radius: var(--radius-sm);
    background: white;
  }

  iframe:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 1px;
  }
</style>
