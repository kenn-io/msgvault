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
