<script lang="ts">
  import { appShortcuts, IconButton, trapFocus } from '@kenn-io/kit-ui';
  import X from '@lucide/svelte/icons/x';
  import { onMount, tick, type Snippet } from 'svelte';

  let { onclose, children }: { onclose: () => void; children: Snippet } = $props();
  let panel = $state<HTMLElement>();

  onMount(() => {
    const popScope = appShortcuts.pushScope('navigation-drawer');
    const unregister = appShortcuts.register('escape', () => onclose(), {
      scope: 'navigation-drawer'
    });
    void tick().then(() => panel?.querySelector<HTMLElement>('[aria-current="page"]')?.focus());
    return () => {
      unregister();
      popScope();
    };
  });
</script>

<!-- kit-ui-check-ignore: kit DetailDrawer is a right-side sheet with its own Escape handling; this is a left slide-out that owns the navigation-drawer shortcut scope. -->
<div class="drawer">
  <button
    type="button"
    class="drawer__scrim"
    aria-hidden="true"
    tabindex="-1"
    onclick={onclose}
  ></button>
  <div
    class="drawer__panel"
    role="dialog"
    aria-modal="true"
    aria-label="Navigation"
    tabindex="-1"
    bind:this={panel}
    {@attach trapFocus}
  >
    <div class="drawer__close">
      <IconButton ariaLabel="Close navigation" onclick={onclose}><X size={18} /></IconButton>
    </div>
    {@render children()}
  </div>
</div>

<style>
  /* kit-ui-check-ignore: same as the markup above. */
  .drawer {
    position: fixed;
    /* kit-ui-check-ignore: same as the markup above. */
    inset: 0;
    z-index: var(--z-overlay);
  }

  .drawer__scrim {
    position: absolute;
    inset: 0;
    border: 0;
    background: var(--overlay-bg);
  }

  .drawer__panel {
    --control-h: 44px;
    --control-h-lg: 44px;
    position: absolute;
    inset: 0 auto 0 0;
    height: 100%;
    box-shadow: var(--shadow-lg);
  }

  .drawer__close {
    position: absolute;
    top: var(--space-3);
    right: var(--space-3);
  }
</style>
