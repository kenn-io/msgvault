<script lang="ts">
  import type { Employment, Organization } from '../../directory/models';

  interface Props {
    employment: Employment;
    organization?: Organization;
  }

  let { employment, organization = undefined }: Props = $props();

  function organizationText(): string {
    return organization?.id === employment.organization_id
      ? `${organization.name} (${employment.organization_id})`
      : `Organization ${employment.organization_id}`;
  }

  function partialDate(value: { year?: number; month?: number; day?: number } | undefined): string {
    if (!value) return '';
    return [
      value.year?.toString().padStart(4, '0'),
      value.month?.toString().padStart(2, '0'),
      value.day?.toString().padStart(2, '0')
    ].filter(Boolean).join('-');
  }
</script>

<dl aria-label="Current saved employment">
  <div><dt>Organization</dt><dd>{organizationText()}</dd></div>
  <div><dt>Person</dt><dd>{employment.person_id}</dd></div>
  <div><dt>Title</dt><dd>{employment.title || 'None'}</dd></div>
  <div><dt>Role</dt><dd>{employment.role || 'None'}</dd></div>
  <div><dt>Department</dt><dd>{employment.department || 'None'}</dd></div>
  <div><dt>Location</dt><dd>{employment.location || 'None'}</dd></div>
  <div><dt>Description</dt><dd>{employment.description || 'None'}</dd></div>
  <div><dt>Dates</dt><dd>{partialDate(employment.start_date) || 'Unspecified'} – {employment.is_current ? 'present' : partialDate(employment.end_date) || 'unspecified'}</dd></div>
  <div><dt>Status</dt><dd>{employment.is_current ? 'Current' : 'Historical'}; {employment.is_primary ? 'primary' : 'not primary'}</dd></div>
  <div><dt>Address</dt><dd>{employment.address_id === undefined ? 'None' : `Address ID ${employment.address_id}`}</dd></div>
  <div><dt>Confidence</dt><dd>{employment.confidence === undefined ? 'None' : employment.confidence}</dd></div>
  <div><dt>Source</dt><dd>{employment.source}{#if employment.source_ref} · {employment.source_ref}{/if}</dd></div>
</dl>

<style>
  dl, dd { margin: 0; }
  dl { display: grid; gap: var(--space-1); }
  div { display: grid; grid-template-columns: 7rem 1fr; gap: var(--space-2); }
  dt { font-weight: 600; }
</style>
