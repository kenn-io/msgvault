<script lang="ts">
  import { ADD_SOURCE_KINDS, type CommandSourceKind } from '../../sources/add-source';
  import Dialog from '../common/Dialog.svelte';
  import ExistingPicker from '../common/ExistingPicker.svelte';
  import AddSourceSteps from './AddSourceSteps.svelte';

  interface Props {
    /** The kind whose steps show; unset shows the choices. */
    kind?: CommandSourceKind;
    /** First run: the choices sit on the page, and a choice opens its steps through `onchoose`. */
    inline?: boolean;
    onchoose?: (kind: CommandSourceKind) => void;
    onback?: () => void;
    onclose?: () => void;
    /** Done: the person ran the command, so the caller reloads its sources. */
    ondone?: () => void;
    onOpenSettings: (category: string) => void;
  }

  let { kind = $bindable(), inline = false, onchoose, onback, onclose = () => undefined, ondone = onclose, onOpenSettings }: Props = $props();
  const items = ADD_SOURCE_KINDS.map(source => ({ id: source.id, label: source.name, detail: `${source.description}${'settings' in source ? ' · opens Settings' : ''}` }));
  const title = $derived(!kind ? 'Add source' : kind.id === 'gmail' ? 'Add Gmail' : kind.id === 'microsoft' ? 'Add Microsoft account' : 'Add IMAP mailbox');

  function choose(id: string): void {
    const source = ADD_SOURCE_KINDS.find(item => item.id === id)!;
    if ('settings' in source) { onclose(); onOpenSettings(source.settings); }
    else if (inline) onchoose?.(source);
    else kind = source;
  }
</script>

{#snippet choices()}
  <div class="choices">
    <ExistingPicker {items} legend="Source kinds" onadvance={(item) => choose(item.id)} />
    <p>More sources: Slack, Teams, WhatsApp, MBOX… <a href="https://msgvault.io/docs/guides/sources/" target="_blank" rel="noreferrer">All sources</a></p>
  </div>
{/snippet}

{#if inline}
  <section aria-labelledby="first-source-heading"><h2 id="first-source-heading">Add your first source</h2>{@render choices()}</section>
{:else}
  <Dialog {title} {onclose} onback={kind ? onback ?? (() => (kind = undefined)) : undefined} backLabel="Back to Add source"
    primaryLabel={kind ? 'Done' : undefined} cancellable={false} onprimary={ondone}>
    {#if kind}{#key kind.id}<AddSourceSteps {kind} />{/key}
    {:else}{@render choices()}{/if}
  </Dialog>
{/if}

<style>
  /* The kinds are list items at the body's full width; "More sources" is a Meta line 16 below them. */
  section { display: grid; gap: var(--space-pane); max-width: var(--dialog-width-medium); }
  .choices { display: grid; gap: var(--space-pane); min-width: 0; }
  h2 { margin: 0; font-size: var(--type-title-size); line-height: var(--type-title-line); }
  p { margin: 0; color: var(--text-secondary); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  /* "All sources" is one link, so it wraps whole on a phone. */
  a:hover { text-decoration: underline; }
  a { text-decoration: none; color: var(--link-ink); white-space: nowrap; }
</style>
