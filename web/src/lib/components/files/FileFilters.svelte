<script lang="ts">
  import { Checkbox, SearchInput } from '@kenn-io/kit-ui';
  import { FILE_FAMILY_LABELS } from '../../explore/labels';
  import type { FileMIMEFamily } from '../../explore/models';

  let { filenameQuery, mimeFamilies, onFilenameQueryChange, onMIMEFamiliesChange, available = Object.keys(FILE_FAMILY_LABELS) as FileMIMEFamily[], keepOne = false }: {
    available?: FileMIMEFamily[];
    keepOne?: boolean;
    filenameQuery: string;
    mimeFamilies: FileMIMEFamily[];
    onFilenameQueryChange: (query: string) => void;
    onMIMEFamiliesChange: (families: FileMIMEFamily[]) => void;
  } = $props();
</script>

<div class="filters">
  <div class="filename"><SearchInput value={filenameQuery} ariaLabel="Filter filename" placeholder="Filename" oninput={onFilenameQueryChange} /></div>
  <fieldset class="field">
    <legend>Type</legend>
    <div class="types">
      {#each available.map(family => [family, FILE_FAMILY_LABELS[family]]) as [family, label] (family)}
        <Checkbox
          {label}
          disabled={keepOne && mimeFamilies.includes(family as FileMIMEFamily) && mimeFamilies.length === 1}
          checked={mimeFamilies.includes(family as FileMIMEFamily)}
          onchange={(checked) => onMIMEFamiliesChange(checked ? [...mimeFamilies, family as FileMIMEFamily] : mimeFamilies.filter((value) => value !== family))}
        />
      {/each}
    </div>
  </fieldset>
</div>

<style>
  .filters { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-group); }
  .filename { width: 280px; max-width: 100%; }
  .field { display: grid; min-width: 0; gap: var(--label-gap); margin: 0; padding: 0; border: 0; }
  legend { padding: 0; color: var(--text-muted); font-size: var(--font-size-xs); }
  .types { display: flex; flex-wrap: wrap; gap: var(--control-gap) var(--section-gap); }
</style>
