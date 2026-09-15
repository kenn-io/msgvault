import { expect, test } from '@playwright/test';

for (const messageType of ['email', 'whatsapp']) {
  test(`direct ${messageType} link opens its message without Explore pagination`, async ({ page }) => {
    const message = {
      id: 42001, source_id: 3, source_message_id: 'source-message', conversation_id: 71,
      subject: 'Linked message', message_type: messageType, from: 'sender@example.com',
      to: ['reader@example.com'], sent_at: '2020-01-01T12:00:00Z', snippet: 'Old message',
      labels: [], has_attachments: false, size_bytes: 20, body: 'The requested old message', attachments: [],
    };
    const archiveRequests: string[] = [];
    await page.route('**/api/session', (route) => route.fulfill({ json: {
      auth_mode: 'loopback', https: false, plain_http_warning: false,
    } }));
    await page.route('**/api/v1/**', async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname === '/api/v1/settings') return route.fulfill({ json: { settings: [], pending_restart: false } });
      archiveRequests.push(url.pathname);
      if (url.pathname === '/api/v1/messages/42001') return route.fulfill({ json: message });
      if (url.pathname === '/api/v1/conversations/71') {
        expect(url.searchParams.get('anchor')).toBe('42001');
        return route.fulfill({ json: { id: 71, anchor_id: 42001, messages: [message], has_before: true, has_after: true, total: 100000 } });
      }
      return route.fulfill({ status: 404, json: { message: 'Unexpected archive request' } });
    });
    await page.goto('/messages/42001');
    await expect(page.getByRole('article', { name: 'Message 42001' })).toContainText('The requested old message');
    expect(archiveRequests).toEqual(['/api/v1/messages/42001', '/api/v1/conversations/71']);
    await page.screenshot({ path: `/tmp/msgvault-direct-${messageType}.png` });
  });
}
