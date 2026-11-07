<script lang="ts">
  import { appShortcuts, autoReposition, Button, dismissable, floatingPopoverStyle, IconButton, localDateStr, TextInput } from '@kenn-io/kit-ui';
  import X from '@lucide/svelte/icons/x';
  import { onDestroy, onMount, tick } from 'svelte';
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
  let active = true;
  let rowActions: HTMLElement | null = null;
  const employerName = $derived(controller.organizationRecords.get(employment.organization_id)?.organization.name ?? controller.organizations.find(item => item.id === employment.organization_id)?.name ?? 'organization');
  const uid = $props.id();
  const dateError = $derived(intervalDateError(endDate));

  function position(): void {
    if (!trigger || !panel) return;
    const anchor = trigger.getBoundingClientRect();
    const right = trigger.closest('.detail-row')?.getBoundingClientRect().right ?? anchor.right;
    style = floatingPopoverStyle({ trigger: { left: anchor.left, right, top: anchor.top, bottom: anchor.bottom }, viewportWidth: window.innerWidth, viewportHeight: window.innerHeight, popoverWidth: panel.offsetWidth, popoverHeight: panel.offsetHeight, align: 'end', placement: 'below', edgeGap: 16 }) + `; max-height: ${Math.max(0, window.innerHeight - trigger.getBoundingClientRect().bottom - 20)}px`;
  }
  function close(): void {
    if (submitting || !active) return;
    if (document.activeElement === document.body || panel?.contains(document.activeElement)) rowActions?.querySelector<HTMLButtonElement>('[aria-label="Edit employment"]')?.focus({ preventScroll: true });
    onClose();
  }
  onDestroy(() => { active = false; });
  onMount(() => { rowActions = trigger.parentElement; void tick().then(() => {
    if (panel) {
      const overflow = trigger.getBoundingClientRect().bottom + panel.scrollHeight + 6 - (window.innerHeight - 16);
      if (overflow > 0) trigger.closest('.person-page')?.scrollBy({ top: overflow });
    }
    position();
    panel?.querySelector<HTMLInputElement>('input')?.focus({ preventScroll: true });
  }); });
  async function submit(): Promise<void> {
    if (submitting || !endDate.trim() || dateError) return;
    submitting = true;
    message = '';
    current = undefined;
    try {
      const result = await controller.endEmployment(employment.id, { end_date: endDate.trim() });
      if (result.ok) { submitting = false; close(); return; }
      message = result.kind === 'conflict' ? 'This employment changed elsewhere.' : result.message;
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
    <header><h3>End {employment.title || employment.role || 'employment'} at {employerName}</h3><IconButton ariaLabel="Close end employment" disabled={submitting} onclick={close}><X size={16} /></IconButton></header>
    <form onsubmit={(event) => { event.preventDefault(); void submit(); }}>
      <label>End date<TextInput ariaLabel="Employment end date" bind:value={endDate} block required disabled={submitting} ariaDescribedby={dateError ? `${uid}-date-error` : undefined} /></label>
      {#if dateError}<p id="{uid}-date-error" class="error">{dateError}</p>{/if}
      {#if message}<div role="alert"><p>{message}</p>{#if current}<EmploymentCurrentData employment={current} organization={controller.organizationRecords.get(current.organization_id)?.organization ?? controller.organizations.find(item => item.id === current?.organization_id)} />{/if}</div>{/if}
      <div class="actions"><Button label="End" type="submit" tone="info" surface="solid" busy={submitting} disabled={!endDate.trim() || !!dateError} /></div>
    </form>
  </div>

<style>
  .end-panel { position: fixed; z-index: var(--z-popover); width: 320px; box-sizing: border-box; max-width: calc(100vw - 32px); max-height: calc(100vh - 32px); padding: var(--space-6); overflow: auto; margin: 0; }
  header { display: flex; justify-content: space-between; align-items: center; gap: var(--space-2); margin-bottom: var(--space-3); }
  h3, p { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  form { display: grid; gap: var(--field-gap); }
  label { display: grid; gap: var(--label-gap); color: var(--text-muted); font-size: var(--font-size-sm); }
  .actions { display: flex; justify-content: flex-end; }
  .actions :global(.kit-button--busy .kit-button__label) { opacity: 0; }
  .error, [role="alert"] { color: var(--text-danger); }
</style>
