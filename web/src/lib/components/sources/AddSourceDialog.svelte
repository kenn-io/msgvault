<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { ADD_SOURCE_KINDS, type CommandSourceKind } from '../../sources/add-source';
  import Dialog from '../common/Dialog.svelte';
  import AddSourceSteps from './AddSourceSteps.svelte';

  let { onclose, onOpenSettings }: { onclose: () => void; onOpenSettings: (category: string) => void } = $props();
  let kind = $state<CommandSourceKind>();
</script>

<Dialog title="Add source" {onclose}>
  {#if kind}
    <AddSourceSteps {kind} onback={() => (kind = undefined)} />
  {:else}
    <div class="picker">
      <ul class="overview" aria-label="Source kinds">
        {#each ADD_SOURCE_KINDS as source (source.id)}
          <li class="row">
            <div><span class="name">{source.name}</span><p>{source.description}</p></div>
            <Button size="sm" label="Set up" ariaLabel={`Set up ${source.name}`} onclick={() => {
              if ('settings' in source) { onOpenSettings(source.settings); onclose(); }
              else kind = source;
            }} />
          </li>
        {/each}
      </ul>
      <p>Slack, Teams, WhatsApp, meeting notes, calendars, and more: <a href="https://msgvault.io/docs/guides/sources/" target="_blank" rel="noreferrer">see every source</a></p>
    </div>
  {/if}
</Dialog>

<style>
  .picker { display: grid; gap: var(--space-4); }
  .overview { margin: 0; padding: 0; border: 1px solid var(--border-default); border-radius: var(--radius-md); list-style: none; }
  .row { display: grid; grid-template-columns: minmax(0, 1fr) max-content; align-items: center; gap: var(--control-gap) var(--section-gap); padding: var(--pane-pad-y) var(--pane-pad-x); }
  .row + .row { border-top: 1px solid var(--border-default); }
  .name { color: var(--text-primary); font-weight: var(--font-weight-medium); }
  p { margin: 0; color: var(--text-secondary); font-size: var(--font-size-sm); }
  a { color: var(--link-ink); }
</style>
