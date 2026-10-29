<script lang="ts">
  import type { SecretUpdate } from '../../settings/secrets';
  import { Chip, Toggle } from '@kenn-io/kit-ui';
  import type { PersonEnrichmentProviderSetting as ProviderSetting } from '../../api/generated/models';
  import ProviderCredentialControl from './ProviderCredentialControl.svelte';
  let { provider, draft, pendingKey, onchange, onkey }: {
    provider: ProviderSetting; draft: ProviderSetting; pendingKey?: SecretUpdate;
    onchange: (value: ProviderSetting) => void; onkey: (update: SecretUpdate | undefined) => void;
  } = $props();
  function setString(field: keyof ProviderSetting, value: string) {
    onchange({ ...draft, [field]: value });
  }
  function setNumber(field: keyof ProviderSetting, value: string) {
    onchange({ ...draft, [field]: value === '' ? '' : Number(value) } as ProviderSetting);
  }
  function setList(field: 'allowed_identifiers' | 'target_keys', value: string) {
    onchange({
      ...draft,
      [field]: value
        .split(',')
        .map((item) => item.trim())
        .filter(Boolean),
    });
  }
</script>
<article class="provider-card" aria-labelledby={`provider-${provider.name}`}>
  <header>
    <h4 id={`provider-${provider.name}`}>{provider.name}</h4>
    <Chip size="xs" tone="muted" uppercase={false}>{provider.kind === 'exa' ? 'Exa' : 'SixtyFour'}</Chip>
    <span class="grow"></span>
    <Toggle
      ariaLabel={`Enable ${provider.name}`}
      checked={draft.enabled}
      onchange={(enabled) => {
        onchange({ ...draft, enabled });
      }}
    />
  </header>
  <div class="field" class:setting--changed={pendingKey !== undefined} aria-label={`${provider.name} credential`}>
    <span>API key</span>
    <ProviderCredentialControl label={`API key for ${provider.name}`} credentialState={provider.credential} pending={pendingKey} onchange={onkey} />
  </div>
  <details class="advanced">
    <summary>Advanced</summary>
    <div class="provider-grid">
      <label class="wide">
        Endpoint
        <input
          type="url"
          required
          aria-label={`${provider.name} endpoint`}
          value={draft.endpoint}
          oninput={(event) => setString('endpoint', event.currentTarget.value)}
        />
      </label>
      {#if provider.kind === 'sixtyfour'}
        <label class="wide">
          Poll endpoint
          <input
            type="url"
            required
            aria-label={`${provider.name} poll endpoint`}
            value={draft.poll_endpoint ?? ''}
            oninput={(event) => setString('poll_endpoint', event.currentTarget.value)}
          />
        </label>
        <label>
          Tier
          <input value={draft.tier ?? ''} oninput={(event) => setString('tier', event.currentTarget.value)} />
        </label>
      {:else}
        <label>
          Search mode
          <input value={draft.mode ?? ''} oninput={(event) => setString('mode', event.currentTarget.value)} />
        </label>
        <label>
          Results per request
          <input
            type="number"
            min="1"
            value={draft.num_results ?? 1}
            oninput={(event) => setNumber('num_results', event.currentTarget.value)}
          />
        </label>
      {/if}
      <label class="wide">
        Details sent to the provider
        <input
          value={(draft.allowed_identifiers ?? []).join(', ')}
          oninput={(event) => setList('allowed_identifiers', event.currentTarget.value)}
        />
      </label>
      <label class="wide">
        Fields it may fill in
        <input
          value={(draft.target_keys ?? []).join(', ')}
          oninput={(event) => setList('target_keys', event.currentTarget.value)}
        />
      </label>
      <label>
        How long the provider keeps data
        <input
          required
          value={draft.retention_posture}
          oninput={(event) => setString('retention_posture', event.currentTarget.value)}
        />
      </label>
      <label>
        Whether the provider trains on data
        <input
          required
          value={draft.training_posture}
          oninput={(event) => setString('training_posture', event.currentTarget.value)}
        />
      </label>
      <label>
        Refresh every
        <input
          required
          value={draft.refresh_interval}
          oninput={(event) => setString('refresh_interval', event.currentTarget.value)}
        />
      </label>
      <label>
        Request timeout
        <input
          required
          value={draft.request_timeout}
          oninput={(event) => setString('request_timeout', event.currentTarget.value)}
        />
      </label>
      {#if provider.kind === 'sixtyfour'}
        <label>
          Check for results every
          <input
            required
            value={draft.poll_interval}
            oninput={(event) => setString('poll_interval', event.currentTarget.value)}
          />
        </label>
        <label>
          Give up after
          <input
            required
            value={draft.max_job_age}
            oninput={(event) => setString('max_job_age', event.currentTarget.value)}
          />
        </label>
      {/if}
      <label>
        Retries
        <input
          type="number"
          min="0"
          value={draft.max_retries}
          oninput={(event) => setNumber('max_retries', event.currentTarget.value)}
        />
      </label>
      <label>
        Requests per run
        <input
          type="number"
          min="1"
          value={draft.max_requests_per_run}
          oninput={(event) => setNumber('max_requests_per_run', event.currentTarget.value)}
        />
      </label>
      <label>
        Requests per day
        <input
          type="number"
          min="1"
          value={draft.max_requests_per_day}
          oninput={(event) => setNumber('max_requests_per_day', event.currentTarget.value)}
        />
      </label>
      <div class="sensitive-targets">
        <span>Allow sensitive fields</span>
        <Toggle
          ariaLabel={`Allow sensitive fields for ${provider.name}`}
          checked={draft.allow_sensitive_targets}
          onchange={(allow_sensitive_targets) => {
                onchange({ ...draft, allow_sensitive_targets });
          }}
        />
      </div>
    </div>
  </details>
</article>

<style>
  .provider-card {
    display: grid;
    gap: var(--field-gap);
    padding: var(--pane-pad-x);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }
  header,
  .sensitive-targets {
    display: flex;
    align-items: center;
    gap: var(--control-gap);
  }
  .grow {
    flex: 1;
  }
  h4 {
    margin: 0;
  }
  h4 {
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
  }
  .field {
    display: grid;
    gap: var(--space-2);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  .setting--changed > span::after {
    content: '';
    display: inline-block;
    width: 6px;
    height: 6px;
    margin-left: var(--space-4);
    border-radius: 50%;
    background: var(--accent-amber);
    vertical-align: middle;
  }
  .advanced summary {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    cursor: pointer;
  }
  .provider-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--field-gap) var(--section-gap);
    margin-top: var(--field-gap);
  }
  label {
    display: grid;
    gap: var(--space-2);
    min-width: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  .wide,
  .sensitive-targets {
    grid-column: 1 / -1;
  }
  .sensitive-targets {
    justify-content: space-between;
    font-size: var(--font-size-sm);
  }
  input {
    box-sizing: border-box;
    min-width: 0;
    height: var(--control-height);
    width: 100%;
    padding: 0 var(--space-3);
    border: var(--border-width) solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
  }

  @media (max-width: 760px) {
    .provider-grid {
      grid-template-columns: 1fr;
    }
    .wide,
    .sensitive-targets {
      grid-column: auto;
    }
  }
</style>
