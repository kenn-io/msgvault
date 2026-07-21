#!/usr/bin/env node

import { createServer, request } from 'node:http';

const [upstreamValue, expectedAPIKey] = process.argv.slice(2);
if (!upstreamValue || !expectedAPIKey) {
  process.stderr.write('usage: smoke-api-key-proxy.mjs UPSTREAM API_KEY\n');
  process.exit(2);
}

const upstream = new URL(upstreamValue);
if (upstream.protocol !== 'http:' || upstream.hostname !== '127.0.0.1') {
  process.stderr.write('smoke API-key proxy requires a loopback HTTP upstream\n');
  process.exit(2);
}

const server = createServer((incoming, outgoing) => {
  if (incoming.headers['x-api-key'] !== expectedAPIKey) {
    outgoing.writeHead(401, { 'content-type': 'application/json' });
    outgoing.end('{"error":"X-Api-Key required"}\n');
    return;
  }

  const target = new URL(incoming.url ?? '/', upstream);
  const headers = { ...incoming.headers, host: target.host };
  delete headers['x-api-key'];
  headers.authorization = `Bearer ${expectedAPIKey}`;
  const forwarded = request(target, { method: incoming.method, headers }, (response) => {
    outgoing.writeHead(response.statusCode ?? 502, response.headers);
    response.pipe(outgoing);
  });
  forwarded.on('error', (error) => {
    if (!outgoing.headersSent) outgoing.writeHead(502, { 'content-type': 'text/plain' });
    outgoing.end(`proxy request failed: ${error.message}\n`);
  });
  incoming.pipe(forwarded);
});

server.on('error', (error) => {
  process.stderr.write(`smoke API-key proxy failed: ${error.message}\n`);
  process.exitCode = 1;
});
server.listen(0, '127.0.0.1', () => {
  const address = server.address();
  if (!address || typeof address === 'string') {
    process.stderr.write('smoke API-key proxy did not bind TCP\n');
    process.exit(1);
  }
  process.stdout.write(`http://127.0.0.1:${address.port}\n`);
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => server.close(() => process.exit(0)));
}
