<script lang="ts">
  import { SplitResizeHandle, type SplitResizeEvent } from '@kenn-io/kit-ui';
  import { onMount, type Snippet } from 'svelte';

  interface Props {
    ariaLabel: string;
    storageKey: string;
    initialSize?: number;
    minPrimary?: number;
    minSecondary?: number;
    primary?: Snippet;
    secondary?: Snippet;
    onSizeChange?: (size: number) => void;
  }

  let {
    ariaLabel,
    storageKey,
    initialSize = 360,
    minPrimary = 220,
    minSecondary = 320,
    primary,
    secondary,
    onSizeChange
  }: Props = $props();

  const handleWidth = 4;

  function readSize(): number {
    try {
      const storage = globalThis.localStorage;
      if (typeof storage === 'undefined') return initialSize;
      const stored = storage.getItem(storageKey);
      if (stored === null) return initialSize;
      const parsed = Number(stored);
      return Number.isFinite(parsed) ? parsed : initialSize;
    } catch {
      return initialSize;
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
  let primarySize = $state(readSize());
  let availableWidth = $state(0);
  let resizeStartSize = 0;

  const maximum = $derived(
    availableWidth > 0
      ? Math.max(0, availableWidth - minSecondary - handleWidth)
      : Math.max(minPrimary, primarySize)
  );
  const minimum = $derived(Math.min(minPrimary, maximum));

  function clamp(size: number): number {
    const atLeastMinimum = Math.max(minimum, size);
    return availableWidth > 0 ? Math.min(maximum, atLeastMinimum) : atLeastMinimum;
  }

  function setSize(size: number): void {
    const next = clamp(size);
    primarySize = next;
    persistSize(next);
    onSizeChange?.(next);
  }

  function startResize(): void {
    resizeStartSize = primarySize;
  }

  function resize(event: SplitResizeEvent): void {
    setSize(resizeStartSize + event.deltaX);
  }

  onMount(() => {
    const observer = new ResizeObserver((entries) => {
      const width = entries[0]?.contentRect.width;
      if (width === undefined) return;
      availableWidth = width;
      setSize(primarySize);
    });
    observer.observe(host);
    return () => observer.disconnect();
  });
</script>

<div class="split-pane" data-split-pane bind:this={host}>
  <section class="pane primary" data-pane="primary" style:flex-basis={`${primarySize}px`}>
    {#if primary}{@render primary()}{/if}
  </section>
  <SplitResizeHandle {ariaLabel} onResizeStart={startResize} onResize={resize} />
  <section class="pane secondary" data-pane="secondary">
    {#if secondary}{@render secondary()}{/if}
  </section>
</div>

<style>
  .split-pane {
    display: flex;
    width: 100%;
    min-width: 0;
    height: 100%;
  }

  .pane {
    min-width: 0;
    overflow: auto;
  }

  .primary {
    flex-grow: 0;
    flex-shrink: 0;
  }

  .secondary {
    flex: 1;
  }
</style>
