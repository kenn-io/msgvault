<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import BuildIndexAction from './BuildIndexAction.svelte';

  import type { ExploreSearchMode } from '../../explore/models';
  import type { SearchCoverageAction, SearchCoverageValue } from '../../search/modes';

  interface Props {
    requestedMode?: ExploreSearchMode;
    coverage: SearchCoverageValue;
    onaction?: (action: SearchCoverageAction) => void;
  }

  let { requestedMode = 'full_text', coverage, onaction = undefined }: Props = $props();


  const count = $derived(coverage.eligible_count.toLocaleString());
  const percentage = $derived(Math.round(coverage.percentage).toLocaleString());
  const actions = $derived(coverage.actions ?? []);
  const summary = $derived.by(() => {
    switch (coverage.status) {
      case 'disabled': return 'Semantic search is disabled';
      case 'initializing': return 'Semantic index is initializing';
      case 'stale': return 'Semantic index is stale';
      case 'unavailable': return 'Semantic index is unavailable';
      case 'incomplete':
      case 'ready': return `Semantic index: ${percentage}% of ${count} items`;
    }
  });
</script>

<div class="coverage" role="status" aria-live="polite">
  <span>{summary}.</span>
  {#if coverage.detail}<span>{coverage.detail}</span>{/if}
  {#if requestedMode === 'semantic' && coverage.status === 'incomplete'}
    <span>Unembedded items cannot appear in Semantic results.</span>
  {/if}
  {#if actions.includes('retry')}
    <Button label="Retry" tone="info" surface="outline" onclick={() => onaction?.('retry')} />
  {/if}
  {#if actions.includes('build_index')}
    <BuildIndexAction onconfirm={() => onaction?.('build_index')} />
  {/if}
</div>

<style>
  .coverage {
    display: flex;
    min-height: 28px;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }
</style>
