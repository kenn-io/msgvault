<script lang="ts">
  import { appShortcuts, Button, Modal } from '@kenn-io/kit-ui';
  import { onMount, type Snippet } from 'svelte';

  onMount(() => appShortcuts.pushScope('shell-dialog'));

  interface Props {
    title: string;
    onclose: () => void;
    children: Snippet;
    /** The primary action, bottom-right. Without it the dialog has no footer. */
    primaryLabel?: string;
    onprimary?: () => void;
    primaryDisabled?: boolean;
    /** Styles the primary action as destructive. */
    danger?: boolean;
    /** Left side of the footer, such as a status line. */
    footerStart?: Snippet;
  }

  let {
    title,
    onclose,
    children,
    primaryLabel = undefined,
    onprimary = undefined,
    primaryDisabled = false,
    danger = false,
    footerStart = undefined,
  }: Props = $props();
</script>

{#snippet footer()}
  {#if footerStart}
    <span class="dialog-footer-start">{@render footerStart()}</span>
  {/if}
  {#if primaryLabel}
    <Button
      surface="solid"
      tone={danger ? 'danger' : 'info'}
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
  footer={primaryLabel || footerStart ? footer : undefined}
>
  {@render children()}
</Modal>

<style>
  .dialog-footer-start {
    margin-right: auto;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
</style>
