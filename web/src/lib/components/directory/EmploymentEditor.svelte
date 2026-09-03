<script lang="ts">
  import { appShortcuts, Button, Checkbox, Modal, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment, EmploymentBody, Organization } from '../../directory/models';
  import EmploymentCurrentData from './EmploymentCurrentData.svelte';

  interface Props {
    controller: DirectoryEntityController;
    personID: number;
    employment?: Employment;
    action?: 'edit' | 'end';
    onDone?: () => void;
    onClose?: () => void;
  }

  let { controller, personID, employment = undefined, action = 'edit', onDone = () => undefined, onClose = () => undefined }: Props = $props();
  const initialEmployment = untrack(() => employment);
  const initialAction = untrack(() => action);
  let organizationID = $state(initialEmployment ? String(initialEmployment.organization_id) : '');
  let title = $state(initialEmployment?.title ?? '');
  let role = $state(initialEmployment?.role ?? '');
  let department = $state(initialEmployment?.department ?? '');
  let location = $state(initialEmployment?.location ?? '');
  let description = $state(initialEmployment?.description ?? '');
  let startDate = $state(partialDate(initialEmployment?.start_date));
  let endDate = $state(partialDate(initialEmployment?.end_date));
  let isCurrent = $state(initialEmployment?.is_current ?? true);
  let isPrimary = $state(initialEmployment?.is_primary ?? false);
  let loading = $state(!!initialEmployment);
  let submitting = $state(false);
  let message = $state('');
  let conflictCurrent = $state<Employment>();
  let conflictOrganization = $state<Organization>();
  let selectedOrganization = $state<Organization>();
  let releaseScope: (() => void) | undefined;
  const organizationOptions = $derived.by(() => {
    const organizations = [...controller.organizations];
    if (selectedOrganization && !organizations.some((item) => item.id === selectedOrganization?.id)) organizations.push(selectedOrganization);
    return [
      { value: '', label: 'Choose an organization', disabled: true },
      ...organizations.map((organization) => ({ value: String(organization.id), label: organization.name }))
    ];
  });

  onMount(() => {
    releaseScope = appShortcuts.pushScope('directory-employment-editor');
    void prepare();
  });
  onDestroy(() => releaseScope?.());

  async function prepare(): Promise<void> {
    message = '';
    if (!initialEmployment) {
      if (controller.organizations.length === 0) await controller.refreshOrganizations();
      return;
    }
    loading = true;
    try {
      const current = await controller.prepareEmploymentMutation(initialEmployment.id);
      organizationID = String(current.organization_id);
      title = current.title ?? '';
      role = current.role ?? '';
      department = current.department ?? '';
      location = current.location ?? '';
      description = current.description ?? '';
      startDate = partialDate(current.start_date);
      endDate = partialDate(current.end_date);
      isCurrent = current.is_current;
      isPrimary = current.is_primary;
      try {
        selectedOrganization = (await controller.prepareOrganizationMutation(current.organization_id)).organization;
      } catch {
        selectedOrganization = controller.organizations.find((item) => item.id === current.organization_id) ?? {
          id: current.organization_id,
          revision: 0,
          name: `Organization ${current.organization_id}`,
          kind: 'other',
          created_at: current.created_at,
          updated_at: current.updated_at
        };
      }
    } catch (cause: unknown) {
      message = errorMessage(cause);
    } finally {
      loading = false;
    }
  }

  function body(current: Employment | undefined = undefined): EmploymentBody {
    return {
      person_id: current?.person_id ?? personID,
      organization_id: current?.organization_id ?? Number(organizationID),
      source: (current?.source ?? 'user') as EmploymentBody['source'],
      ...(current?.source_ref === undefined ? {} : { source_ref: current.source_ref }),
      ...(title.trim() ? { title: title.trim() } : { title: null }),
      ...(role.trim() ? { role: role.trim() } : { role: null }),
      ...(department.trim() ? { department: department.trim() } : { department: null }),
      ...(location.trim() ? { location: location.trim() } : { location: null }),
      ...(description.trim() ? { description: description.trim() } : { description: null }),
      ...(startDate.trim() ? { start_date: startDate.trim() } : { start_date: null }),
      ...(endDate.trim() ? { end_date: endDate.trim() } : { end_date: null }),
      is_current: isCurrent,
      is_primary: isPrimary,
      ...(current?.address_id !== undefined ? { address_id: current.address_id } : {}),
      ...(current?.confidence !== undefined ? { confidence: current.confidence } : {})
    };
  }

  async function submit(): Promise<void> {
    if (submitting || !organizationID || controller.createBlocked.employments) return;
    submitting = true;
    message = '';
    conflictCurrent = undefined;
    conflictOrganization = undefined;
    try {
      const result = initialEmployment
        ? initialAction === 'end'
          ? await controller.endEmployment(initialEmployment.id, { end_date: endDate.trim() })
          : await controller.updateEmployment(initialEmployment.id, (freshEmployment) => body(freshEmployment))
        : await controller.createEmployment({ ...body(), source: 'user' });
      if (result.ok) {
        onDone();
        return;
      }
      if (result.kind === 'conflict') {
        conflictCurrent = result.current;
        if (result.current) conflictOrganization = await loadConflictOrganization(result.current.organization_id);
        message = `This employment changed elsewhere. ${result.message}`;
      } else if (result.kind === 'unknown' || result.kind === 'blocked') {
        message = `The create outcome is unknown. ${result.message}`;
      } else {
        message = result.message;
      }
    } finally {
      submitting = false;
    }
  }

  async function loadConflictOrganization(id: number): Promise<Organization | undefined> {
    if (selectedOrganization?.id === id) return selectedOrganization;
    try {
      return (await controller.prepareOrganizationMutation(id)).organization;
    } catch {
      return controller.organizations.find((organization) => organization.id === id);
    }
  }

  async function refreshAfterUnknown(): Promise<void> {
    await controller.refreshEmployments();
    if (!controller.createBlocked.employments) message = '';
  }

  function requestClose(): void {
    if (!submitting) onClose();
  }

  function partialDate(value: { year?: number; month?: number; day?: number } | undefined): string {
    if (!value) return '';
    const year = value.year?.toString().padStart(4, '0');
    const month = value.month?.toString().padStart(2, '0');
    const day = value.day?.toString().padStart(2, '0');
    return [year, month, day].filter(Boolean).join('-');
  }

  function errorMessage(value: unknown): string {
    return value instanceof Error && value.message ? value.message : 'Unable to load the employment.';
  }
