<script lang="ts">
  import { SegmentedControl } from '@kenn-io/kit-ui';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { NetworkEdge, NetworkNode } from '../../directory/models';
  import { joinWords } from '../../util/words';
  import InlineNotice from '../common/InlineNotice.svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import DetailSection from '../people/DetailSection.svelte';

  interface Props {
    controller: DirectoryEntityController;
    onOpenPerson: (personID: number) => void;
    onOpenOrganization: (organizationID: number) => void;
  }

  let { controller, onOpenPerson, onOpenOrganization }: Props = $props();
  // Direct links are this person's jobs and connections, which Work and Connections already list,
  // so Direct opens as a one-line summary naming them; 2 and 3 steps list the people reached through them, and links among them.
  let depth = $state('1');
  const depthOptions = [
    { value: '1', label: 'Direct' },
    { value: '2', label: '2 steps' },
    { value: '3', label: '3 steps' }
  ];
  const nodes = $derived(controller.network?.nodes ?? []);
  const edges = $derived(controller.network?.edges ?? []);
  const nodeByID = $derived(new Map(nodes.map((node) => [node.id, node])));
  // Name up to three direct records, then retain the count of the rest.
  const direct = $derived(nodes.filter(node => node.hop === 1));
  const reached = $derived(direct.length <= 3 ? direct : [...direct.slice(0, 2), { id: 'more', label: `${direct.length - 2} more` }]);

  $effect(() => {
    void controller.loadNetwork(Number(depth));
  });

  /** The connection label or job title, using the catalog's direction when available. */
  function relation(edge: NetworkEdge, node: NetworkNode): { label: string | undefined; namesSource: boolean } {
    if (edge.kind === 'employment') return { label: edge.label || undefined, namesSource: false };
    const type = controller.relationshipTypes.find(({ slug }) => slug === edge.relationship_type_slug);
    // A relationship's target reads its type's reverse label from the source's side, and the source its forward label.
    if (type) return { label: edge.target_node_id === node.id ? type.reverse_label : type.forward_label, namesSource: false };
    if (!edge.label || edge.source_node_id === node.id) return { label: edge.label || undefined, namesSource: false };
    // The row names the target; name the source to keep the forward label's direction clear.
    return { label: `From ${nodeByID.get(edge.source_node_id)!.label}: ${edge.label}`, namesSource: true };
  }

  // Each link is said once, on its farther end. Between two at the same step it's said on the side it describes:
  // a job on its person, a connection on its source.
  function side(edge: NetworkEdge): string | undefined {
    const source = nodeByID.get(edge.source_node_id), target = nodeByID.get(edge.target_node_id);
    if (!source || !target) return undefined;
    if (source.hop !== target.hop) return source.hop > target.hop ? source.id : target.id;
    return edge.kind === 'employment' && source.kind !== 'person' ? target.id : source.id;
  }

  // Everyone past the direct links, and links among contacts at the same step.
  const further = $derived.by(() => {
    const rows: { node: NetworkNode; line: string }[] = [];
    for (const node of nodes) {
      if (node.hop === 0) continue;
      // Group by relation, so each title stays with its employer.
      const paths = new Map<string, { label: string | undefined; via: string[]; with: string[] }>();
      for (const edge of edges.filter((edge) => side(edge) === node.id)) {
        const other = nodeByID.get(edge.source_node_id === node.id ? edge.target_node_id : edge.source_node_id)!;
        if (node.hop === 1 && other.hop === 0) continue;
        const { label, namesSource } = relation(edge, node), key = label ?? '';
        const path = paths.get(key) ?? { label, via: [], with: [] };
        if (!namesSource) (other.hop < node.hop ? path.via : path.with).push(other.label);
        paths.set(key, path);
      }
      if (!paths.size) continue;
      const line = [...paths.values()].map((path) => [
        path.label,
        path.via.length ? `via ${joinWords([...new Set(path.via)])}` : undefined,
        path.with.length ? `with ${joinWords([...new Set(path.with)])}` : undefined
      ].filter(Boolean).join(' · ')).join('; ');
      rows.push({ node, line: line[0]!.toUpperCase() + line.slice(1) });
    }
    return rows.sort((left, right) => left.node.hop - right.node.hop || left.node.label.localeCompare(right.node.label));
  });

  function retry(): void {
    void controller.loadNetwork(Number(depth));
  }
</script>

{#if edges.length > 0 || depth !== '1' || controller.errors.network}
  <DetailSection title="Network">
    {#snippet control()}<SegmentedControl ariaLabel="Network depth" value={depth} options={depthOptions} onchange={(value) => { depth = value; }} />{/snippet}
    {#if controller.errors.network}
      <InlineNotice message="Couldn't load the network." details={controller.errors.network} actionLabel="Retry" onaction={retry} />
    {/if}
    {#if depth === '1'}
      <!-- Direct links only: name them in one line rather than repeat Work and Connections. -->
      <p class="summary">{#if controller.networkLoading}<span role="status">Loading network…</span>{:else}{#each reached as node, index (node.id)}{#if index > 0}{index === reached.length - 1 ? (reached.length === 2 ? ' and ' : ', and ') : ', '}{/if}{#if 'kind' in node}<!-- kit-ui-check-ignore: a record name styled as a link, matching Connections and Work. --><button type="button" class="link" onclick={() => node.kind === 'person' ? onOpenPerson(node.entity_id) : onOpenOrganization(node.entity_id)}>{node.label}</button>{:else}{node.label}{/if}{/each}{/if}</p>
    {:else}
      <div class="further" role="region" aria-label={`Network at ${depth} steps`} aria-busy={controller.networkLoading}>
        {#each further as { node, line } (node.id)}
          <DetailRow description={line}>
            <!-- kit-ui-check-ignore: a record name styled as a link, matching Connections and Work. -->
            <button type="button" class="link" onclick={() => node.kind === 'person' ? onOpenPerson(node.entity_id) : onOpenOrganization(node.entity_id)}>{node.label}</button>
          </DetailRow>
        {:else}
          {#if controller.networkLoading}<p class="summary" role="status">Loading network…</p>
          {:else if !controller.errors.network}<p class="summary">No one else at {depth} steps</p>{/if}
        {/each}
      </div>
    {/if}
    {#if controller.network?.truncated}<p class="summary truncated">Showing up to 250 people and organizations and 500 connections.</p>{/if}
  </DetailSection>
{/if}

<style>
  /* L2 through DetailSection: the summary (Meta) sits 8 below the header, and the 2 and 3 steps lists are L3 rows.
   * A notice sits 16 from what follows it. */
  p { margin: 0; }
  .further { display: grid; min-width: 0; }
  .further > :global(.detail-row:last-child) { box-shadow: none; }
  .further[aria-busy='true'] { opacity: 0.6; }
  :global(.inline-notice) + .summary, :global(.inline-notice) + .further { margin-top: var(--space-pane); }
  .truncated { margin-top: var(--space-group); }
  /* kit-ui-check-ignore: a text link, matching record links in Connections and Work. */
  .link { padding: 0; border: 0; background: none; color: var(--link-ink); cursor: pointer; font: inherit; text-align: left; }
  .link:hover { text-decoration: underline; }
  .summary { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
</style>
