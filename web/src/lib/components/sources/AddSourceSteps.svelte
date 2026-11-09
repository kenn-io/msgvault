<script lang="ts">
  import { Checkbox, SegmentedControl, TextInput } from '@kenn-io/kit-ui';
  import { addSourceCommand, imapServer, type CommandSourceKind } from '../../sources/add-source';
  import CommandLine from './CommandLine.svelte';

  let { kind }: { kind: CommandSourceKind } = $props();
  let values = $state<Record<string, string>>({});
  let starttls = $state(false);
  // The visitor's own system first; the switch shows only when the two quote the command differently.
  let shell = $state<'posix' | 'cmd'>(typeof navigator !== 'undefined' && /Win/i.test(navigator.platform) ? 'cmd' : 'posix');
  const posix = $derived(addSourceCommand(kind, values, 'posix', starttls));
  const cmd = $derived(addSourceCommand(kind, values, 'cmd', starttls));
</script>

<div class="steps">
  {#each kind.fields as field, index (field.id)}
    <label>{field.label}<TextInput block bind:value={() => values[field.id] ?? '', value => (values[field.id] = value)} placeholder={field.placeholder} autofocus={index === 0} /></label>
  {/each}
  {#if kind.id === 'imap'}
    {#if values.host?.trim() && !imapServer(values.host)}<p>Enter a port from 1 to 65535.</p>{/if}
    <Checkbox label="Use STARTTLS" bind:checked={starttls} />
    <p>The command asks for your password.</p>
  {/if}
  {#if posix && cmd}
    <div class="terminal">
      {#if posix !== cmd}<SegmentedControl ariaLabel="Platform" value={shell} options={[{ value: 'posix', label: 'macOS or Linux' }, { value: 'cmd', label: 'Windows' }]} onchange={(value) => shell = value as typeof shell} />{/if}
      {#if shell === 'cmd' && posix !== cmd}<p>Start Command Prompt with <code>cmd /d /v:off</code>, then paste:</p>{/if}
      <CommandLine command={shell === 'cmd' ? cmd : posix} />
    </div>
    <ol>
      <li>Run this command in a terminal where msgvault is installed.</li>
      <li>Run the first-sync command it prints.</li>
    </ol>
  {/if}
  <a href={kind.docs} target="_blank" rel="noreferrer">Setup guide</a>
</div>

<style>
  /* Steps 16 apart. A field's label (Meta, 600) is 4 above it; the platform switch is 8 above its command. */
  .steps { display: grid; gap: var(--space-pane); min-width: 0; }
  label { display: grid; gap: var(--space-tight); color: var(--text-primary); font-size: var(--type-meta-size); font-weight: 600; line-height: var(--type-meta-line); }
  .terminal { display: grid; justify-items: start; gap: var(--space-group); min-width: 0; }
  p, ol { margin: 0; color: var(--text-secondary); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  ol { padding-left: var(--space-pane); }
  a:hover { text-decoration: underline; }
  a { text-decoration: none; justify-self: start; color: var(--link-ink); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
</style>
