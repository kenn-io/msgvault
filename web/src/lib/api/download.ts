import type { APIClient } from './client';
import { getCLIMessageRaw } from './generated/api/api';

export async function downloadRawMessage(client: APIClient, messageID: number, filename: string): Promise<void> {
  const { data, response } = await getCLIMessageRaw({ id: String(messageID) }, client);
  if (!response.ok || data === undefined) throw new Error('The original message isn’t available.');
  const objectURL = URL.createObjectURL(data);
  try {
    const anchor = document.createElement('a');
    anchor.href = objectURL;
    anchor.download = filename;
    anchor.click();
  } finally {
    URL.revokeObjectURL(objectURL);
  }
}
