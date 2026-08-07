import { readFile } from 'node:fs/promises';

const [, , baseURL, manifestPath] = process.argv;
if (!baseURL || !manifestPath) throw new Error('usage: smoke_fixture.mjs BASE_URL MANIFEST');
const manifest = JSON.parse(await readFile(manifestPath, 'utf8'));
const request = async (path, body) => {
  const response = await fetch(`${baseURL}${path}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body)
  });
  if (!response.ok) throw new Error(`${path} returned HTTP ${response.status}: ${await response.text()}`);
  return response.json();
};
const get = async (path) => {
  const response = await fetch(`${baseURL}${path}`);
  if (!response.ok) throw new Error(`${path} returned HTTP ${response.status}: ${await response.text()}`);
  return response.json();
};
const expectedMessageCount = manifest.fixture?.message_count;
const expectedParticipantCount = manifest.fixture?.participant_count;
if (!Number.isInteger(expectedMessageCount) || !Number.isInteger(expectedParticipantCount)) {
  throw new Error('manifest is missing integer fixture message and participant counts');
}
const expectedParticipants = manifest.selection?.selected_participants;
if (!Array.isArray(expectedParticipants) || expectedParticipants.length !== expectedParticipantCount) {
  throw new Error(
    `manifest selection.selected_participants (${expectedParticipants?.length ?? 'missing'}) does not match fixture participant count ${expectedParticipantCount}`
  );
}
const expectedEdges = manifest.selection?.message_recipient_edges;
if (!Array.isArray(expectedEdges) || expectedEdges.length !== expectedMessageCount) {
  throw new Error('manifest selection.message_recipient_edges does not match fixture message count');
}

const overview = await request('/api/v1/explore', { limit: 500, presentation: 'table' });
if (!Number.isInteger(overview.total_count) || overview.total_count !== expectedMessageCount) {
  throw new Error(`overview total ${overview.total_count ?? 'missing'} does not match manifest message count ${expectedMessageCount}`);
}
if (!Array.isArray(overview.rows) || overview.rows.length !== expectedMessageCount) {
  throw new Error(`overview returned ${overview.rows?.length ?? 0} rows for ${expectedMessageCount} messages`);
}
const participantIds = new Set();
for (const row of overview.rows) {
  if (!Array.isArray(row.participant_ids)) throw new Error('overview row has no participant population');
  for (const participantId of row.participant_ids) {
    if (!Number.isInteger(participantId)) throw new Error('overview row contains an invalid participant ID');
    participantIds.add(participantId);
  }
}
if (participantIds.size === 0) throw new Error('overview rows did not expose any participant IDs');

const importedMessages = [];
for (let page = 1; importedMessages.length < expectedMessageCount; page += 1) {
  const response = await get(`/api/v1/messages?page=${page}&page_size=100`);
  if (!Number.isInteger(response.total) || response.total !== expectedMessageCount) {
    throw new Error(`message list total ${response.total ?? 'missing'} does not match manifest message count ${expectedMessageCount}`);
  }
  if (!Array.isArray(response.messages) || response.messages.length === 0) {
    throw new Error(`message list ended after ${importedMessages.length} messages`);
  }
  importedMessages.push(...response.messages);
  if (importedMessages.length > expectedMessageCount) {
    throw new Error('message list returned more messages than the manifest count');
  }
}
const emailPattern = /[A-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Z0-9.-]+\.[A-Z]{2,}/gi;
const normalizeStructuredAddresses = (value, field, messageID) => {
  const values = field === 'from' ? [value] : (value ?? []);
  if (!Array.isArray(values) || values.some((entry) => typeof entry !== 'string')) {
    throw new Error(`message ${messageID} has invalid structured ${field} recipients`);
  }
  const addresses = new Set();
  for (const entry of values) {
    const matches = entry.match(emailPattern) ?? [];
    if (matches.length === 0) throw new Error(`message ${messageID} has an unparsable structured ${field} recipient`);
    for (const email of matches) addresses.add(email.toLowerCase());
  }
  return [...addresses].sort();
};
const importedParticipants = new Set();
for (const message of importedMessages) {
  const detail = await get(`/api/v1/messages/${encodeURIComponent(message.id)}`);
  if (!Number.isInteger(detail.id) || detail.id !== message.id) {
    throw new Error(`message detail ${message.id} returned an unexpected ID`);
  }
  const sequenceMatch = /^(?:mbox-[0-9a-f]{64})-(\d+)$/.exec(message.source_message_id ?? '');
  if (!sequenceMatch) throw new Error(`message ${message.id} has an unexpected importer source ID`);
  const sequence = Number(sequenceMatch[1]);
  const expected = expectedEdges[sequence - 1];
  if (!expected || expected.sequence !== sequence) {
    throw new Error(`message ${message.id} has no manifest recipient edge for importer sequence ${sequence}`);
  }
  const actual = {
    from: normalizeStructuredAddresses(detail.from, 'from', message.id),
    to: normalizeStructuredAddresses(detail.to, 'to', message.id),
    cc: normalizeStructuredAddresses(detail.cc, 'cc', message.id),
    bcc: normalizeStructuredAddresses(detail.bcc, 'bcc', message.id),
  };
  const expectedFields = {
    from: [expected.from],
    to: [...expected.to].sort(),
    cc: [...expected.cc].sort(),
    bcc: [...expected.bcc].sort(),
  };
  for (const field of ['from', 'to', 'cc', 'bcc']) {
    if (JSON.stringify(actual[field]) !== JSON.stringify(expectedFields[field])) {
      throw new Error(`message ${message.id} structured ${field} recipients do not match manifest sequence ${sequence}`);
    }
    for (const address of actual[field]) importedParticipants.add(address);
  }
}
const expectedParticipantSet = new Set(expectedParticipants);
const missingParticipants = expectedParticipants.filter((address) => !importedParticipants.has(address));
const unexpectedParticipants = [...importedParticipants].filter((address) => !expectedParticipantSet.has(address)).sort();
if (missingParticipants.length > 0 || unexpectedParticipants.length > 0) {
  throw new Error(
    `imported participants do not match manifest selection.selected_participants: missing [${missingParticipants.join(', ')}], unexpected [${unexpectedParticipants.join(', ')}]`
  );
}

const relationships = await request('/api/v1/relationships', { limit: 50, show_all: true });
const minimum = manifest.fixture.minimum_relationship_results;
if (!Array.isArray(relationships.rows) || relationships.rows.length < minimum) {
  throw new Error(`relationship fan-out ${relationships.rows?.length ?? 0} is below ${minimum}`);
}
const selected = relationships.rows[0];
if (!Number.isInteger(selected.canonical_id) || !Array.isArray(selected.member_ids) || selected.member_ids.length === 0) {
  throw new Error('relationship row has no usable identity cluster');
}

const timeline = await request(`/api/v1/relationships/${selected.canonical_id}/timeline`, { timezone: 'UTC', limit: 50 });
if (!Array.isArray(timeline.rows) || timeline.rows.length === 0) throw new Error('selected relationship has an empty timeline');
if (!timeline.rows.some((row) => String(row.title ?? '').trim() && String(row.preview ?? '').trim())) {
  throw new Error('selected relationship timeline has no subject and preview content');
}
console.log(JSON.stringify({ overview_rows: overview.rows.length, imported_messages: importedMessages.length, imported_participants: importedParticipants.size, relationship_rows: relationships.rows.length, timeline_rows: timeline.rows.length, selected_label: selected.display_label }));
