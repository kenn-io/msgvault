import { expect, test } from '@playwright/test';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';

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
    await page.getByRole('button', { name: /^People sweep/ }).click();
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
