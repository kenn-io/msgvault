<script lang="ts">
  import { appShortcuts, Button, escapeCloses, Modal } from '@kenn-io/kit-ui';
  import { onMount, type Snippet } from 'svelte';

  const id = $props.id();
  const scope = `shell-dialog:${id}`;
  onMount(() => {
    const popScope = appShortcuts.pushScope(scope);
    const close = escapeCloses(() => onclose());
    const handleEscape = (event: KeyboardEvent): void => {
      if (appShortcuts.activeScope() !== scope) return;
      // kit-ui-check-ignore: observes Kit popup roles; renders no popup markup.
      if (document.querySelector('[role="listbox"], [role="menu"]')) return;
      close(event);
    };
    // Claim Escape before the shell and an underlying reader's window listeners.
    document.addEventListener('keydown', handleEscape);
    return () => { document.removeEventListener('keydown', handleEscape); popScope(); };
  });

  interface Props {
    title: string;
    onclose: () => void;
    children: Snippet;
    /** The primary action, bottom-right. Without it the dialog has no footer. */
    primaryLabel?: string;
    onprimary?: () => void;
    primaryDisabled?: boolean;
  }

  let {
    title,
    onclose,
    children,
    primaryLabel = undefined,
    onprimary = undefined,
    primaryDisabled = false,
  }: Props = $props();
</script>

{#snippet footer()}
  {#if primaryLabel}
    <Button
      surface="solid"
      tone="info"
      label={primaryLabel}
      disabled={primaryDisabled}
      onclick={onprimary}
    />
  {/if}
{/snippet}

<Modal
  {title}
  {onclose}
  width="520px"
  maxWidth="calc(100vw - 32px)"
  footer={primaryLabel ? footer : undefined}
>
  {@render children()}
</Modal>
