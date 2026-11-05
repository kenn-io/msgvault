<script lang="ts">
  import { appShortcuts, autoReposition, Button, dismissable, floatingPopoverStyle, IconButton, localDateStr, TextInput } from '@kenn-io/kit-ui';
  import X from '@lucide/svelte/icons/x';
  import { tick } from 'svelte';
  import { intervalDateError } from '../../directory/dates';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment } from '../../directory/models';
  import EmploymentCurrentData from './EmploymentCurrentData.svelte';

  let { controller, employment }: { controller: DirectoryEntityController; employment: Employment } = $props();
  let open = $state(false);
  let trigger = $state<HTMLSpanElement>();
  let panel = $state<HTMLDivElement>();
  let style = $state('');
  let endDate = $state('');
  let loading = $state(false);
  let ready = $state(false);
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
    open = false;
    trigger?.querySelector('button')?.focus();
  }
  async function load(): Promise<void> {
    loading = true;
    ready = false;
    message = '';
    try {
      await controller.prepareEmploymentMutation(employment.id);
      ready = true;
    } catch (cause) { message = cause instanceof Error ? cause.message : 'Unable to load the employment.'; }
    finally { loading = false; }
  }
  async function show(): Promise<void> {
    if (open) { close(); return; }
    endDate = localDateStr(new Date());
    current = undefined;
    open = true;
    void load();
    await tick();
    position();
  }
  async function submit(): Promise<void> {
    if (!ready || submitting || !endDate.trim() || dateError) return;
    submitting = true;
    message = '';
    current = undefined;
    try {
      const result = await controller.endEmployment(employment.id, { end_date: endDate.trim() });
      if (result.ok) { submitting = false; close(); return; }
      message = result.kind === 'conflict' ? `This employment changed elsewhere. ${result.message}` : result.message;
      if (result.kind === 'conflict') current = result.current;
    } finally { submitting = false; }
  }
  $effect(() => {
    if (!open) return;
    const release = appShortcuts.pushScope('directory-employment-end');
    const reposition = autoReposition(() => [panel, trigger], position);
    const dismiss = dismissable({ owners: () => [panel, trigger], dismiss: close });
    return () => { dismiss(); reposition(); release(); };
  });
</script>

<span bind:this={trigger}><Button size="sm" label="End employment" shortLabel="End" ariaExpanded={open} onclick={() => void show()} /></span>
{#if open}
  <div class="end-panel kit-popover-card" role="dialog" aria-label="End employment" bind:this={panel} {style}>
    <header><h3>End employment</h3><IconButton ariaLabel="Close end employment" disabled={submitting} onclick={close}><X size={16} /></IconButton></header>
    {#if loading}<p role="status">Loading current employment…</p>{/if}
    <form onsubmit={(event) => { event.preventDefault(); void submit(); }}>
      <label>End date<TextInput ariaLabel="Employment end date" bind:value={endDate} block required disabled={loading || submitting || !ready} ariaDescribedby={dateError ? `${uid}-date-error` : undefined} /></label>
      {#if dateError}<p id="{uid}-date-error" class="error">{dateError}</p>{/if}
      {#if message}<div role="alert"><p>{message}</p>{#if current}<EmploymentCurrentData employment={current} organization={controller.organizationRecords.get(current.organization_id)?.organization ?? controller.organizations.find(item => item.id === current?.organization_id)} />{/if}</div>{/if}
      {#if !loading && !ready}<Button label="Retry employment load" onclick={() => void load()} />{/if}
      <div class="actions"><Button label="End" type="submit" tone="info" surface="solid" busy={submitting} disabled={loading || !ready || !endDate.trim() || !!dateError} /></div>
    </form>
  </div>
{/if}

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
