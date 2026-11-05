<script lang="ts">
  import { Button, SegmentedControl } from '@kenn-io/kit-ui';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { NetworkEdge, NetworkNode } from '../../directory/models';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';

  interface Props {
    controller: DirectoryEntityController;
    onOpenPerson: (personID: number) => void;
    onOpenOrganization: (organizationID: number) => void;
  }

  let { controller, onOpenPerson, onOpenOrganization }: Props = $props();
  const COLUMN = 280;
  const NAME = 170;
  const ROW = 56;
  const AVATAR = 28;
  let depth = $state('1');
  const depthOptions = [
    { value: '1', label: 'Direct' },
    { value: '2', label: '2 steps' },
    { value: '3', label: '3 steps' }
  ];
  const nodes = $derived(controller.network?.nodes ?? []);
  const edges = $derived(controller.network?.edges ?? []);
  const nodeByID = $derived(new Map(nodes.map((node) => [node.id, node])));
  const sameColumnEdges = $derived(edges.filter((edge) => nodeByID.get(edge.source_node_id)?.hop === nodeByID.get(edge.target_node_id)?.hop));

  $effect(() => {
    void controller.loadNetwork(Number(depth), controller.relationshipsIncludeEnded);
  });

  // One column per step away, each ordered by the people it hangs off so lines cross as little as possible.
  const layout = $derived.by(() => {
    const neighbours = new Map<string, string[]>();
    for (const edge of edges) {
      neighbours.set(edge.source_node_id, [...(neighbours.get(edge.source_node_id) ?? []), edge.target_node_id]);
      neighbours.set(edge.target_node_id, [...(neighbours.get(edge.target_node_id) ?? []), edge.source_node_id]);
    }
    const columns: NetworkNode[][] = [];
    for (const node of nodes) (columns[node.hop] ??= []).push(node);
    const columnRows = Array.from(columns, (column) => column?.length ?? 0);
    const tallest = Math.max(1, ...columnRows);
    const row = new Map<string, number>();
    columns.forEach((column, hop) => {
      if (hop > 0) {
        const anchor = (node: NetworkNode) => {
          const parents = (neighbours.get(node.id) ?? []).map((id) => row.get(id)).filter((value) => value !== undefined);
          return parents.length ? parents.reduce((sum, value) => sum + value, 0) / parents.length : Infinity;
        };
        column.sort((left, right) => anchor(left) - anchor(right) || left.label.localeCompare(right.label));
      }
      const offset = (tallest - columnRows[hop]!) / 2;
      column.forEach((node, index) => row.set(node.id, offset + index));
    });
    const place = new Map<string, { x: number; y: number }>();
    for (const node of nodes) place.set(node.id, { x: node.hop * COLUMN + AVATAR / 2, y: (row.get(node.id) ?? 0) * ROW + AVATAR / 2 });
    return { nodes, edges, place, width: columns.length * COLUMN, height: (tallest - 1) * ROW + AVATAR };
  });

  // Each node's drawn width, so a line starts where its name ends.
  let widths = $state<Record<string, number>>({});

  // A line runs from the end of the left node's name to the right node's avatar.
  function segment(edge: NetworkEdge) {
    const a = layout.place.get(edge.source_node_id);
    const b = layout.place.get(edge.target_node_id);
    if (!a || !b) return null;
    const [left, right, leftID] = a.x <= b.x ? [a, b, edge.source_node_id] : [b, a, edge.target_node_id];
    if (left.x === right.x) return null;
    const x1 = left.x - AVATAR / 2 + (widths[leftID] ?? AVATAR) + 8;
    const x2 = right.x - AVATAR / 2 - 6;
    return { x1, y1: left.y, x2, y2: right.y, tx: (x1 + x2) / 2, ty: (left.y + right.y) / 2, room: x2 - x1 - 8 };
  }

  const labelGroups = $derived.by(() => {
    const grouped = new Map<string, { edge: NetworkEdge; labels: string[] }>();
    for (const edge of edges) {
      const key = JSON.stringify([edge.source_node_id, edge.target_node_id].sort());
      const group = grouped.get(key) ?? { edge, labels: [] };
      group.labels.push(edge.label);
      grouped.set(key, group);
    }
    return [...grouped.values()];
  });

  function retry(): void {
    void controller.loadNetwork(Number(depth), controller.relationshipsIncludeEnded);
  }
</script>

