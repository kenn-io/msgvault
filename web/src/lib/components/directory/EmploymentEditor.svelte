<script lang="ts">
  import { appShortcuts, Button, Checkbox, Modal, SelectDropdown, Typeahead, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';

  import { intervalDateError } from '../../directory/dates';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment, EmploymentBody, Organization, OrganizationCreateBody } from '../../directory/models';
  import EmploymentCurrentData from './EmploymentCurrentData.svelte';

  interface Props {
    controller: DirectoryEntityController;
    personID: number;
    employment?: Employment;
    onDone?: () => void;
    onClose?: () => void;
  }

  let { controller, personID, employment = undefined, onDone = () => undefined, onClose = () => undefined }: Props = $props();
  const initialEmployment = untrack(() => employment);
  let formElement = $state<HTMLFormElement>();
  let creatingOrganization = $state(false);
  let organizationName = $state('');
  let organizationDomain = $state('');
  let organizationKind = $state('company');
  const kindOptions = ['company', 'nonprofit', 'school', 'government', 'household', 'other'].map((value) => ({ value, label: value[0]!.toUpperCase() + value.slice(1) }));
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
  const uid = $props.id();
  const startDateError = $derived(intervalDateError(startDate));
  const endDateError = $derived(intervalDateError(endDate));
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
    return organizations.map((organization) => ({ name: String(organization.id), label: organization.name }));
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
    if (submitting || (!organizationID && !(creatingOrganization && organizationName.trim())) || startDateError || endDateError || controller.createBlocked.employments || (creatingOrganization && controller.createBlocked.organizations)) return;
    if (isCurrent && endDate.trim()) { message = 'A current employment cannot have an end date.'; return; }
    if (startDate.trim() && endDate.trim()) {
      const start = startDate.trim().replaceAll('-', '');
      const end = endDate.trim().replaceAll('-', '');
      const precision = Math.min(start.length, end.length);
      if (end.slice(0, precision) < start.slice(0, precision)) { message = 'End date must not precede start date.'; return; }
    }
    submitting = true;
    message = '';
    conflictCurrent = undefined;
    conflictOrganization = undefined;
    try {
      if (creatingOrganization) {
        const created = await controller.createOrganization({ name: organizationName.trim(), kind: organizationKind as OrganizationCreateBody['kind'], primary_domain: organizationDomain.trim() || null });
        if (!created.ok) {
          message = created.kind === 'unknown' || created.kind === 'blocked' ? `The create outcome is unknown. ${created.message}` : created.message;
          return;
        }
        selectedOrganization = created.entity;
        organizationID = String(created.entity!.id);
        creatingOrganization = false;
      }
      const result = initialEmployment
        ? await controller.updateEmployment(initialEmployment.id, (freshEmployment) => body(freshEmployment))
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
  title={initialEmployment ? 'Edit employment' : 'Add employment'}
  ariaLabel={initialEmployment ? 'Edit employment' : 'Add employment'}
  closable={true}
  closeLabel="Close employment editor"
  width="560px"
  maxWidth="min(560px, calc(100vw - 32px))"
  onclose={requestClose}
>
  <form bind:this={formElement} class="editor" aria-busy={loading || submitting} onsubmit={(event) => { event.preventDefault(); void submit(); }}>
    <button type="submit" hidden disabled={loading || submitting}>Save employment</button>
    {#if loading}
      <p role="status">Loading current employment…</p>
    {:else}
      <div class="field">
        <div class="organization-heading"><span>Organization</span>
          {#if !initialEmployment}<Button size="sm" label={creatingOrganization ? 'Choose existing' : 'New organization'} disabled={submitting} onclick={() => { creatingOrganization = !creatingOrganization; }} />{/if}
        </div>
        {#if creatingOrganization}
          <div class="pair"><label>Name<TextInput ariaLabel="Organization name" bind:value={organizationName} required block disabled={submitting} /></label>
          <label>Domain<TextInput ariaLabel="Organization domain" bind:value={organizationDomain} block disabled={submitting} /></label></div>
          <label>Kind<SelectDropdown title="Organization kind" value={organizationKind} options={kindOptions} onchange={(value) => { organizationKind = value; }} disabled={submitting} /></label>
        {:else}
          <div class="org"><Typeahead title="Organization" placeholder="Organization" fallbackLabel="Choose an organization" value={organizationID} options={organizationOptions} remote loading={controller.organizationsLoading} error={controller.errors.organizations} onquery={(query) => void controller.refreshOrganizations(query)} onselect={(value) => { organizationID = value; selectedOrganization = controller.organizations.find(item => String(item.id) === value) ?? selectedOrganization; }} disabled={submitting || !!initialEmployment} /></div>
        {/if}
      </div>
      <div class="pair"><label>Title<TextInput ariaLabel="Employment title" bind:value={title} block disabled={submitting} /></label>
      <label>Role<TextInput ariaLabel="Employment role" bind:value={role} block disabled={submitting} /></label></div>
      <div class="pair"><label>Department<TextInput ariaLabel="Employment department" bind:value={department} block disabled={submitting} /></label>
      <label>Location<TextInput ariaLabel="Employment location" bind:value={location} block disabled={submitting} /></label></div>
      <div class="pair">
        <div class="field"><label>Start date<TextInput ariaLabel="Employment start date" bind:value={startDate} placeholder="2024, 2024-03, or 2024-03-15" block disabled={submitting} ariaDescribedby={startDateError ? `${uid}-start-date-error` : undefined} /></label>{#if startDateError}<p id="{uid}-start-date-error" class="field-error">{startDateError}</p>{/if}</div>
        <div class="field"><label>End date<TextInput ariaLabel="Employment end date" bind:value={endDate} placeholder="2024, 2024-03, or 2024-03-15" block disabled={submitting} ariaDescribedby={endDateError ? `${uid}-end-date-error` : undefined} /></label>{#if endDateError}<p id="{uid}-end-date-error" class="field-error">{endDateError}</p>{/if}</div>
      </div>
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
    {#if controller.createBlocked.organizations}<Button label="Refresh organizations" disabled={submitting} onclick={() => void controller.refreshOrganizations()} />{/if}
  </form>
  {#snippet footer()}
    <Button label="Cancel" disabled={submitting} onclick={requestClose} />
    <Button tone="info" surface="solid" busy={submitting}
      label={initialEmployment ? 'Save employment' : 'Create employment'}
      disabled={loading || (creatingOrganization ? !organizationName.trim() || controller.createBlocked.organizations : !organizationID) || Boolean(startDateError) || Boolean(endDateError) || controller.createBlocked.employments}
      onclick={() => formElement?.requestSubmit()} />
  {/snippet}
</Modal>


<style>
  .editor { display: grid; gap: var(--field-gap); min-width: 0; }
  label, .field { display: grid; gap: var(--label-gap); align-content: start; color: var(--text-secondary); font-size: var(--font-size-sm); }
  .pair { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--field-gap); }
  .org { --typeahead-max-width: 100%; --typeahead-min-width: 100%; min-width: 0; }
  .organization-heading { display: flex; justify-content: space-between; align-items: center; gap: var(--space-2); }
  textarea { min-height: 5rem; resize: vertical; padding: var(--space-2) var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); font: inherit; }
  .field-error { color: var(--text-danger); font-size: var(--font-size-xs); margin: 0; }
  p { margin: 0; } [role="alert"] { color: var(--text-danger); }
  .checks { display: flex; gap: var(--space-5); flex-wrap: wrap; }
  @media (max-width: 640px) { .pair { grid-template-columns: minmax(0, 1fr); } }
</style>
