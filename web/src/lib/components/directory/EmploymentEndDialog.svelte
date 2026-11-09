<script lang="ts">
  import { onMount } from 'svelte';
  import { localDateStr } from '@kenn-io/kit-ui';
  import { formatPartialDate, readablePartialDate } from '../../directory/dates';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment, Prepared } from '../../directory/models';
  import ConflictNotice from '../common/ConflictNotice.svelte';
  import { employmentFacts } from '../../directory/conflict';
  import { WriteAttempt } from '../../directory/write-attempt.svelte';
  import DateField from '../common/DateField.svelte';
  import Dialog from '../common/Dialog.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  let { controller, employment, onClose }: { controller: DirectoryEntityController; employment: Employment; onClose: () => void } = $props();
  let endDate = $state(localDateStr(new Date()));
  const attempt = new WriteAttempt<Prepared<Employment>>();
  let opened = $state<Prepared<Employment>>();
  const prepare = async () => { opened = await attempt.prepare(() => controller.prepareEmploymentMutation(employment.id)); };
  onMount(() => { void prepare(); });
  const shown = $derived(opened?.data ?? employment);
  const employerName = $derived(controller.organizationName(shown.organization_id));
  let dateError = $state<string | null>(null);
  // The title names the action; the line under it names the job: "Garden designer at Lumen Grove · since Mar 2024".
  const since = $derived(readablePartialDate(shown.start_date));
  const context = $derived([`${shown.title || shown.role || 'Job'} at ${employerName}`, since ? `since ${since}` : ''].filter(Boolean).join(' · '));
  function close(): void { if (!attempt.busy) { if (attempt.hadConflict) void controller.acceptSavedEmployment(); attempt.reset(); onClose(); } }
  async function submit(): Promise<void> {
    if (attempt.busy || !opened || !endDate.trim() || dateError) return;
    if (await attempt.run(current => controller.endEmployment(employment.id, { end_date: endDate.trim() }, current ?? opened))) close();
  }
</script>
<Dialog title="End job" width="small" onclose={close} primaryLabel={attempt.conflict ? 'Keep yours' : 'End'} onprimary={() => void submit()} primaryDisabled={!opened || !endDate.trim() || !!dateError} busy={attempt.busy}
  cancelLabel={attempt.conflict ? 'Use saved' : 'Cancel'}>
  <form onsubmit={(event) => { event.preventDefault(); void submit(); }}>
    <p class="context">{context}</p>
    <DateField bind:value={endDate} start={formatPartialDate((attempt.conflict?.data ?? attempt.prepared?.data ?? shown).start_date)} bind:error={dateError} disabled={attempt.busy} />
  </form>
  {#snippet notice()}
    {#if attempt.conflict}<ConflictNotice noun="job" saved={attempt.conflict.data} record={employmentFacts(attempt.conflict.data, controller, controller.personID)} message={attempt.details} />
    {:else if !opened && attempt.details}<InlineNotice message="Couldn't load this job." details={attempt.details} actionLabel="Retry" onaction={() => void prepare()} />
    {:else if attempt.failed}<InlineNotice message="Couldn't end this job." details={attempt.details} actionLabel="Retry" onaction={() => void submit()} />{/if}
  {/snippet}
</Dialog>

<style>
  /* The context line (Body) 16 above End date; the field's hint line 4 below it, kept while empty. */
  form { display: grid; gap: var(--space-pane); }
  p { margin: 0; }
  .context { color: var(--text-secondary); font-size: var(--type-body-size); line-height: var(--type-body-line); }
</style>
