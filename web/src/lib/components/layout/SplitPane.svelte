<script lang="ts">
  import { SplitResizeHandle, type SplitResizeEvent } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, type Snippet } from 'svelte';

  interface Props {
    ariaLabel: string;
    storageKey: string;
    /** Horizontal panes sit side by side and size the PRIMARY (left) pane;
     * vertical panes stack and size the SECONDARY (bottom) pane — the
     * reading-pane arrangement, where the list above flexes and the pane
     * below keeps its height. */
    orientation?: 'horizontal' | 'vertical';
    initialSize?: number;
    /** Fraction of the container granted to the sized pane while the user
     * has never resized it (no stored size). Once the user drags or keys the
     * handle, the persisted pixel size wins. */
    initialFraction?: number;
    minPrimary?: number;
    minSecondary?: number;
    /** Collapses the split to the primary pane only (no handle, no
     * secondary) without unmounting the primary content. */
    collapsed?: boolean;
    primary?: Snippet;
    secondary?: Snippet;
    onSizeChange?: (size: number) => void;
  }

  let {
    ariaLabel,
    storageKey,
    orientation = 'horizontal',
    initialSize = 360,
    initialFraction = undefined,
    minPrimary = 220,
    minSecondary = orientation === 'vertical' ? 160 : 320,
    collapsed = false,
    primary,
    secondary,
    onSizeChange
  }: Props = $props();

  const keyboardStep = 24;
  const vertical = orientation === 'vertical';
  const handleThickness = vertical ? 5 : 4;

  function readStoredSize(): number | undefined {
    try {
      const storage = globalThis.localStorage;
      if (typeof storage === 'undefined') return undefined;
      const stored = storage.getItem(storageKey);
      if (stored === null) return undefined;
      const parsed = Number(stored);
      return Number.isFinite(parsed) ? parsed : undefined;
    } catch {
      return undefined;
    }
  }

  function persistSize(size: number): void {
    try {
      const storage = globalThis.localStorage;
      if (typeof storage === 'undefined') return;
      storage.setItem(storageKey, String(size));
    } catch {
      // Storage can be unavailable without making the layout unusable.
    }
  }

  let host: HTMLDivElement;
  const storedSize = readStoredSize();
  let sizedSize = $state(storedSize ?? initialSize);
  let userSized = storedSize !== undefined;
  let available = $state(0);
  let resizeStartSize = 0;
  let dragCleanup: (() => void) | undefined;

  const minSized = $derived(vertical ? minSecondary : minPrimary);
  const minOther = $derived(vertical ? minPrimary : minSecondary);
  const maximum = $derived(
    available > 0
      ? Math.max(0, available - minOther - handleThickness)
      : Math.max(minSized, sizedSize)
  );
  const minimum = $derived(Math.min(minSized, maximum));

  function clamp(size: number): number {
    const atLeastMinimum = Math.max(minimum, size);
    return available > 0 ? Math.min(maximum, atLeastMinimum) : atLeastMinimum;
  }

  function setSize(size: number, persist = true): void {
    const next = clamp(size);
    sizedSize = next;
    if (persist) {
      userSized = true;
      persistSize(next);
    }
    onSizeChange?.(next);
  }

  function startResize(): void {
    resizeStartSize = sizedSize;
  }

  function resize(event: SplitResizeEvent): void {
    setSize(resizeStartSize + event.deltaX);
  }

  function startVerticalDrag(event: MouseEvent): void {
    event.preventDefault();
    dragCleanup?.();
    const startY = event.clientY;
    resizeStartSize = sizedSize;
    const onMove = (moveEvent: MouseEvent): void => {
      // The sized pane is below the handle: dragging up grows it.
      setSize(resizeStartSize - (moveEvent.clientY - startY));
    };
    const onUp = (): void => {
      dragCleanup?.();
      dragCleanup = undefined;
    };
    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', onUp);
    dragCleanup = () => {
      window.removeEventListener('mousemove', onMove);
      window.removeEventListener('mouseup', onUp);
    };
  }

  function handleVerticalKeydown(event: KeyboardEvent): void {
    if (event.key !== 'ArrowUp' && event.key !== 'ArrowDown') return;
    event.preventDefault();
    setSize(sizedSize + (event.key === 'ArrowUp' ? keyboardStep : -keyboardStep));
  }

  function measure(entry: ResizeObserverEntry): number | undefined {
    return vertical ? entry.contentRect.height : entry.contentRect.width;
  }

  onMount(() => {
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (!entry) return;
      const measured = measure(entry);
      if (measured === undefined) return;
      available = measured;
      if (userSized) setSize(sizedSize);
      else if (initialFraction !== undefined && available > 0) {
        setSize(Math.round(available * initialFraction), false);
      } else setSize(sizedSize, false);
    });
    observer.observe(host);
    return () => observer.disconnect();
  });

  onDestroy(() => dragCleanup?.());
</script>

<div class="split-pane" class:split-pane--vertical={vertical} data-split-pane bind:this={host}>
  <section
    class="pane primary"
    data-pane="primary"
    style:flex-basis={vertical ? undefined : `${sizedSize}px`}
  >
    {#if primary}{@render primary()}{/if}
  </section>
  {#if !collapsed}
    {#if vertical}
      <button
        class="vertical-handle"
        type="button"
        aria-label={ariaLabel}
        onkeydown={handleVerticalKeydown}
        onmousedown={startVerticalDrag}
      ></button>
      <section class="pane secondary" data-pane="secondary" style:flex-basis={`${sizedSize}px`}>
        {#if secondary}{@render secondary()}{/if}
      </section>
    {:else}
      <SplitResizeHandle {ariaLabel} onResizeStart={startResize} onResize={resize} />
      <section class="pane secondary" data-pane="secondary">
        {#if secondary}{@render secondary()}{/if}
      </section>
    {/if}
  {/if}
</div>

<style>
  .split-pane {
    display: flex;
    width: 100%;
    min-width: 0;
    height: 100%;
  }

  .split-pane--vertical {
    min-height: 0;
    flex-direction: column;
  }

  .pane {
    min-width: 0;
    min-height: 0;
    overflow: auto;
  }

  .primary {
    flex-grow: 0;
    flex-shrink: 0;
  }

  .secondary {
    flex: 1;
  }

  .split-pane--vertical > .primary {
    flex: 1;
    overflow: hidden;
  }

  .split-pane--vertical > .secondary {
    flex-grow: 0;
    flex-shrink: 0;
    overflow: hidden;
  }

  .vertical-handle {
    height: 5px;
    flex: none;
    padding: 0;
    border: 0;
    appearance: none;
    background: var(--border-muted);
    cursor: row-resize;
  }

  .vertical-handle:hover,
  .vertical-handle:focus-visible {
    background: var(--accent-blue);
  }
</style>
