import { expect, test } from '@playwright/test';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';

test('People sweep keeps removal unavailable until another profile exists', async ({ page }) => {
  await installMixedArchive(page);
  const profile = {
    name: 'router', protocol: 'openai_chat', model: 'model-one',
    credential_source: 'env', credential_configured: true,
    checked: true, consent_active: false, fingerprint: 'router-fingerprint',
  };
  const status = {
    profiles: [profile], configured_name: 'router', configured_enabled: true,
    running_enabled: false, pending_restart: false,
  };
  let removals = 0;
  await page.route('**/api/v1/settings/people-inference', (route) => route.fulfill({
    headers: { ETag: '"config-1"' }, json: status,
  }));
  await page.route('**/api/v1/settings/people-inference/disable', (route) => {
    status.configured_enabled = false;
    return route.fulfill({ headers: { ETag: '"config-2"' }, json: status });
  });
  await page.route('**/api/v1/settings/people-inference/providers/router', (route) => {
    expect(route.request().method()).toBe('DELETE');
    removals += 1;
    status.profiles = status.profiles.filter((item) => item.name !== 'router');
    status.configured_name = 'backup';
    return route.fulfill({ headers: { ETag: '"config-3"' }, json: status });
  });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
  await page.getByRole('button', { name: 'People lookups', exact: true }).click();
  const remove = page.getByRole('button', { name: 'Remove profile', exact: true });
  await expect(remove).toBeDisabled();
  await page.getByRole('button', { name: 'Disable people sweep', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Disable people sweep', exact: true })).toHaveCount(0);
  await expect(remove).toBeDisabled();
  await expect(page.getByText('Add another profile before removing this one.')).toBeVisible();
  expect(removals).toBe(0);

  status.profiles.push({ ...profile, name: 'backup', fingerprint: 'backup-fingerprint' });
  await page.getByRole('button', { name: 'Reload people sweep settings' }).click();
  await expect(remove).toBeEnabled();
  await remove.click();
  await page.getByRole('button', { name: 'Confirm removal' }).click();
  await expect(page.getByText('Add another profile before removing this one.')).toBeVisible();
  await expect(remove).toBeDisabled();
  expect(removals).toBe(1);
});

for (const width of [390, 320]) {
  test(`People sweep setup fits a ${width}px viewport with disclosure open`, async ({ page }) => {
    await installMixedArchive(page);
    const name = 'routed-profile-with-a-long-visible-name';
    const profile = {
      name, preset_id: 'openrouter', protocol: 'openai-chat', model: 'model-one',
      endpoint: 'https://openrouter.example.test/a/long/path/to/the/selected/endpoint/for/this/model',
      credential_source: 'stored', credential_configured: true, checked: true,
      consent_active: false, fingerprint: 'synthetic-fingerprint',
      allowed_sources: ['conversation_text', 'meeting_text'], source_since: '2025-01-01',
      allow_sensitive: false, retention_posture: 'No retention', training_posture: 'No training',
    };
    await page.route('**/api/v1/settings/people-inference', (route) => route.fulfill({
      headers: { ETag: '"config-a"' }, json: {
        profiles: [profile, { ...profile, name: 'spare-profile', fingerprint: 'spare-fingerprint' }],
        configured_name: name, running_name: 'previous-profile',
        configured_enabled: true, running_enabled: true, pending_restart: true,
      },
    }));
    await page.route('**/api/v1/settings/people-inference/providers/*/check', (route) => route.fulfill({
      json: { ok: true, fingerprint: 'synthetic-fingerprint' },
    }));
    await page.setViewportSize({ width, height: 844 });
    await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
    await page.getByRole('button', { name: 'People lookups', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'People sweep' })).toBeVisible();
    await page.getByRole('button', { name: 'Check provider' }).click();
    await expect(page.getByRole('region', { name: 'Archive disclosure' })).toBeVisible();

    const overflow = await page.evaluate(() => {
      const component = document.querySelector<HTMLElement>('[aria-label="People sweep settings"]')!;
      return { viewport: window.innerWidth, document: document.documentElement.scrollWidth,
        component: component.scrollWidth, componentWidth: component.clientWidth };
    });
    expect(overflow.document).toBeLessThanOrEqual(overflow.viewport);
    expect(overflow.component).toBeLessThanOrEqual(overflow.componentWidth);
    await expect(page.getByRole('button', { name: 'Select and enable' })).toBeDisabled();
    await page.getByRole('combobox', { name: /^Profile:/ }).click();
    await page.getByRole('option', { name: 'spare-profile' }).click();
    await page.getByRole('button', { name: 'Remove profile' }).click();
    await expect(page.getByRole('button', { name: 'Confirm removal' })).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  });
}

test('Kept fields use the same text color after revealing sensitive fields', async ({ page }) => {
  await installMixedArchive(page);
  await page.route('**/api/v1/settings', route => route.fulfill({ json: {
    groups: [{ id: 'enrichment', label: 'People lookups' }],
    settings: [{ key: 'people.enrichment.enabled', label: 'Look people up', group: 'enrichment', kind: 'boolean', restart_required: false, value: { boolean: false } }], pending_restart: false,
  } }));
  await page.route('**/api/v1/settings/people-inference', route => route.fulfill({ headers: { ETag: '"config-1"' }, json: { profiles: [], configured_enabled: false, running_enabled: false, pending_restart: false } }));
  await page.route('**/api/v1/attribute-definitions*', route => route.fulfill({ json: { definitions: [
    { slug: 'favorite_plant', label: 'Favorite plant' }, { slug: 'private_note', label: 'Private note' },
  ] } }));
  await page.route('**/api/v1/person-fact-targets*', route => route.fulfill({ json: { targets: [
    { key: 'field:favorite_plant', kind: 'attribute', slug: 'favorite_plant', description: 'A plant', sensitive: false },
    ...(new URL(route.request().url()).searchParams.get('include_sensitive') === 'true' ? [{ key: 'field:private_note', kind: 'attribute', slug: 'private_note', description: 'A note', sensitive: true }] : []),
  ] } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
  await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'People lookups', exact: true }).click();
  const ordinary = page.getByText('Favorite plant', { exact: true });
  await expect(ordinary).toBeVisible();
  await expect(page.getByText('A plant', { exact: true })).toBeVisible();
  await page.getByText('Show sensitive fields', { exact: true }).click();
  const sensitive = page.getByText('Private note', { exact: true });
  await expect(sensitive).toBeVisible();
  await expect(page.getByText('A note', { exact: true })).toBeVisible();
  await expect(sensitive).toHaveCSS('color', await ordinary.evaluate(node => getComputedStyle(node).color));
});