{#if edges.length > 0 || depth !== '1' || controller.errors.network}
  <section class="person-network" aria-label="Directory network">
    <header>
      <h3>Network</h3>
      <SegmentedControl ariaLabel="Network depth" value={depth} options={depthOptions} onchange={(value) => { depth = value; }} />
    </header>
    {#if controller.errors.network}
      <div class="refresh-error" role="alert">
        <p>{controller.errors.network}</p>
        <Button label="Retry network" size="sm" onclick={retry} />
      </div>
    {/if}
    <!-- Keyboard focus lets a user scroll a graph wider than its pane without a pointer. -->
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div class="scroll" tabindex="0" role="region" aria-label="Network graph" aria-busy={controller.networkLoading}>
      <div class="graph" style:width={`${layout.width}px`} style:height={`${layout.height}px`}>
        <svg width={layout.width} height={layout.height} aria-hidden="true">
          {#each edges as edge (edge.id)}
            {@const line = segment(edge)}
            {#if line}<line x1={line.x1} y1={line.y1} x2={line.x2} y2={line.y2} />{/if}
          {/each}
        </svg>
        {#each labelGroups as group (group.edge.id)}
          {@const line = segment(group.edge)}
          {#if line}<span class="edge-label" aria-hidden="true" style:left={`${line.tx}px`} style:top={`${line.ty}px`} style:max-width={`${line.room}px`} title={group.labels.join(' · ')}>{group.labels.join(' · ')}</span>{/if}
        {/each}
        {#each nodes as node (node.id)}
          {@const at = layout.place.get(node.id)}
          {#if at}
            <div class="node" class:node--root={node.hop === 0} style:left={`${at.x - AVATAR / 2}px`} style:top={`${at.y - AVATAR / 2}px`} style:max-width={`${NAME + AVATAR}px`} title={node.label} bind:clientWidth={widths[node.id]}>
              <IdentityAvatar label={node.label} seed={node.id} shape={node.kind === 'person' ? 'person' : 'domain'} size={AVATAR} />
              {#if node.hop === 0}<span class="name">{node.label}</span>
              {:else}
                <!-- kit-ui-check-ignore: a record name styled as a link, matching Connections and Work. -->
                <button type="button" class="name link" onclick={() => node.kind === 'person' ? onOpenPerson(node.entity_id) : onOpenOrganization(node.entity_id)}>{node.label}</button>
              {/if}
            </div>
          {/if}
        {/each}
      </div>
      {#if controller.networkLoading}<p class="loading" role="status">Loading network…</p>{/if}
    </div>
    {#each sameColumnEdges as edge (edge.id)}
      <p class="same-column" aria-hidden="true">{nodeByID.get(edge.source_node_id)?.label} · {edge.label} · {nodeByID.get(edge.target_node_id)?.label}</p>
    {/each}
    {#if controller.network?.truncated}<p class="truncated">Showing up to 250 people and organizations and 500 connections.</p>{/if}
    <ul class="kit-sr-only" aria-label="Network connections">
      {#each edges as edge (edge.id)}
        <li>{nodeByID.get(edge.source_node_id)?.label} · {edge.label} · {nodeByID.get(edge.target_node_id)?.label}</li>
      {/each}
    </ul>
  </section>
{/if}

<style>
  .person-network { display: grid; gap: var(--space-2); }
  header { display: flex; align-items: center; justify-content: space-between; min-height: var(--control-h); }
  h3, p { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  .scroll { position: relative; overflow-x: auto; padding-top: var(--space-2); }
  .scroll:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  .graph { position: relative; }
  svg { position: absolute; inset: 0; overflow: visible; }
  line { stroke: var(--border-strong); stroke-width: 1; }
  .node { position: absolute; display: flex; align-items: center; gap: var(--space-2); }
  .name { max-width: 170px; overflow: hidden; font-size: var(--font-size-sm); text-overflow: ellipsis; white-space: nowrap; }
  .node--root .name { font-weight: var(--font-weight-semibold, 600); }
  /* kit-ui-check-ignore: a text link inside the graph, matching record links in Connections and Work. */
  .link { padding: 0; border: 0; background: none; color: var(--link-ink); cursor: pointer; font: inherit; font-size: var(--font-size-sm); }
  .link:hover { text-decoration: underline; }
  .edge-label { position: absolute; transform: translate(-50%, -50%); box-sizing: border-box; overflow: hidden; text-overflow: ellipsis; padding: 0 var(--chip-pad-x); border: 1px solid var(--border-strong); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-muted); font-size: var(--font-size-xs); line-height: 18px; white-space: nowrap; }
  .loading { position: absolute; inset: 0; display: grid; place-items: center; background: color-mix(in srgb, var(--bg-surface) 72%, transparent); font-size: var(--font-size-sm); color: var(--text-muted); }
  .refresh-error { display: flex; align-items: center; gap: var(--space-2); color: var(--text-danger); font-size: var(--font-size-sm); }
  .truncated, .same-column { color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
