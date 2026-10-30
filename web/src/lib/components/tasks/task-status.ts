import type { getTaskIntegrationStatus } from '../../api/generated/api/api';

export const conversationTaskStatusKey = Symbol('conversation-task-status');
export type TaskStatusLoader = () => ReturnType<typeof getTaskIntegrationStatus>;

export interface ConversationTaskStatus {
  load: TaskStatusLoader;
  readonly latest: Awaited<ReturnType<TaskStatusLoader>>['data'];
}
