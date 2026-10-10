export const recordingMessage = {
  id: 9, conversation_id: 2, subject: '', conversation_title: 'Team chat', body: 'Source recording',
  from: 'Example Person', to: [], sent_at: '2026-07-18T12:00:00Z', snippet: '', size_bytes: 44,
  has_attachments: true, attachments: [{ id: 1, filename: 'voice.wav', mime_type: 'audio/wav', size_bytes: 2048 }], labels: [],
};

export const recordingTranscript = {
  message_id: 9,
  recordings: [{ attachment_id: 1, filename: 'voice.wav', size_bytes: 2048, state: 'ready',
    transcript: { origin: 'supplied', partial: false, units: [{ text: 'Quarterly numbers in the source transcript' }] } }],
};
