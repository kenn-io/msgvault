<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { FrameColorScheme } from '../../content/frame-document';
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
  let frameDocument = $state<{ generation: number; srcdoc: string; mode: 'canvas' | 'themed' }>();
  let documentState = $state<'building' | 'loading' | 'ready' | 'failed'>('building');
  let remoteImageCount = $state(0);
  let remoteImagesAllowed = $state(false);
  let frameHeight = $state(96);
  let nonce = $state(createFrameNonce());
  let colorScheme = $state<FrameColorScheme>(resolvedColorScheme());
  let documentGeneration = 0;
  let messageIdentity = '';
  let inlineController: AbortController | undefined;
  let themeObserver: MutationObserver | undefined;

  function resolvedColorScheme(): FrameColorScheme {
    if (typeof document === 'undefined') return 'light';
    return document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light';
  }

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
    const currentScheme = colorScheme;
    const nextIdentity = `${currentMessageID}\u0000${currentHTML}`;
    const identityChanged = nextIdentity !== messageIdentity;
    messageIdentity = nextIdentity;
    if (identityChanged && remoteImagesAllowed) remoteImagesAllowed = false;
    // A new message starts from the compact default height; the bridge
    // reports the real content height as soon as the document loads. Reusing
    // the previous message's height would leave a tall empty frame.
    if (identityChanged) frameHeight = 96;
    const allowRemoteImages = identityChanged ? false : remoteImagesAllowed;
    untrack(invalidateDocument);
    const generation = documentGeneration;
    const buildNonce = createFrameNonce();
    const sanitized = sanitizeArchivedHTML(currentHTML, {
      messageId: currentMessageID,
      allowRemoteImages
    });
    remoteImageCount = sanitized.remoteImages.length;
    const mode = sanitized.designed ? 'canvas' as const : 'themed' as const;
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
      remoteImages: allowRemoteImages ? sanitized.remoteImages : [],
      appearance: { mode, colorScheme: currentScheme }
    })).then((document) => {
      if (generation !== documentGeneration || signal.aborted) return;
      nonce = buildNonce;
      frameDocument = { generation, srcdoc: document, mode };
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

  onMount(() => {
    window.addEventListener('message', messageHandler);
    themeObserver = new MutationObserver(() => { colorScheme = resolvedColorScheme(); });
    themeObserver.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ['data-theme']
    });
  });
  onDestroy(() => {
    window.removeEventListener('message', messageHandler);
    themeObserver?.disconnect();
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
        class:canvas={frameDocument.mode === 'canvas'}
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

  /* Simple mail renders transparently on the theme surface; the bridge sizes
   * the frame to its content, so the thread is the only scroller. */
  iframe {
    display: block;
    width: 100%;
    border: 0;
    background: transparent;
  }

  /* Designed mail keeps the white canvas it was authored for — bounded by a
   * hairline so it reads as an artifact rather than a hole in dark mode.
   * content-box keeps the styled height equal to the inner viewport, so the
   * hairline never steals bridge-reported content height. */
  iframe.canvas {
    box-sizing: content-box;
    border: 1px solid var(--border-muted);
    border-radius: 6px;
    background: white;
  }

  iframe:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 1px;
  }
</style>
