const assert = require('node:assert/strict');
const http = require('node:http');
const { once } = require('node:events');
const { test } = require('node:test');

const { startServer } = require('./server');

test('hello-node binds to loopback and serves its health endpoint', async (t) => {
  const server = startServer(0);
  t.after(async () => {
    if (server.listening) {
      await new Promise((resolve, reject) => {
        server.close((err) => err ? reject(err) : resolve());
      });
    }
  });
  await once(server, 'listening');

  const address = server.address();
  assert.notEqual(address, null);
  assert.equal(address.address, '127.0.0.1');

  const response = await new Promise((resolve, reject) => {
    http.get(`http://127.0.0.1:${address.port}/health`, (result) => {
      let body = '';
      result.setEncoding('utf8');
      result.on('data', (chunk) => { body += chunk; });
      result.on('end', () => resolve({ status: result.statusCode, body }));
    }).on('error', reject);
  });
  assert.deepEqual(response, { status: 200, body: 'ok' });
});
