export function batch(id: string, status: string, count = 2, created = '2026-07-19T10:00:00Z') {
  return { id, status, created_at: created, created_by: 'api', description: 'reviewed explicit selection', message_count: count };
}
