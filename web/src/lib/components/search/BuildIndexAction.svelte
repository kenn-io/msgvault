<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';

  let { onconfirm, size = 'md', surface = 'outline', ariaLabel = 'Build index', disabled = false }: {
    onconfirm: () => void;
    size?: 'sm' | 'md';
    surface?: 'outline' | 'soft';
    ariaLabel?: string;
    disabled?: boolean;
  } = $props();
  let confirming = $state(false);
</script>

<span class="build-index-action">
  {#if confirming}
    <span>Start a full rebuild of the semantic index?</span>
    <Button label="Cancel" {size} {disabled} tone="neutral" surface="outline" onclick={() => (confirming = false)} />
    <Button label="Confirm full rebuild" {size} {disabled} tone="info" surface="solid" onclick={() => { confirming = false; onconfirm(); }} />
  {:else}
    <Button label="Build index" {ariaLabel} {size} {disabled} tone="info" {surface} onclick={() => (confirming = true)} />
  {/if}
</span>

<style>
  .build-index-action {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--control-gap);
  }
</style>
