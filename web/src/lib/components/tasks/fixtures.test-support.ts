import type { TaskLinkLookupResponse } from '../../api/generated/models';
import { createAPIClient } from '../../api/client';

export const taskOutboundMetadata = { archive_uid: 'archive-a', message_id: 42, conversation_id: 7, source_type: 'gmail', source_identifier: 'archive@example.com', source_message_id: 'source-42', subject: 'Follow up', from: 'sender@example.com', sent_at: '2026-07-18T12:00:00Z' };
export function disabledTaskLookup(messageId = 42, conversationId = 7): TaskLinkLookupResponse {
  return { state: 'disabled', complete: false, last_scan: '', tasks: [], outbound_metadata: { ...taskOutboundMetadata, message_id: messageId, conversation_id: conversationId } };
}

export function taskCreationClient(requests: Request[] = []) {
  return createAPIClient(async (input) => {
    const request = input as Request;
    requests.push(request);
    if (request.method === 'POST') return Response.json({ task: { id: 'task-1', title: 'Follow up' } }, { status: 201 });
    return Response.json(request.url.endsWith('/status')
      ? { state: 'ready', project: 'example' }
      : { state: 'ready', complete: true, last_scan: '', tasks: [], outbound_metadata: taskOutboundMetadata });
  });
}
