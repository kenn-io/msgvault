<script lang="ts">
  import { appShortcuts, Checkbox, FormField, SelectDropdown } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';

  import { formatPartialDate as partialDate, intervalDateError } from '../../directory/dates';
  import { organizationKindOptions as kindOptions } from '../../directory/labels';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment, EmploymentBody, Organization, OrganizationCreateBody, Prepared } from '../../directory/models';
  import Dialog from '../common/Dialog.svelte';
  import FieldPicker from '../common/FieldPicker.svelte';
  import DateRange from '../common/DateRange.svelte';
  import { Disclosure } from '@kenn-io/kit-ui';
  import InlineNotice from '../common/InlineNotice.svelte';
  import ConflictNotice from '../common/ConflictNotice.svelte';
  import { conflict, employmentFacts } from '../../directory/conflict';
  import { WriteAttempt } from '../../directory/write-attempt.svelte';

  interface Props {
    controller: DirectoryEntityController;
    personID: number;
    employment?: Employment;
    onDone?: () => void;
    onClose?: () => void;
  }

  let { controller, personID, employment = undefined, onDone = () => undefined, onClose = () => undefined }: Props = $props();
  const initialEmployment = untrack(() => employment);
  let creatingOrganization = $state(false);
  let organizationName = $state('');
  let organizationDomain = $state('');
  let organizationKind = $state('company');
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
  let organizationQuery = $state('');
  let moreDetails = $state(!!(initialEmployment?.department || initialEmployment?.location || initialEmployment?.description));
  const attempt = new WriteAttempt<Prepared<Employment>>();
  // The job as the person opened it, so a conflict can tell their edits from changes made elsewhere.
  let opened = $state<Prepared<Employment>>();
  const loading = $derived(!!initialEmployment && !opened && !attempt.details);
  const loadFailed = $derived(!!initialEmployment && !opened && !!attempt.details);
  let selectedOrganization = $state<Organization>();
  const showPrimary = $derived(!initialEmployment && isCurrent && controller.employments.some(job => job.is_current) && !controller.employments.some(job => job.is_current && job.is_primary));
  // The dates the next write sends: after a conflict, Keep yours keeps a start the person didn't change from the saved version.
  const kept = $derived(merged((attempt.conflict ?? attempt.prepared)?.data));
  const datesInvalid = $derived(!!intervalDateError(kept.start_date) || (kept.end_date !== null && !!intervalDateError(kept.end_date, kept.start_date)));
  const saveDisabled = $derived(loading || loadFailed || (creatingOrganization ? !organizationName.trim() || controller.createBlocked.organizations : !organizationID) || datesInvalid || controller.createBlocked.employments);
  const organizationCreateBlocked = $derived(creatingOrganization && controller.createBlocked.organizations);
  let releaseScope: (() => void) | undefined;
  const organizationOptions = $derived.by(() => {
    const organizations = [...controller.organizations];
    if (selectedOrganization && !organizations.some((item) => item.id === selectedOrganization?.id)) organizations.push(selectedOrganization);
    return organizations.map(organization => ({ name: String(organization.id), label: organization.name, meta: organization.primary_domain ?? '' }));
  });

  onMount(() => {
    releaseScope = appShortcuts.pushScope('directory-employment-editor');
    void prepare();
  });
  onDestroy(() => releaseScope?.());

  async function prepare(): Promise<void> {
    if (!initialEmployment) {
      if (controller.organizations.length === 0) await controller.refreshOrganizations();
      return;
    }
    opened = await attempt.prepare(async () => {
      const prepared = await controller.prepareEmploymentMutation(initialEmployment.id);
      const current = prepared.data;
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
      moreDetails = !!(department || location || description);
      try {
        selectedOrganization = (await controller.prepareOrganizationMutation(current.organization_id)).data.organization;
      } catch {
        selectedOrganization = controller.organizations.find((item) => item.id === current.organization_id) ?? {
          id: current.organization_id,
          revision: 0,
          name: 'this organization',
          kind: 'other',
          created_at: current.created_at,
          updated_at: current.updated_at
        };
      }
      return prepared;
    });
  }

  // The fields Edit job changes, as the form holds them, so the draft and a saved job compare field by field. End date and
  // Current are one choice: a current job has no end (null), so Keep yours keeps or drops them together.
  const jobFields = (job: Employment) => ({ title: job.title ?? '', role: job.role ?? '', department: job.department ?? '', location: job.location ?? '', description: job.description ?? '', start_date: partialDate(job.start_date), end_date: job.is_current ? null : partialDate(job.end_date) });
  type JobFields = ReturnType<typeof jobFields>;

  // Yours is only what the person changed since opening; every other field keeps the version being written.
  function merged(current: Employment | undefined): JobFields {
    const draft: JobFields = { title, role, department, location, description, start_date: startDate, end_date: isCurrent ? null : endDate };
    if (!current || !opened) return draft;
    const was = jobFields(opened.data), now = jobFields(current);
    const same = (a: string | null, b: string | null) => a?.trim() === b?.trim();
    return Object.fromEntries((Object.keys(draft) as (keyof JobFields)[]).map(key => [key, same(draft[key], was[key]) ? now[key] : draft[key]])) as JobFields;
  }

  function body(current: Employment | undefined = undefined): EmploymentBody {
    const job = merged(current);
    return {
      person_id: current?.person_id ?? personID,
      organization_id: current?.organization_id ?? Number(organizationID),
      source: (current?.source ?? 'user') as EmploymentBody['source'],
      ...(current?.source_ref === undefined ? {} : { source_ref: current.source_ref }),
      title: job.title.trim() || null,
      role: job.role.trim() || null,
      department: job.department.trim() || null,
      location: job.location.trim() || null,
      description: job.description.trim() || null,
      start_date: job.start_date.trim() || null,
      ...(job.end_date === null ? {} : { end_date: job.end_date.trim() || null }),
      is_current: job.end_date === null,
      ...(showPrimary ? { is_primary: isPrimary } : {}),
      ...(current?.address_id !== undefined ? { address_id: current.address_id } : {}),
      ...(current?.confidence !== undefined ? { confidence: current.confidence } : {})
    };
  }

  async function submit(): Promise<void> {
    if (saveDisabled || attempt.busy) return;
    const ok = await attempt.run(async current => {
      if (creatingOrganization) {
        const created = await controller.createOrganization({ name: organizationName.trim(), kind: organizationKind as OrganizationCreateBody['kind'], primary_domain: organizationDomain.trim() || null });
        if (!created.ok) return created;
        selectedOrganization = created.entity;
        organizationID = String(created.entity!.id);
        creatingOrganization = false;
      }
      return initialEmployment
        ? controller.updateEmployment(initialEmployment.id, fresh => body(fresh), current ?? opened)
        : controller.createEmployment({ ...body(), source: 'user' });
    });
    if (ok) onDone();
  }

  async function refreshAfterUnknown(): Promise<void> {
    await controller.refreshEmployments();
    if (!controller.createBlocked.employments) attempt.details = '';
  }

  function requestClose(): void {
    if (!attempt.busy) { if (attempt.conflict) void controller.refreshEmployments(); attempt.reset(); onClose(); }
  }
