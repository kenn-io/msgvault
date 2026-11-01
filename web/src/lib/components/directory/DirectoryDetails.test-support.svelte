<script lang="ts">
  import { onDestroy, type ComponentProps } from 'svelte';
  import DirectoryWorkspace from './DirectoryWorkspace.svelte';
  import PersonFields from '../people/PersonFields.svelte';
  import PersonRecordActions from './PersonRecordActions.svelte';
  import UndoToast from '../common/UndoToast.svelte';
  import { PendingUndo } from '../../people/pending-undo.svelte';
  let props: ComponentProps<typeof DirectoryWorkspace> = $props();
  const pending = new PendingUndo();
  onDestroy(() => { void pending.flush(); });
</script>
<DirectoryWorkspace {...props} />
{#if props.controller.profile && props.controller.selectedPersonID !== null}
  <PersonRecordActions client={props.client} controller={props.controller.profile} personID={props.controller.selectedPersonID} />
  <PersonFields controller={props.controller.profile} {pending} />
{/if}
{#if pending.current}
  {#key pending.current.id}<UndoToast message={pending.current.message} onUndo={() => pending.undo()} onExpire={() => void pending.flush()} />{/key}
{/if}
