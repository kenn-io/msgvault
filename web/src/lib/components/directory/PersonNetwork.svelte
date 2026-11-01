<script lang="ts">
  import { Button, Checkbox, SegmentedControl } from '@kenn-io/kit-ui';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { NetworkEdge, NetworkNode, PersonNetwork } from '../../api/generated/models';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';

  interface Props {
    controller: DirectoryEntityController;
    onOpenOrganization: (id: number) => void;
    /** Opens a person in the graph. */
    onOpenPerson: (personID: number) => void;
  }

  let { controller, onOpenOrganization, onOpenPerson }: Props = $props();

  const COLUMN = 280;
  const NAME = 170;
  const ROW = 56;
  const AVATAR = 28;
  const depthOptions = [
    { value: '1', label: 'Direct' },
    { value: '2', label: '2 steps' },
    { value: '3', label: '3 steps' },
  ];

  let depth = $state('1');
  let includeEnded = $state(false);
  const personID = $derived(controller.personID);
  const network = $derived(controller.network);

  $effect(() => { void controller.loadNetwork(Number(depth), includeEnded); });

  // One column per step away, each ordered by the people it hangs off so lines cross as little as possible.
  const layout = $derived.by(() => {
    const nodes = network?.nodes ?? [];
    const edges = network?.edges ?? [];
    const neighbours = new Map<string, string[]>();
    for (const edge of edges) {
      neighbours.set(edge.source_node_id, [...(neighbours.get(edge.source_node_id) ?? []), edge.target_node_id]);
      neighbours.set(edge.target_node_id, [...(neighbours.get(edge.target_node_id) ?? []), edge.source_node_id]);
    }
    const columns: NetworkNode[][] = [];
    for (const node of nodes) (columns[node.hop] ??= []).push(node);
    const row = new Map<string, number>();
    columns.forEach((column, hop) => {
      if (hop > 0) {
        const anchor = (node: NetworkNode) => {
          const parents = (neighbours.get(node.id) ?? []).map((id) => row.get(id)).filter((value) => value !== undefined);
          return parents.length ? parents.reduce((sum, value) => sum + value, 0) / parents.length : Infinity;
        };
        column.sort((left, right) => anchor(left) - anchor(right) || left.label.localeCompare(right.label));
      }
      const tallest = Math.max(...columns.map((each) => each?.length ?? 0));
      const offset = (tallest - column.length) / 2;
      column.forEach((node, index) => row.set(node.id, offset + index));
    });
    const tallest = Math.max(1, ...columns.map((column) => column?.length ?? 0));
    const place = new Map<string, { x: number; y: number }>();
    for (const node of nodes) place.set(node.id, { x: node.hop * COLUMN + AVATAR / 2, y: (row.get(node.id) ?? 0) * ROW + ROW / 2 });
    return { nodes, edges, place, width: columns.length * COLUMN, height: tallest * ROW };
  });

  // Each node's drawn width, so a line starts where its name ends.
  let widths = $state<Record<string, number>>({});

  // A line runs from the end of the left node's name to the right node's avatar, with the label in the connection list.
  function segment(edge: NetworkEdge) {
    const a = layout.place.get(edge.source_node_id);
    const b = layout.place.get(edge.target_node_id);
    if (!a || !b) return null;
    const [left, right, leftID] = a.x <= b.x ? [a, b, edge.source_node_id] : [b, a, edge.target_node_id];
    if (left.x === right.x) {
      const x = left.x - AVATAR / 2 - 8;
      return { x1: x, y1: left.y, x2: x, y2: right.y };
    }
    const x1 = left.x - AVATAR / 2 + (widths[leftID] ?? AVATAR) + 8;
    const x2 = right.x - AVATAR / 2 - 6;
    return { x1, y1: left.y, x2, y2: right.y };
  }

  const hasLinks = $derived((network?.edges.length ?? 0) > 0);

  // How a node relates to nodes no farther away. A relationship names both ends in reading order,
  // such as "Ann mentors" under Bob or "mentors Ann" under Ann's mentor; a job shows its title.
  function relation(node: NetworkNode): string {
    const find = (id: string) => network?.nodes.find((each) => each.id === id);
    const hop = (id: string) => find(id)?.hop ?? 0;
    return (network?.edges ?? [])
      .filter((edge) => (edge.target_node_id === node.id && hop(edge.source_node_id) <= node.hop) || (edge.source_node_id === node.id && hop(edge.target_node_id) < node.hop))
      .map((edge) => edge.kind !== 'relationship' ? edge.label : edge.target_node_id === node.id ? `${find(edge.source_node_id)?.label} ${edge.label}` : `${edge.label} ${find(edge.target_node_id)?.label}`)
      .filter((label, index, all) => label && all.indexOf(label) === index)
      .join(', ');
  }