</script>

<Dialog title={initialEmployment ? 'Edit job' : 'Add job'} onclose={requestClose} primaryLabel={loadFailed ? undefined : attempt.conflict ? 'Keep yours' : initialEmployment ? 'Save' : 'Add job'} onprimary={() => void submit()} primaryDisabled={saveDisabled} busy={attempt.busy}
  cancelLabel={attempt.conflict ? 'Use saved' : 'Cancel'}>
  {#if !loadFailed}<form class="editor" aria-busy={loading || attempt.busy} onsubmit={(event) => { event.preventDefault(); void submit(); }}>
    {#if loading}<p role="status">Loading job…</p>{:else}
      {#if initialEmployment}<p class="context">At {selectedOrganization?.name ?? 'this organization'}</p>
      {:else}
      <div class="field"><span>Organization</span>
          <FieldPicker remote title="Organization" placeholder="Organization" fallbackLabel={creatingOrganization ? 'New organization' : 'Search or create'} value={creatingOrganization ? '' : organizationID} options={organizationOptions}
            loading={controller.organizationsLoading} errorDetails={controller.errors.organizations} error={controller.errors.organizations ? "Couldn't load organizations." : ''}
            onretry={() => void controller.refreshOrganizations(organizationQuery)}
            onquery={query => { organizationQuery = query; void controller.refreshOrganizations(query); }}
            onselect={value => { organizationID = value; selectedOrganization = controller.organizations.find(item => String(item.id) === value); }}
            createLabel={'Create organization “{query}”'} existingLabel="Choose an existing organization instead"
            oncreate={query => { organizationName = query; creatingOrganization = true; }} creating={creatingOrganization}
            onexisting={() => creatingOrganization = false} disabled={attempt.busy}>
            {#snippet creation()}
              <FormField field={{ id: 'work-org-name', label: 'Name', value: organizationName, disabled: attempt.busy }} oninput={value => organizationName = value} />
              <div class="form-pair">
                <label>Kind<SelectDropdown title="Organization kind" value={organizationKind} options={kindOptions} onchange={value => organizationKind = value} disabled={attempt.busy} /></label>
                <FormField field={{ id: 'work-org-domain', label: 'Domain', value: organizationDomain, disabled: attempt.busy }} oninput={value => organizationDomain = value} />
              </div>
            {/snippet}
          </FieldPicker>
      </div>
      {/if}
      <div class="form-pair"><FormField field={{ id: 'work-title', label: 'Job title', value: title, disabled: attempt.busy }} oninput={value => title = value} /><FormField field={{ id: 'work-role', label: 'Role on the team', value: role, disabled: attempt.busy }} oninput={value => role = value} /></div>
      <DateRange bind:start={startDate} bind:end={endDate} bind:current={isCurrent} endAfter={kept.start_date} disabled={attempt.busy} />
      {#if showPrimary}<Checkbox label="Primary job" checked={isPrimary} onchange={value => isPrimary = value} disabled={attempt.busy} />{/if}
      <Disclosure variant="plain" class="fold" title="More details" bind:open={moreDetails}>
        <div class="more">
          <div class="form-pair"><FormField field={{ id: 'work-department', label: 'Department', value: department, disabled: attempt.busy }} oninput={value => department = value} /><FormField field={{ id: 'work-location', label: 'Location', value: location, disabled: attempt.busy }} oninput={value => location = value} /></div>
          <label>Description<textarea aria-label="Description" bind:value={description} disabled={attempt.busy}></textarea></label>
        </div>
      </Disclosure>
    {/if}
  <button type="submit" hidden>Save</button>
</form>{/if}
  {#snippet notice()}
    {#if attempt.conflict}
      {@const facts = employmentFacts({ title, role, start_date: startDate, end_date: endDate, is_current: isCurrent, department, location, description })}
      {@const openedFacts = employmentFacts(opened?.data ?? initialEmployment!, controller)}
      <!-- Yours lists only the fields the person changed, the ones Keep yours writes. -->
      {@const yours = Object.fromEntries(['Job title', 'Role on the team', 'Start date', 'End date', 'Department', 'Location', 'Description'].filter(label => facts[label] !== openedFacts[label]).map(label => [label, facts[label]]))}
      <ConflictNotice noun="job" saved={attempt.conflict.data} {...conflict(employmentFacts(attempt.conflict.data, controller), openedFacts, yours)} message={attempt.details} />
    {:else if attempt.details || controller.createBlocked.employments || organizationCreateBlocked}
      <InlineNotice message={organizationCreateBlocked ? "Couldn't confirm the organization was saved." : controller.createBlocked.employments ? "Couldn't confirm the job was saved." : loadFailed ? "Couldn't load this job." : "Couldn't save this job."} details={attempt.details} actionLabel={controller.createBlocked.employments || organizationCreateBlocked ? 'Check again' : 'Retry'} onaction={() => { if (organizationCreateBlocked) void controller.refreshOrganizations().then(() => { if (!controller.createBlocked.organizations) attempt.details = ''; }); else if (controller.createBlocked.employments) void refreshAfterUnknown(); else if (loadFailed) void prepare(); else void submit(); }} />
    {/if}
  {/snippet}
</Dialog>

<style>
  /* L4: fields 16 apart, each label (Meta, 600) 4 above its field; pairs are .form-pair. */
  .editor { display: grid; gap: var(--space-pane); min-width: 0; }
  label, .field { display: grid; gap: var(--space-tight); align-content: start; min-width: 0; color: var(--text-secondary); font-size: var(--type-meta-size); font-weight: var(--font-weight-semibold); line-height: var(--type-meta-line); }
  label > :global(*), .field > :global(:not(span)) { font-weight: normal; }
  /* "At Lumen Grove": a Meta line 16 above Job title. */
  .context { color: var(--text-secondary); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  /* More details' fields keep the 16 rhythm; the 28 fold row above them is their only gap. */
  .more { display: grid; gap: var(--space-pane); }
  textarea { resize: vertical; border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); font: inherit; font-weight: normal; }
  p { margin: 0; }
</style>
