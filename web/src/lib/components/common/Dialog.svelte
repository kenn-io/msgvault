<script lang="ts">
  import { claimDialogEscape } from './dialog-escape';
  import { Button, Modal } from '@kenn-io/kit-ui';
  import { onMount, type Snippet } from 'svelte';

  const id = $props.id();
  const scope = `shell-dialog:${id}`;
  onMount(() => claimDialogEscape(scope, () => onclose()));

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

<!-- Kit's header, scrolling body, and footer take the shared sizes in tokens.css. -->
<div class="dialog-shell">
  <Modal
    {title}
    {onclose}
    closable={true}
    width="560px"
    maxWidth="calc(100vw - 2 * var(--dialog-inset))"
    maxHeight="calc(100svh - var(--dialog-top) - var(--dialog-inset))"
    footer={primaryLabel ? footer : undefined}
  >
    {@render children()}
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
</style>
