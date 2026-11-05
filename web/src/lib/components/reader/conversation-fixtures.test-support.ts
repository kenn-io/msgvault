import type { MessageDetail } from '../../api/generated/models';

export function message(id: number, type = 'email'): MessageDetail {
  return {
    id,
    conversation_id: 7,
    subject: `Message subject ${id}`,
    message_type: type,
    from: 'alice@example.com',
    to: ['bob@example.com'],
    sent_at: `2026-01-0${id}T12:00:00Z`,
    snippet: `Preview ${id}`,
    labels: [],
    has_attachments: false,
    hidden_text_present: false,
    size_bytes: 10,
    body: `Body ${id}`,
    body_html: `<p>Body ${id}</p>`,
    attachments: []
  };
}
