import '../../src/app.css';
import { mount } from 'svelte';

import PeopleInferenceSettings from '../../src/lib/components/settings/PeopleInferenceSettings.svelte';
import type { PeopleInferencePort } from '../../src/lib/settings/people-inference-controller.svelte';

const name = 'routed-profile-with-a-long-visible-name';
const profile = {
  name, provider: 'openrouter' as const, model: 'model-one',
  endpoint: 'https://openrouter.example.test/api/v1', credentialStatus: 'Stored key',
  fingerprint: 'synthetic-fingerprint',
};
const status = {
  profiles: [profile, { ...profile, name: 'spare-profile', fingerprint: 'synthetic-fingerprint-spare' }],
  configuredName: name, runningName: 'previous-profile',
  configuredEnabled: true, runningEnabled: true, pendingRestart: true,
};
const port: PeopleInferencePort = {
  load: async () => status,
  create: async () => status,
  saveKey: async () => status,
  startLogin: async () => { throw new Error('Not used in this fixture.'); },
  pollLogin: async () => { throw new Error('Not used in this fixture.'); },
  cancelLogin: async () => undefined,
  models: async () => [],
  check: async () => ({ fingerprint: profile.fingerprint, disclosure: {
    sourceClasses: ['conversation_text', 'meeting_text'], since: '2025-01-01',
    sensitiveContent: false,
    endpoint: 'https://openrouter.example.test/a/long/path/to/the/selected/endpoint/for/this/model',
    retention: 'Operator assertion: no retention', training: 'Operator assertion: no training',
  } }),
  consent: async () => status,
  revoke: async () => status,
  disable: async () => status,
  remove: async () => status,
  select: async () => status,
};

mount(PeopleInferenceSettings, { target: document.getElementById('app')!, props: { port } });
