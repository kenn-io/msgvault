<script lang="ts">
  import { Disclosure } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import EmptyLine from '../common/EmptyLine.svelte';
  import { listAttributeDefinitions, listPersonFactTargets } from '../../api/generated/api/api';
  import type { APIClient } from '../../api/client';
  import { sentenceCase } from '../../explore/labels';
  import type { TargetDescriptor as GeneratedTarget } from '../../api/generated/models';
  type PersonTrackingTarget = Pick<GeneratedTarget, 'key' | 'kind' | 'slug' | 'description' | 'sensitive'>;

  let { client }: { client: APIClient } = $props();
  let targets = $state<PersonTrackingTarget[]>([]);
  let catalogLoading = $state(false);
  let catalogError = $state<string | null>(null);
  let catalogIncludesSensitive = $state(false);
  let catalogGeneration = 0;
  let catalogAbort: AbortController | undefined;
  let disposed = false;
  let labels = $state(new Map<string, string>());
  let sensitiveOpen = $state(false);
  let namesError = $state<string | null>(null);
  const namesAbort = new AbortController();
  // Without the field names, a field reads as its description; the fields already listed stay while Retry asks again.
  function loadNames(): void {
    namesError = null;
    void listAttributeDefinitions({ object_type: 'person' }, { ...client, signal: namesAbort.signal })
      .then(({ data, response }) => { if (namesAbort.signal.aborted) return; if (data) labels = new Map((data.definitions ?? []).map(definition => [definition.slug, definition.label])); else namesError = `The server answered ${response.status}.`; })
      .catch((cause: unknown) => { if (!namesAbort.signal.aborted) namesError = cause instanceof Error && cause.message ? cause.message : "Couldn't reach msgvault."; });
  }
  onMount(() => { void loadCatalog(false); loadNames(); });
  onDestroy(() => { namesAbort.abort(); disposed = true; catalogAbort?.abort(); });

  async function loadCatalog(includeSensitive: boolean): Promise<boolean> {
    const request = ++catalogGeneration;
    catalogAbort?.abort();
    const abort = new AbortController();
    catalogAbort = abort;
    catalogLoading = true;
    catalogError = null;
    try {
      const { data, response } = await listPersonFactTargets(
        { include_sensitive: includeSensitive },
        {
          ...client,
          signal: abort.signal,
        },
      );
      if (!currentCatalog(request, abort.signal)) return false;
      const loaded = data ? safeTargets(data.targets) : undefined;
      if (!loaded) {
        catalogError = data ? "The server's list of fields couldn't be read." : `The server answered ${response.status}.`;
        return false;
      }
      targets = loaded;
      catalogIncludesSensitive = includeSensitive;
      return true;
    } catch (cause: unknown) {
      if (!currentCatalog(request, abort.signal)) return false;
      // Details holds what went wrong as it was reported; the notice says what didn't load.
      catalogError = cause instanceof Error && cause.message ? cause.message : "Couldn't reach msgvault.";
      return false;
    } finally {
      if (currentCatalog(request)) {
        if (catalogAbort === abort) catalogAbort = undefined;
        catalogLoading = false;
      }
    }
  }
  function currentCatalog(request: number, signal: AbortSignal | undefined = undefined): boolean {
    return !disposed && !signal?.aborted && catalogGeneration === request;
  }
  function safeTargets(values: GeneratedTarget[] | null): PersonTrackingTarget[] | undefined {
    if (values === null) return [];
    const targets: PersonTrackingTarget[] = [];
    for (const value of values) {
      if (
        typeof value.key !== 'string' ||
        typeof value.kind !== 'string' ||
        typeof value.slug !== 'string' ||
        typeof value.description !== 'string' ||
        typeof value.sensitive !== 'boolean'
      )
        return undefined;
      targets.push({
        key: value.key,
        kind: value.kind,
        slug: value.slug,
        description: value.description,
        sensitive: value.sensitive,
      });
    }
    return targets;
  }

  function label(target: PersonTrackingTarget): string {
    return target.kind === 'employment' ? 'Work · job title, organization, and dates' : labels.get(target.slug) ?? (target.description || sentenceCase(target.slug));
  }
  const sensitive = $derived(targets.filter(target => target.sensitive));
</script>

<!-- The fields Keep this profile updated may change on a person: a heading 8 above 36 rows, then the fold. -->
<section class="tracked-fields" aria-labelledby="tracked-fields-heading">
  <h3 id="tracked-fields-heading">Fields kept updated</h3>
  {#if catalogLoading && !targets.length}<p role="status">Loading fields…</p>{/if}
  <ul>{#each targets.filter(target => !target.sensitive) as target (target.key)}<li>{label(target)}</li>{/each}</ul>
  {#if !catalogLoading && !catalogError && !targets.length}<EmptyLine message="No fields yet" />{/if}
  {#if catalogError && !sensitiveOpen}<InlineNotice message="Couldn't load the fields kept updated." actionLabel="Retry" onaction={() => void loadCatalog(sensitiveOpen)} details={catalogError} />{/if}
  {#if namesError}<InlineNotice message="Couldn't load the field names." actionLabel="Retry" onaction={loadNames} details={namesError} />{/if}
  <Disclosure variant="plain" class="fold" title="Show sensitive fields" bind:open={sensitiveOpen} ontoggle={(open) => { if (open && !catalogIncludesSensitive) void loadCatalog(true); }}>
    {#if catalogError && sensitiveOpen}<InlineNotice message="Couldn't load sensitive fields." actionLabel="Retry" onaction={() => void loadCatalog(true)} details={catalogError} />{/if}
    {#if catalogIncludesSensitive && !sensitive.length}<EmptyLine message="No sensitive fields" />{/if}
    <ul>{#each sensitive as target (target.key)}<li>{label(target)}</li>{/each}</ul>
  </Disclosure>
</section>

<style>
  .tracked-fields { display: grid; min-width: 0; }
  h3, p, ul { margin: 0; }
  h3 { margin-bottom: var(--space-group); font-size: var(--type-heading-size); line-height: var(--type-heading-line); }
  p { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  ul { padding: 0; list-style: none; }
  li { padding: var(--row-pad-y) var(--row-pad-x); box-shadow: inset 0 -1px 0 var(--border-muted); color: var(--text-primary); font-size: var(--type-body-size); line-height: var(--type-body-line); }
</style>