</script>

  <section aria-labelledby={`network-${personID}`}>
    <header>
      <h3 id={`network-${personID}`}>Network</h3>
      <span class="controls">
        <Checkbox checked={includeEnded} label="Show ended" ariaLabel="Include ended connections" onchange={(checked) => (includeEnded = checked)} />
        <SegmentedControl ariaLabel="Network depth" value={depth} options={depthOptions} onchange={(value) => (depth = value)} />
      </span>
    </header>
    {#if controller.errors.network}<p role="alert">{controller.errors.network}<Button label="Retry network" onclick={() => void controller.loadNetwork(Number(depth), includeEnded)} /></p>{/if}
    {#if controller.networkLoading}<p role="status">Loading network…</p>{/if}
    {#if network?.truncated}<p role="status">Showing a bounded prefix of at most 250 nodes and 500 connections at depth {depth}.</p>{/if}
    {#if network && !hasLinks}<p class="muted">No connections at this depth</p>{/if}
    {#if network && hasLinks}
      <!-- The drawn lines are decorative; this list names every link for screen readers. -->
      <ul class="kit-sr-only" aria-label="Network connections">
        {#each network.edges as edge (edge.id)}
          <li>{network.nodes.find((node) => node.id === edge.source_node_id)?.label} {edge.label} {network.nodes.find((node) => node.id === edge.target_node_id)?.label}</li>
        {/each}
      </ul>
    {/if}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    {#if hasLinks}<div class="scroll" tabindex="0" role="region" aria-label="Network graph">
      <div class="graph" style:width={`${layout.width}px`} style:height={`${layout.height}px`}>
        <svg width={layout.width} height={layout.height} aria-hidden="true">
          {#each layout.edges as edge (edge.id)}
            {@const line = segment(edge)}
            {#if line}
              <line class:ended={Boolean(edge.end_date)} x1={line.x1} y1={line.y1} x2={line.x2} y2={line.y2} />
            {/if}
          {/each}
        </svg>
        {#each layout.nodes as node (node.id)}
          {@const at = layout.place.get(node.id)}
          {#if at}
            {@const person = node.kind === 'person'}
            {@const root = node.hop === 0}
            <div class="node" class:node--root={root} style:left={`${at.x - AVATAR / 2}px`} style:top={`${at.y}px`} style:max-width={`${NAME + AVATAR}px`} title={node.label} bind:clientWidth={widths[node.id]}>
              <IdentityAvatar label={node.label} seed={node.id} shape={person ? 'person' : 'domain'} size={AVATAR} />
              {#if !root}
                <span class="text">
                  <!-- kit-ui-check-ignore: a name in the graph that opens the person, styled as a link. -->
                  <button type="button" class="name link" aria-label={`Open ${node.kind} ${node.label}`} onclick={() => person ? onOpenPerson(node.entity_id) : onOpenOrganization(node.entity_id)}>{node.label}</button>
                  {#if relation(node)}<span class="relation">{relation(node)}</span>{/if}
                </span>
              {:else}
                <span class="name">{node.label}</span>
              {/if}
            </div>
          {/if}
        {/each}
      </div>
    </div>{/if}
  </section>

<style>
  section {
    display: flex;
    flex-direction: column;
  }

  header {
    flex-wrap: wrap;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--control-gap);
    min-height: var(--control-h);
    padding-bottom: var(--space-2);
  }

  .controls {
    display: flex;
    align-items: center;
    gap: var(--control-gap);
  }

  .muted { margin: 0; color: var(--text-muted); font-size: var(--font-size-sm); }

  .text {
    display: grid;
    min-width: 0;
    justify-items: start;
  }

  .relation {
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Ended connections, shown only when asked for, read as muted like in Connections. */
  line.ended {
    stroke-dasharray: 3 3;
  }

  h3 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold, 600);
  }

  .scroll {
    overflow-x: auto;
    padding: var(--space-2) 0;
  }

  .graph {
    position: relative;
  }

  svg {
    position: absolute;
    inset: 0;
    overflow: visible;
  }

  line {
    stroke: var(--border-strong);
    stroke-width: 1;
  }

  .node {
    position: absolute;
    transform: translateY(-50%);
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .name {
    overflow: hidden;
    font-size: var(--font-size-sm);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .node--root .name {
    font-weight: var(--font-weight-semibold, 600);
  }

  /* kit-ui-check-ignore: a text link inside the graph, not a button control. */
  .link {
    padding: 0;
    border: 0;
    background: none;
    color: var(--link-ink);
    cursor: pointer;
    font: inherit;
    font-size: var(--font-size-sm);
  }

  .link:hover {
    text-decoration: underline;
  }
</style>
