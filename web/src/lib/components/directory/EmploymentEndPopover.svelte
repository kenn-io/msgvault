<script lang="ts">
  import { appShortcuts, autoReposition, Button, dismissable, floatingPopoverStyle, IconButton, localDateStr, TextInput } from '@kenn-io/kit-ui';
  import X from '@lucide/svelte/icons/x';
  import { onMount, tick } from 'svelte';
  import { intervalDateError } from '../../directory/dates';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment } from '../../directory/models';
  import EmploymentCurrentData from './EmploymentCurrentData.svelte';

  let { controller, employment, trigger, onClose }: { controller: DirectoryEntityController; employment: Employment; trigger: HTMLElement; onClose: () => void } = $props();
  let panel = $state<HTMLDivElement>();
  let style = $state('');
  let endDate = $state(localDateStr(new Date()));
  let submitting = $state(false);
  let message = $state('');
  let current = $state<Employment>();
  const uid = $props.id();
  const dateError = $derived(intervalDateError(endDate));

  function position(): void {
    if (!trigger || !panel) return;
    style = floatingPopoverStyle({ trigger: trigger.getBoundingClientRect(), viewportWidth: window.innerWidth, viewportHeight: window.innerHeight, popoverWidth: panel.offsetWidth, popoverHeight: panel.offsetHeight, align: 'start' });
  }
  function close(): void {
    if (submitting) return;
    trigger.focus();
    onClose();
  }
  onMount(() => { void tick().then(() => {
    position();
    panel?.querySelector<HTMLInputElement>('input')?.focus();
  }); });
  async function submit(): Promise<void> {
    if (submitting || !endDate.trim() || dateError) return;
    submitting = true;
    message = '';
    current = undefined;
    try {
      const result = await controller.endEmployment(employment.id, { end_date: endDate.trim() });
      if (result.ok) { submitting = false; close(); return; }
      message = result.kind === 'conflict' ? `This employment changed elsewhere. ${result.message}` : result.message;
      if (result.kind === 'conflict') {
        current = result.current;
        if (current) {
          try { await controller.prepareOrganizationMutation(current.organization_id); } catch { /* Keep the conflict and date draft if the employer cannot be loaded. */ }
        }
      }
    } finally { submitting = false; }
  }
  $effect(() => {
    const release = appShortcuts.pushScope('directory-employment-end');
    const reposition = autoReposition(() => [panel, trigger], position);
    const dismiss = dismissable({ owners: () => [panel, trigger], dismiss: close });
    return () => { dismiss(); reposition(); release(); };
  });
</script>

  <div class="end-panel kit-popover-card" role="dialog" aria-label="End employment" bind:this={panel} {style}>
    <header><h3>End employment</h3><IconButton ariaLabel="Close end employment" disabled={submitting} onclick={close}><X size={16} /></IconButton></header>
    <form onsubmit={(event) => { event.preventDefault(); void submit(); }}>
      <label>End date<TextInput ariaLabel="Employment end date" bind:value={endDate} block required disabled={submitting} ariaDescribedby={dateError ? `${uid}-date-error` : undefined} /></label>
      {#if dateError}<p id="{uid}-date-error" class="error">{dateError}</p>{/if}
      {#if message}<div role="alert"><p>{message}</p>{#if current}<EmploymentCurrentData employment={current} organization={controller.organizationRecords.get(current.organization_id)?.organization ?? controller.organizations.find(item => item.id === current?.organization_id)} />{/if}</div>{/if}
      <div class="actions"><Button label="End" type="submit" tone="info" surface="solid" busy={submitting} disabled={!endDate.trim() || !!dateError} /></div>
    </form>
  </div>

<style>
  .end-panel { position: fixed; z-index: var(--z-popover); width: 320px; box-sizing: border-box; max-width: calc(100vw - 32px); max-height: calc(100vh - 32px); padding: var(--space-4); overflow: auto; }
  header { display: flex; justify-content: space-between; align-items: center; gap: var(--space-2); margin-bottom: var(--space-3); }
  h3, p { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  form { display: grid; gap: var(--field-gap); }
  label { display: grid; gap: var(--label-gap); color: var(--text-muted); font-size: var(--font-size-sm); }
  .actions { display: flex; justify-content: flex-end; }
  .error, [role="alert"] { color: var(--text-danger); }
</style>
