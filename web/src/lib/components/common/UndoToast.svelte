<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';

  interface Props {
    /** What happened. */
    message: string;
    onUndo: () => void;
    /** Runs when the time is up, which is the moment to make the change final. */
    onExpire: () => void;
    durationMs?: number;
  }

  let { message, onUndo, onExpire, durationMs = 8000 }: Props = $props();

  onMount(() => {
    const timer = setTimeout(onExpire, durationMs);
    return () => clearTimeout(timer);
  });
</script>

<!-- kit-ui-check-ignore: kit FlashBanner has no action button, and Undo must sit in the toast. -->
<div class="undo-toast" role="status">
  <span class="undo-toast__message">{message}</span>
  <Button size="sm" surface="soft" label="Undo" onclick={onUndo} />
</div>

<style>
  /* kit-ui-check-ignore: same as the markup above. */
  .undo-toast {
    position: fixed;
    bottom: var(--section-gap);
    left: 50%;
    z-index: var(--z-tooltip);
    display: flex;
    max-width: calc(100vw - 2 * var(--section-gap));
    align-items: center;
    gap: var(--pane-pad-x);
    padding: var(--control-gap) var(--control-gap) var(--control-gap) var(--pane-pad-x);
    transform: translateX(-50%);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-lg);
    background: var(--bg-surface);
    box-shadow: var(--shadow-md);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .undo-toast__message {
    min-width: 0;
    overflow-wrap: anywhere;
  }
</style>
