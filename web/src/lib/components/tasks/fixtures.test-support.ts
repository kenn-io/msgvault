import type { TaskLinkLookupResponse } from '../../api/generated/models';
import { createAPIClient } from '../../api/client';

export const taskOutboundMetadata = { archive_uid: 'archive-a', message_id: 42, conversation_id: 7, source_type: 'gmail', source_identifier: 'archive@example.com', source_message_id: 'source-42', subject: 'Follow up', from: 'sender@example.com', sent_at: '2026-07-18T12:00:00Z' };
export function taskLookup(overrides: Partial<TaskLinkLookupResponse> = {}, messageId = 42, conversationId = 7): TaskLinkLookupResponse {
  return { state: 'ready', complete: true, last_scan: '', tasks: [], outbound_metadata: { ...taskOutboundMetadata, message_id: messageId, conversation_id: conversationId }, ...overrides };
}

export function disabledTaskLookup(messageId = 42, conversationId = 7): TaskLinkLookupResponse {
  return taskLookup({ state: 'disabled', complete: false }, messageId, conversationId);
}

export function taskCreationClient(requests: Request[] = []) {
  return createAPIClient(async (input) => {
    const request = input as Request;
    requests.push(request);
    if (request.method === 'POST') return Response.json({ task: { id: 'task-1', title: 'Follow up' } }, { status: 201 });
    if (request.url.includes('/search')) return Response.json({ tasks: [], truncated: false });
    return Response.json(request.url.endsWith('/status')
      ? { state: 'ready', project: 'example' }
      : taskLookup());
  });
}