</script>

<Modal
  title={initialAction === 'end' ? 'End employment' : initialEmployment ? 'Edit employment' : 'Add employment'}
  ariaLabel={initialAction === 'end' ? 'End employment' : initialEmployment ? 'Edit employment' : 'Add employment'}
  closeLabel="Close employment editor"
  onclose={requestClose}
>
  <form class="editor" aria-busy={loading || submitting} onsubmit={(event) => { event.preventDefault(); void submit(); }}>
    {#if loading}
      <p role="status">Loading current employment…</p>
    {:else if initialAction === 'end'}
      <label>End date<TextInput ariaLabel="Employment end date" bind:value={endDate} placeholder="YYYY, YYYY-MM, or YYYY-MM-DD" required block disabled={submitting} /></label>
    {:else}
      <label>Organization<SelectDropdown title="Organization" value={organizationID} options={organizationOptions} onchange={(value) => { organizationID = value; }} disabled={submitting || !!initialEmployment} /></label>
      <label>Title<TextInput ariaLabel="Employment title" bind:value={title} block disabled={submitting} /></label>
      <label>Role<TextInput ariaLabel="Employment role" bind:value={role} block disabled={submitting} /></label>
      <label>Department<TextInput ariaLabel="Employment department" bind:value={department} block disabled={submitting} /></label>
      <label>Location<TextInput ariaLabel="Employment location" bind:value={location} block disabled={submitting} /></label>
      <label>Start date<TextInput ariaLabel="Employment start date" bind:value={startDate} placeholder="YYYY, YYYY-MM, or YYYY-MM-DD" block disabled={submitting} /></label>
      <label>End date<TextInput ariaLabel="Employment end date" bind:value={endDate} placeholder="YYYY, YYYY-MM, or YYYY-MM-DD" block disabled={submitting} /></label>
      <label>Description<textarea aria-label="Employment description" bind:value={description} disabled={submitting}></textarea></label>
      <div class="checks"><Checkbox checked={isCurrent} label="Current employment" onchange={(checked) => { isCurrent = checked; }} disabled={submitting} /><Checkbox checked={isPrimary} label="Primary employment" onchange={(checked) => { isPrimary = checked; }} disabled={submitting} /></div>
    {/if}
    {#if message}
      <div role="alert">
        <p>{message}</p>
        {#if conflictCurrent}
          <EmploymentCurrentData employment={conflictCurrent} organization={conflictOrganization} />
        {/if}
      </div>
    {/if}
    {#if controller.createBlocked.employments}<Button label="Refresh employment records" disabled={submitting} onclick={() => void refreshAfterUnknown()} />{/if}
    <div class="actions">
      <Button label="Cancel" disabled={submitting} onclick={requestClose} />
      <Button type="submit" tone="info" surface="solid"
        label={initialAction === 'end' ? 'Confirm end employment' : initialEmployment ? 'Save employment' : 'Create employment'}
        disabled={loading || submitting || !organizationID || (initialAction === 'end' && !endDate.trim()) || controller.createBlocked.employments} />
    </div>
  </form>
</Modal>

<style>
  .editor { display: grid; gap: var(--space-3); min-width: min(30rem, 80vw); }
  label { display: grid; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  textarea { min-height: 5rem; resize: vertical; padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); }
  p { margin: 0; } [role="alert"] { color: var(--text-danger); }
  .checks, .actions { display: flex; gap: var(--space-2); flex-wrap: wrap; }
  .actions { justify-content: flex-end; }
</style>
