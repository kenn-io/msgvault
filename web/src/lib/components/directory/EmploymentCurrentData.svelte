<script lang="ts">
  import type { Employment, Organization } from '../../directory/models';
  import { historyProvenance } from './history-format';

  let { employment, organization = undefined }: { employment: Employment; organization?: Organization } = $props();

  function when(value: Employment['start_date']): string {
    if (!value?.year) return '';
    return new Intl.DateTimeFormat(undefined, { year: 'numeric', ...(value.month ? { month: 'short' } : {}), ...(value.day ? { day: 'numeric' } : {}), timeZone: 'UTC' }).format(new Date(Date.UTC(value.year, (value.month ?? 1) - 1, value.day ?? 1)));
  }

  const rows = $derived([
    { label: 'Organization', value: organization?.id === employment.organization_id ? organization.name : 'Employer unavailable' },
    { label: 'Title', value: employment.title },
    { label: 'Role', value: employment.role },
    { label: 'Department', value: employment.department },
    { label: 'Location', value: employment.location },
    { label: 'Description', value: employment.description },
    { label: 'Dates', value: `${when(employment.start_date) || 'Unspecified'} – ${employment.is_current ? 'present' : when(employment.end_date) || 'unspecified'}` },
    { label: 'Status', value: employment.is_current ? 'Current' : 'Past work' },
  ].filter(row => row.value));
</script>

<section aria-label="Current saved employment">
  <h4>Current saved employment</h4>
  <dl>
    {#each rows as row}<div><dt>{row.label}</dt><dd>{row.value}</dd></div>{/each}
  </dl>
  <p>{historyProvenance(employment)}</p>
</section>

<style>
  section { display: grid; gap: var(--space-2); margin-top: var(--space-3); color: var(--text-primary); font-weight: normal; }
  h4, dl, dd, p { margin: 0; }
  div { display: flex; justify-content: space-between; gap: var(--field-gap); padding: var(--space-2) 0; border-bottom: 1px solid var(--border-muted); }
  div:last-child { border-bottom: 0; }
  dd { min-width: 0; overflow-wrap: anywhere; }
  dt { order: 1; }
  dt, p { color: var(--text-muted); font-size: var(--font-size-sm); font-weight: normal; }
  @media (max-width: 640px) { div { flex-direction: column; } }
</style>
