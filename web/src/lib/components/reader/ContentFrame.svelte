<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

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
    onContentFocusChange = undefined,
    onFrameKey = undefined
  }: {
    client?: APIClient;
    messageId: number;
    html: string;
    title: string;
    onContentFocusChange?: (focused: boolean) => void;
    onFrameKey?: (key: string) => void;
  } = $props();

  let frame = $state<HTMLIFrameElement>();
  let enterControl = $state<HTMLSpanElement>();
  let toolbar = $state<HTMLDivElement>();
  let scrollShell = $state<HTMLDivElement>();
  let frameDocument = $state<{ generation: number; srcdoc: string }>();
  let documentState = $state<'building' | 'loading' | 'ready' | 'failed'>('building');
  let remoteImageCount = $state(0);
  let remoteImagesAllowed = $state(false);
  let contentEntered = $state(false);
  let nonce = $state(createFrameNonce());
  let documentGeneration = 0;
  let messageIdentity = '';
  let inlineController: AbortController | undefined;

  function invalidateDocument(): void {
    documentGeneration += 1;
    inlineController?.abort();
    if (contentEntered) {
      contentEntered = false;
      onContentFocusChange?.(false);
      toolbar?.focus({ preventScroll: true });
    }
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

  async function exitContent(): Promise<void> {
    if (!contentEntered) return;
    contentEntered = false;
    onContentFocusChange?.(false);
    await tick();
    enterControl?.querySelector('button')?.focus();
  }

  async function enterContent(): Promise<void> {
    if (documentState !== 'ready' || !frame) return;
    contentEntered = true;
    onContentFocusChange?.(true);
    await tick();
    frame?.focus();
  }

  function loadRemoteImages(): void {
    if (remoteImagesAllowed || documentState !== 'ready') return;
    invalidateDocument();
    remoteImagesAllowed = true;
  }

  function handleFrameKey(key: string): void {
    if (!contentEntered) return;
    if (key === 'Escape') {
      void exitContent();
      return;
    }
    onFrameKey?.(key);
  }

  const messageHandler = createArchivedContentMessageHandler({
    frameWindow: () => frame?.contentWindow ?? null,
    nonce: () => nonce,
    onKey: handleFrameKey,
    onScroll: (deltaY) => scrollShell?.scrollBy({ top: deltaY })
  });

  onMount(() => window.addEventListener('message', messageHandler));
  onDestroy(() => {
    window.removeEventListener('message', messageHandler);
    inlineController?.abort();
    documentGeneration += 1;
    if (contentEntered) onContentFocusChange?.(false);
  });
</script>

<section
  class="content-frame"
  class:content-frame--entered={contentEntered}
  aria-label={title}
>
  <div
    bind:this={toolbar}
    class="content-toolbar"
    aria-label="Archived content controls"
    tabindex="-1"
  >
    {#if contentEntered}
      <span class="content-state" role="status">Archived content active</span>
      <Button
        size="sm"
        surface="outline"
        label="Exit content"
        ariaLabel="Exit archived content"
        onclick={() => { void exitContent(); }}
      />
    {:else}
      <span bind:this={enterControl}>
        <Button
          size="sm"
          surface="outline"
          label="Enter content"
          ariaLabel="Enter archived content"
          disabled={documentState !== 'ready'}
          onclick={() => { void enterContent(); }}
        />
      </span>
    {/if}
    {#if documentState === 'building' || documentState === 'loading'}
      <span class="content-state" role="status">Preparing archived content…</span>
    {:else if documentState === 'failed'}
      <span class="content-state" role="alert">Archived content unavailable</span>
    {/if}
    {#if remoteImageCount > 0 && !remoteImagesAllowed}
      <Button
        size="sm"
        surface="soft"
        label={`Load ${remoteImageCount} remote ${remoteImageCount === 1 ? 'image' : 'images'}`}
        ariaLabel={`Load ${remoteImageCount} remote ${remoteImageCount === 1 ? 'image' : 'images'}`}
        disabled={documentState !== 'ready'}
        onclick={loadRemoteImages}
      />
    {/if}
  </div>
  <div bind:this={scrollShell} class="frame-scroll">
    {#if !contentEntered}
      <div
        class="content-entry-shield"
        aria-hidden="true"
        onpointerdown={(event) => event.preventDefault()}
      ></div>
    {/if}
    {#if frameDocument}
      {#key frameDocument.generation}
        <iframe
          bind:this={frame}
          title={title}
          sandbox="allow-scripts"
          referrerpolicy="no-referrer"
          tabindex="-1"
          inert={!contentEntered}
          srcdoc={frameDocument.srcdoc}
          onload={() => handleFrameLoad(frameDocument!.generation)}
        ></iframe>
      {/key}
    {/if}
  </div>
</section>

<style>
  .content-frame {
    display: flex;
    min-height: 240px;
    flex-direction: column;
    overflow: hidden;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
  }

  .content-frame--entered {
    outline: 2px solid var(--accent-blue);
    outline-offset: 2px;
  }

  .content-toolbar {
    position: sticky;
    z-index: 1;
    top: 0;
    display: flex;
    min-height: 36px;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2);
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-surface);
  }

  .content-state {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .frame-scroll {
    position: relative;
    min-height: 0;
    flex: 1;
    overflow: auto;
  }

  .content-entry-shield {
    position: absolute;
    z-index: 1;
    inset: 0;
    cursor: default;
  }

  iframe {
    display: block;
    width: 100%;
    min-height: 320px;
    border: 0;
    background: white;
  }

  iframe[inert] {
    pointer-events: none;
    user-select: none;
  }
</style>
