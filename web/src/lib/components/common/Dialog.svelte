<script lang="ts">
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import X from '@lucide/svelte/icons/x';
  import { claimDialogEscape } from './dialog-escape';
  import { Button, IconButton, Modal } from '@kenn-io/kit-ui';
  import { onMount, type Snippet } from 'svelte';

  const id = $props.id();
  const scope = `shell-dialog:${id}`;
  let shell: HTMLDivElement;
  onMount(() => {
    queueMicrotask(() => {
      const panel = shell.querySelector('[role="dialog"]');
      if (document.activeElement === panel) shell.querySelector<HTMLElement>('h2')?.focus();
    });
    return claimDialogEscape(scope, () => dismiss());
  });

  /** A step that removes the focused control hands focus to the title, so it stays in the dialog. */
  function keepFocus(node: HTMLElement): () => void {
    let focused: Element | null = null;
    const track = (event: FocusEvent) => { focused = event.target as Element; };
    const observer = new MutationObserver(() => {
      if (focused && !focused.isConnected && document.activeElement === document.body) node.querySelector<HTMLElement>('h2')?.focus();
    });
    node.addEventListener('focusin', track);
    observer.observe(node, { childList: true, subtree: true });
    return () => { observer.disconnect(); node.removeEventListener('focusin', track); };
  }

  interface Props {
    title: string;
    onclose: () => void;
    children: Snippet;
    width?: 'small' | 'medium' | 'large';
    /** A step's way back, left of the title; its name says where it returns to. */
    onback?: () => void;
    backLabel?: string;
    /** A notice after the body's fields, in the same scroll pane. */
    notice?: Snippet;
    /** The primary action, bottom-right, with Cancel before it. Without it the dialog has no footer. */
    primaryLabel?: string;
    onprimary?: () => void;
    primaryDisabled?: boolean;
    primaryTone?: 'info' | 'danger';
    /** Cancel closes through `onclose`. A dialog with nothing left to cancel turns it off. */
    cancellable?: boolean;
    cancelLabel?: string;
    /** Set while a write is pending, when closing would do nothing. */
    cancelDisabled?: boolean;
    /** A write is in flight: ×, Back, Cancel, Escape, and the backdrop do nothing, and the primary shows progress. */
    busy?: boolean;
  }

  let {
    title,
    onclose,
    children,
    width = 'medium',
    onback = undefined,
    backLabel = 'Back',
    notice = undefined,
    primaryLabel = undefined,
    onprimary = undefined,
    primaryDisabled = false,
    primaryTone = 'info',
    cancellable = true,
    cancelLabel = 'Cancel',
    cancelDisabled = false,
    busy = false,
  }: Props = $props();

  function dismiss(): void {
    if (!busy) onclose();
  }
</script>

{#snippet footer()}
  {#if primaryLabel}
    {#if cancellable}<Button label={cancelLabel} disabled={cancelDisabled || busy} onclick={dismiss} />{/if}
    <Button
      surface="solid"
      tone={primaryTone}
      label={primaryLabel}
      disabled={primaryDisabled || busy}
      {busy}
      onclick={onprimary}
    />
  {/if}
{/snippet}

<div class="dialog-shell" bind:this={shell} {@attach keepFocus}>
  <Modal
    ariaLabel={title}
    onclose={dismiss}
    closable={false}
    width={`var(--dialog-width-${width})`}
    maxWidth="calc(100vw - 2 * var(--dialog-inset))"
    maxHeight="calc(100svh - var(--dialog-top) - var(--dialog-inset))"
    footer={primaryLabel ? footer : undefined}
  >
    <div class="dialog-header">
      {#if onback}<IconButton ariaLabel={backLabel} disabled={busy} onclick={onback}><ArrowLeft size={14} aria-hidden="true" /></IconButton>{/if}
      <h2 tabindex="-1" title={title}>{title}</h2>
      <IconButton ariaLabel="Close" disabled={busy} onclick={dismiss}><X size={14} aria-hidden="true" /></IconButton>
    </div>
    <!-- One scroll pane between the header and the footer: the fields, then the notice when one shows. -->
    <div class="dialog-content">
      {@render children()}
      {#if notice}<div class="dialog-notice">{@render notice()}</div>{/if}
    </div>
  </Modal>
</div>

<style>
  /* The dialog stays 16 from the screen's edges and opens 32 from the top (16 on a phone). */
  .dialog-shell {
    --dialog-inset: var(--space-6);
    --dialog-top: 32px;
  }
  .dialog-shell :global(.kit-modal-overlay) {
    align-items: flex-start;
    padding-top: var(--dialog-top);
  }
  @media (max-width: 640px) {
    .dialog-shell { --dialog-top: var(--space-6); }
  }
  /* Kit's body holds the header and the scrolling content, so it neither pads nor scrolls itself. */
  .dialog-shell :global(.kit-modal-body.kit-modal-body) {
    display: flex;
    flex-direction: column;
    min-height: 0;
    padding: 0;
    overflow: hidden;
    scrollbar-gutter: auto;
  }
  /* A 44 band: a 28 control 8 above and below, the title 12 from the left edge and truncated. */
  .dialog-header {
    box-sizing: border-box;
    display: flex;
    flex: none;
    align-items: center;
    gap: var(--space-group);
    min-height: 44px;
    padding: var(--space-group) var(--space-group) var(--space-group) var(--pane-pad-x);
    box-shadow: 0 1px 0 var(--border-default);
    position: relative;
    z-index: 1;
  }
  h2 {
    flex: 1;
    min-width: 0;
    margin: 0;
    overflow: hidden;
    font-size: var(--type-title-size);
    line-height: var(--type-title-line);
    font-weight: 600;
    text-overflow: ellipsis;
    outline: none;
    white-space: nowrap;
  }
  /* The body pads 12 on every side; on the right that is 4 plus the bar's reserved 8. Long words wrap. */
  .dialog-content {
    flex: 1 1 auto;
    min-height: 0;
    overflow-x: hidden;
    overflow-y: auto;
    scrollbar-gutter: stable;
    padding: var(--pane-pad-x) calc(var(--pane-pad-x) - var(--scrollbar-gutter-size)) var(--pane-pad-x) var(--pane-pad-x);
    overflow-wrap: anywhere;
  }
  .dialog-notice { margin-top: var(--pane-pad-x); }
  /* A notice snippet that shows nothing renders nothing: no empty band under the fields. */
  .dialog-notice:not(:has(*)) { display: none; }
</style>
