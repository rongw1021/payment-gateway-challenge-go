import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { readFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const views = JSON.parse(await readFile(new URL('./data-views.json', import.meta.url), 'utf8'));
const dashboardDefinition = JSON.parse(await readFile(new URL('./dashboard.json', import.meta.url), 'utf8'));
const script = fileURLToPath(new URL('./setup.mjs', import.meta.url));

async function runSetup(address) {
  const child = spawn(process.execPath, [script], {
    env: { ...process.env, KIBANA_URL: address },
    stdio: ['ignore', 'pipe', 'pipe'],
    timeout: 15000,
  });
  let output = '';
  child.stdout.on('data', (data) => { output += data; });
  child.stderr.on('data', (data) => { output += data; });
  const [code] = await once(child, 'close');
  return { code, output };
}

async function mockKibana(t, handler) {
  const server = createServer((request, response) => {
    handler(request, response).catch((error) => {
      response.writeHead(500);
      response.end();
      t.assert.fail(error.message);
    });
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => new Promise((resolve) => server.close(resolve)));
  return `http://127.0.0.1:${server.address().port}`;
}

function respond(response, status, value) {
  response.writeHead(status, { 'Content-Type': 'application/json' });
  response.end(JSON.stringify(value));
}

test('creates views and dashboard without indices, waits for readiness, and preserves edits on rerun', async (t) => {
  const stored = new Map();
  let dashboard;
  let dashboardWrites = 0;
  let statusCalls = 0;
  let writes = 0;
  const address = await mockKibana(t, async (request, response) => {
    if (request.url === '/api/status') {
      statusCalls++;
      respond(response, statusCalls === 1 ? 503 : 200, { status: { overall: { level: 'available' } } });
      return;
    }
    assert.ok(statusCalls >= 2, 'must not provision before Kibana is ready');
    assert.equal(request.headers['kbn-xsrf'], 'gateway-setup');
    if (request.url === '/api/dashboards/payment-gateway-dashboard') {
      if (request.method === 'GET') {
        respond(response, dashboard ? 200 : 404, dashboard ?? {});
      } else {
        assert.equal(request.method, 'PUT');
        let body = '';
        for await (const chunk of request) body += chunk;
        dashboard = { id: dashboardDefinition.id, data: JSON.parse(body) };
        assert.equal(dashboard.data.title, 'Payment Gateway Dashboard');
        assert.equal(dashboard.data.panels.length, 5);
        assert.equal(dashboard.data.panels[0].config.layers[0].y[0].time_scale, 's');
        dashboardWrites++;
        respond(response, 201, dashboard);
      }
      return;
    }
    if (request.method === 'GET') {
      const id = decodeURIComponent(request.url.split('/').at(-1));
      respond(response, stored.has(id) ? 200 : 404, { data_view: stored.get(id) });
      return;
    }
    assert.equal(request.url, '/api/data_views/data_view');
    let body = '';
    for await (const chunk of request) body += chunk;
    const payload = JSON.parse(body);
    assert.equal(payload.override, false);
    assert.equal(payload.data_view.allowNoIndex, true);
    assert.equal(payload.data_view.timeFieldName, '@timestamp');
    writes++;
    stored.set(payload.data_view.id, payload.data_view);
    respond(response, 200, payload);
  });
  const first = await runSetup(address);
  assert.equal(first.code, 0, first.output);
  assert.deepEqual([...stored.keys()], ['payment-gateway-logs', 'payment-gateway-metrics', 'payment-gateway-traces']);
  const changed = { ...stored.get(views[0].id), name: 'Engineer-customized logs' };
  stored.set(changed.id, changed);
  dashboard.data.title = 'Engineer-customized dashboard';
  const second = await runSetup(address);
  assert.equal(second.code, 0, second.output);
  assert.equal(writes, 3, 'rerunning must not overwrite existing saved objects');
  assert.equal(stored.get(changed.id).name, 'Engineer-customized logs');
  assert.equal(dashboardWrites, 1);
  assert.equal(dashboard.data.title, 'Engineer-customized dashboard');
});

test('fails explicitly when provisioning is forbidden', async (t) => {
  const address = await mockKibana(t, async (request, response) => {
    if (request.url === '/api/status') {
      respond(response, 200, { status: { overall: { level: 'available' } } });
    } else {
      respond(response, request.method === 'GET' ? 404 : 403, {});
    }
  });
  const result = await runSetup(address);
  assert.equal(result.code, 1, result.output);
  assert.match(result.output, /Create data view payment-gateway-logs: HTTP 403/);
});

test('accepts a concurrent create only if the expected ID exists', async (t) => {
  const stored = new Map();
  const address = await mockKibana(t, async (request, response) => {
    if (request.url === '/api/status') {
      respond(response, 200, { status: { overall: { level: 'available' } } });
    } else if (request.url === '/api/dashboards/payment-gateway-dashboard') {
      respond(response, 200, { id: dashboardDefinition.id, data: dashboardDefinition.dashboard });
    } else if (request.method === 'GET') {
      const id = decodeURIComponent(request.url.split('/').at(-1));
      respond(response, stored.has(id) ? 200 : 404, { data_view: stored.get(id) });
    } else {
      let body = '';
      for await (const chunk of request) body += chunk;
      const payload = JSON.parse(body);
      stored.set(payload.data_view.id, payload.data_view);
      respond(response, 409, {});
    }
  });
  const result = await runSetup(address);
  assert.equal(result.code, 0, result.output);
  assert.equal(stored.size, 3);
});

test('fails explicitly when dashboard creation fails after views are ready', async (t) => {
  const address = await mockKibana(t, async (request, response) => {
    if (request.url === '/api/status') {
      respond(response, 200, { status: { overall: { level: 'available' } } });
    } else if (request.url.startsWith('/api/data_views/')) {
      const id = decodeURIComponent(request.url.split('/').at(-1));
      respond(response, 200, { data_view: { id } });
    } else {
      respond(response, request.method === 'GET' ? 404 : 403, {});
    }
  });
  const result = await runSetup(address);
  assert.equal(result.code, 1, result.output);
  assert.match(result.output, /Create dashboard payment-gateway-dashboard: HTTP 403/);
});

test('rejects a successful API response that omitted a requested dashboard panel', async (t) => {
  const address = await mockKibana(t, async (request, response) => {
    if (request.url === '/api/status') {
      respond(response, 200, { status: { overall: { level: 'available' } } });
    } else if (request.url.startsWith('/api/data_views/')) {
      const id = decodeURIComponent(request.url.split('/').at(-1));
      respond(response, 200, { data_view: { id } });
    } else if (request.method === 'GET') {
      respond(response, 404, {});
    } else {
      respond(response, 201, {
        id: dashboardDefinition.id,
        data: { ...dashboardDefinition.dashboard, panels: dashboardDefinition.dashboard.panels.slice(1) },
      });
    }
  });
  const result = await runSetup(address);
  assert.equal(result.code, 1, result.output);
  assert.match(result.output, /did not save all requested dashboard panels/);
});
