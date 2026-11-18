import { readFile } from 'node:fs/promises';
import { setTimeout as sleep } from 'node:timers/promises';

async function setup() {
  const base = new URL(process.env.KIBANA_URL ?? 'http://kibana:5601');
  if (!['http:', 'https:'].includes(base.protocol) || base.username || base.password ||
      base.search || base.hash || base.pathname !== '/') {
    throw new Error('KIBANA_URL must be an HTTP(S) root URL without credentials, query, or fragment');
  }
  const views = JSON.parse(await readFile(new URL('./data-views.json', import.meta.url), 'utf8'));
  const dashboard = JSON.parse(await readFile(new URL('./dashboard.json', import.meta.url), 'utf8'));
  if (typeof dashboard.id !== 'string' || !dashboard.id || !dashboard.dashboard?.title ||
      !Array.isArray(dashboard.dashboard.panels) || dashboard.dashboard.panels.length === 0) {
    throw new Error('dashboard.json requires an ID, title, and panels');
  }
  if (!Array.isArray(views) || views.length === 0) {
    throw new Error('data-views.json must contain a nonempty array');
  }
  const ids = new Set();
  for (const view of views) {
    for (const field of ['id', 'name', 'title', 'timeFieldName']) {
      if (typeof view[field] !== 'string' || view[field].trim() === '') {
        throw new Error(`Each data view requires a nonempty ${field}`);
      }
    }
    if (ids.has(view.id)) {
      throw new Error(`Duplicate data view ID: ${view.id}`);
    }
    ids.add(view.id);
  }

  const request = (path, options = {}) => fetch(new URL(path, base), {
    ...options,
    redirect: 'error',
    signal: options.signal ?? AbortSignal.timeout(5000),
    headers: { 'kbn-xsrf': 'gateway-setup', 'Content-Type': 'application/json' },
  });
  console.log('Waiting for Kibana readiness...');
  const deadline = Date.now() + 300000;
  let ready = false;
  while (Date.now() < deadline) {
    try {
      const response = await request('/api/status');
      if (response.status === 200) {
        const status = await response.json();
        if (status.status?.overall?.level === 'available') {
          ready = true;
          break;
        }
      } else {
        await response.body?.cancel();
        if (response.status !== 503) {
          throw new Error(`Kibana readiness returned HTTP ${response.status}`);
        }
      }
    } catch (error) {
      const transient = error.name === 'TimeoutError' ||
        ['ECONNREFUSED', 'ECONNRESET', 'ENOTFOUND', 'EAI_AGAIN', 'UND_ERR_SOCKET', 'UND_ERR_CONNECT_TIMEOUT'].includes(error.cause?.code);
      if (!transient) {
        throw error;
      }
    }
    await sleep(2000);
  }
  if (!ready) {
    throw new Error('Kibana did not become available within five minutes');
  }

  async function exists(id) {
    const response = await request(`/api/data_views/data_view/${encodeURIComponent(id)}`);
    if (response.status === 200) {
      const result = await response.json();
      if (result.data_view?.id !== id) {
        throw new Error(`Kibana returned an invalid data view for ${id}`);
      }
      return true;
    }
    await response.body?.cancel();
    if (response.status === 404) {
      return false;
    }
    throw new Error(`Read data view ${id}: HTTP ${response.status}`);
  }

  for (const view of views) {
    if (await exists(view.id)) {
      console.log(`Preserved existing data view: ${view.id}`);
      continue;
    }
    const response = await request('/api/data_views/data_view', {
      method: 'POST',
      body: JSON.stringify({ data_view: view, override: false }),
    });
    if (response.status === 200) {
      const result = await response.json();
      if (result.data_view?.id !== view.id) {
        throw new Error(`Kibana did not create the requested ID: ${view.id}`);
      }
      console.log(`Created data view: ${view.id}`);
    } else {
      await response.body?.cancel();
      if (response.status !== 409 || !(await exists(view.id))) {
        throw new Error(`Create data view ${view.id}: HTTP ${response.status}`);
      }
      console.log(`Preserved concurrently created data view: ${view.id}`);
    }
  }
  const dashboardPath = `/api/dashboards/${encodeURIComponent(dashboard.id)}`;
  const existing = await request(dashboardPath);
  if (existing.status === 200) {
    const result = await existing.json();
    if (result.id !== dashboard.id) {
      throw new Error('Kibana returned an invalid dashboard ID');
    }
    console.log(`Preserved existing dashboard: ${dashboard.id}`);
  } else {
    await existing.body?.cancel();
    if (existing.status !== 404) {
      throw new Error(`Read dashboard ${dashboard.id}: HTTP ${existing.status}`);
    }
    const created = await request(dashboardPath, {
      method: 'PUT',
      body: JSON.stringify(dashboard.dashboard),
      signal: AbortSignal.timeout(20000),
    });
    if (created.status !== 200 && created.status !== 201) {
      await created.body?.cancel();
      throw new Error(`Create dashboard ${dashboard.id}: HTTP ${created.status}`);
    }
    const result = await created.json();
    if (result.id !== dashboard.id ||
        result.data?.panels?.length !== dashboard.dashboard.panels.length ||
        dashboard.dashboard.panels.some((panel) => !result.data.panels.some((saved) => saved.id === panel.id))) {
      throw new Error('Kibana did not save all requested dashboard panels');
    }
    console.log(`Created dashboard: ${dashboard.id}`);
  }
  console.log('Gateway data views and dashboard are ready.');
}

setup().catch((error) => {
  console.error(`Kibana setup failed: ${error.message}`);
  process.exitCode = 1;
});
