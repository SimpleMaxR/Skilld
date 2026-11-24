const TOKEN_STORAGE_KEY = 'skilld.token';

const STATUS_LABELS = {
  synced: 'Synced',
  outdated: 'Update Available',
  modified: 'Modified (Drift)',
  unmanaged: 'Unmanaged',
  missing: 'Missing',
};

const state = {
  token: localStorage.getItem(TOKEN_STORAGE_KEY) || '',
  unlocked: false,
  authError: '',
  overview: null,
  nav: 'matrix', // 'matrix' | 'vault' | 'actions' | 'engine' | 'device'
  selectedDeviceId: null,
  selectedSkillKey: null, // `${deviceId}:${agentId}:${skillName}`
  selectedVaultSkillId: null,
  deviceFilter: 'all', // 'all' | 'attention' | 'synced' | 'ignored'
  searchQuery: '',
  inspectorTab: 'diff', // 'diff' | 'files'
  selectedFilePath: null,
  diffData: null,
  diffCacheKey: null,
  vaultDetail: null,
  vaultDetailVersionId: null,
  vaultDetailVersionFiles: null,
  runtimeData: null,
  auditData: null,
  modal: null,
  toast: null,
  loading: false,
};

let pollTimer = null;
let toastTimer = null;

function escapeHtml(str) {
  return String(str ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function shortHash(hash, len = 10) {
  if (!hash) return '—';
  return String(hash).replace(/^sha256:/, '').slice(0, len);
}

function formatTimeAgo(iso) {
  if (!iso) return '—';
  const diffSec = Math.max(0, Math.floor((Date.now() - Date.parse(iso)) / 1000));
  if (diffSec < 60) return `${diffSec}s ago`;
  const min = Math.floor(diffSec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  return `${Math.floor(hr / 24)}d ago`;
}

function showToast(message, isError = false) {
  state.toast = { message, isError };
  render();
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    state.toast = null;
    render();
  }, 3600);
}

async function api(path, options = {}) {
  const res = await fetch(path, {
    method: options.method || 'GET',
    headers: {
      'content-type': 'application/json',
      authorization: `Bearer ${state.token}`,
      ...(options.headers || {}),
    },
    body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
  });

  if (res.status === 401) {
    state.unlocked = false;
    state.authError = 'Invalid or expired Vault token';
    stopPolling();
    render();
    throw new Error('Unauthorized');
  }

  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(data.error || `Request failed (${res.status})`);
  }
  return data;
}

async function unlockWithToken(token) {
  state.token = token.trim();
  state.authError = '';
  try {
    await api('/api/auth/verify');
    localStorage.setItem(TOKEN_STORAGE_KEY, state.token);
    state.unlocked = true;
    await refreshAll();
    startPolling();
  } catch (err) {
    state.authError = err.message || 'Authentication failed';
    state.unlocked = false;
    render();
  }
}

function lockVault() {
  localStorage.removeItem(TOKEN_STORAGE_KEY);
  state.token = '';
  state.unlocked = false;
  state.overview = null;
  stopPolling();
  render();
}

async function refreshAll() {
  if (!state.unlocked) return;
  try {
    const overview = await api('/api/overview');
    state.overview = overview;

    if (state.nav === 'device') {
      const stillHasDevice = overview.devices.some((d) => d.id === state.selectedDeviceId);
      if (!stillHasDevice && overview.devices.length > 0) {
        state.selectedDeviceId = overview.devices[0].id;
      }
      if (state.selectedSkillKey) {
        await loadDiffForSelected(true);
      }
    } else if (state.nav === 'vault' && state.selectedVaultSkillId) {
      await loadVaultSkillDetail(state.selectedVaultSkillId, true);
    } else if (state.nav === 'engine') {
      await loadEngineTelemetry(true);
    }
  } catch (err) {
    if (err.message !== 'Unauthorized') {
      showToast(err.message, true);
    }
  }
  render();
}

function startPolling() {
  stopPolling();
  pollTimer = setInterval(() => {
    if (state.unlocked && !state.modal) {
      refreshAll();
    }
  }, 5000);
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
}

async function loadDiffForSelected(silent = false) {
  if (!state.selectedSkillKey) return;
  const [deviceId, agentId, ...rest] = state.selectedSkillKey.split(':');
  const skillName = rest.join(':');
  const cacheKey = state.selectedSkillKey;

  try {
    const data = await api(
      `/api/installations/diff?deviceId=${encodeURIComponent(deviceId)}&agentId=${encodeURIComponent(
        agentId
      )}&skillName=${encodeURIComponent(skillName)}`
    );
    if (state.selectedSkillKey !== cacheKey) return;
    state.diffData = data;
    state.diffCacheKey = cacheKey;

    const availableFiles = Object.keys(data.localFiles || data.targetFiles || {});
    if (!state.selectedFilePath || !availableFiles.includes(state.selectedFilePath)) {
      state.selectedFilePath = availableFiles.includes('SKILL.md')
        ? 'SKILL.md'
        : availableFiles[0] || null;
    }
    if (!silent) {
      state.inspectorTab = data.diff?.hasChanges ? 'diff' : 'files';
      render();
    }
  } catch (err) {
    if (!silent) showToast(err.message, true);
  }
}

async function loadVaultSkillDetail(skillId, silent = false) {
  try {
    const data = await api(`/api/skills/${encodeURIComponent(skillId)}`);
    if (state.selectedVaultSkillId !== skillId) return;
    state.vaultDetail = data;

    const currentVer = data.skill.currentVersion;
    if (!state.vaultDetailVersionId || !data.skill.versions.some((v) => v.id === state.vaultDetailVersionId)) {
      state.vaultDetailVersionId = currentVer?.id || null;
      state.vaultDetailVersionFiles = currentVer?.files || {};
    } else if (state.vaultDetailVersionId === currentVer?.id) {
      state.vaultDetailVersionFiles = currentVer?.files || {};
    }

    const fileNames = Object.keys(state.vaultDetailVersionFiles || {});
    if (!state.selectedFilePath || !fileNames.includes(state.selectedFilePath)) {
      state.selectedFilePath = fileNames.includes('SKILL.md') ? 'SKILL.md' : fileNames[0] || null;
    }

    if (!silent) render();
  } catch (err) {
    if (!silent) showToast(err.message, true);
  }
}

async function loadEngineTelemetry(silent = false) {
  try {
    const [runtime, audit] = await Promise.all([
      api('/api/system/runtime'),
      api('/api/audit-logs'),
    ]);
    state.runtimeData = runtime;
    state.auditData = audit;
    if (!silent) render();
  } catch (err) {
    if (err.message && err.message.includes('404')) {
      state.runtimeData = { unsupported: true };
      if (!silent) render();
      return;
    }
    if (!silent) showToast(err.message, true);
  }
}

function statusPill(status) {
  const label = STATUS_LABELS[status] || status;
  return `<span class="pill pill-${escapeHtml(status)}">${escapeHtml(label)}</span>`;
}

// --- Render Root ---
function render() {
  const root = document.getElementById('app');
  if (!state.unlocked) {
    root.innerHTML = renderUnlockScreen();
    bindEvents();
    return;
  }

  const wideMain = state.nav === 'matrix' || state.nav === 'actions' || state.nav === 'engine';
  root.innerHTML = `
    <div class="app-shell ${wideMain ? 'wide-main' : ''}">
      ${renderRail()}
      ${wideMain ? '' : renderMiddlePane()}
      ${renderMainOrInspectorPane()}
    </div>
    ${state.modal ? renderModal() : ''}
    ${state.toast ? `<div class="toast" style="border-color:${state.toast.isError ? 'var(--rose)' : 'var(--emerald)'}">${escapeHtml(state.toast.message)}</div>` : ''}
  `;
  bindEvents();
}

function renderUnlockScreen() {
  return `
    <div class="modal-backdrop">
      <div class="modal-card" style="max-width:420px">
        <div class="modal-header">
          <div class="brand">
            <div class="brand-mark">SD</div>
            <div>
              <div class="brand-title">Skilld</div>
              <div class="brand-sub">Multi-Device Agent Skills Control Plane</div>
            </div>
          </div>
        </div>
        <form id="unlock-form" class="modal-body">
          <p style="margin:0;color:var(--text-muted)">
            Enter your <code class="mono">SKILLD_TOKEN</code> to unlock your personal skills vault.
          </p>
          ${state.authError ? `<div class="banner error" style="margin-top:0">${escapeHtml(state.authError)}</div>` : ''}
          <div>
            <label class="field-label" for="token-input">Vault Access Token</label>
            <input id="token-input" class="field-input mono" type="password" placeholder="sd_..." value="${escapeHtml(state.token)}" autofocus required />
          </div>
          <div style="display:flex;justify-content:flex-end;gap:8px;margin-top:6px">
            <button type="submit" class="btn btn-primary">Unlock Vault</button>
          </div>
        </form>
      </div>
    </div>
  `;
}

function renderRail() {
  const ov = state.overview || { devices: [], skills: [], summary: {} };
  const summary = ov.summary || {};

  return `
    <aside class="rail">
      <div class="rail-header">
        <div class="brand">
          <div class="brand-mark">SD</div>
          <div>
            <div class="brand-title">Skilld</div>
            <div class="brand-sub">${summary.onlineDevices ?? 0}/${summary.totalDevices ?? 0} online</div>
          </div>
        </div>
        <button class="btn btn-sm" data-action="lock-vault" title="Lock Vault">Lock</button>
      </div>

      <div class="rail-scroll">
        <div class="rail-section-label">Overview</div>
        <button class="rail-item ${state.nav === 'matrix' ? 'active' : ''}" data-nav="matrix">
          <span class="rail-item-left"><span>Fleet Matrix</span></span>
          <span class="count-badge">${summary.totalInstallations ?? 0}</span>
        </button>
        <button class="rail-item ${state.nav === 'vault' ? 'active' : ''}" data-nav="vault">
          <span class="rail-item-left"><span>Vault Library</span></span>
          <span class="count-badge ${(summary.upstreamUpdates ?? 0) > 0 ? 'warn' : ''}">${summary.totalSkills ?? 0}</span>
        </button>
        <button class="rail-item ${state.nav === 'actions' ? 'active' : ''}" data-nav="actions">
          <span class="rail-item-left"><span>Activity Queue</span></span>
          <span class="count-badge ${(summary.pendingActionCount ?? 0) > 0 ? 'warn' : ''}">${summary.pendingActionCount ?? 0}</span>
        </button>
        <button class="rail-item ${state.nav === 'engine' ? 'active' : ''}" data-nav="engine">
          <span class="rail-item-left"><span>Engine & Quotas</span></span>
          <span class="count-badge">Go</span>
        </button>

        <div class="rail-section-label">Devices</div>
        ${
          ov.devices.length === 0
            ? `<div style="padding:8px 10px;color:var(--text-dim);font-size:12px">No devices connected yet.</div>`
            : ov.devices
                .map(
                  (d) => `
                <button class="rail-item ${state.nav === 'device' && state.selectedDeviceId === d.id ? 'active' : ''}" data-device-id="${escapeHtml(d.id)}">
                  <span class="rail-item-left">
                    <span class="status-dot ${d.online ? 'online' : 'offline'}"></span>
                    <span class="rail-item-title">${escapeHtml(d.name)}</span>
                  </span>
                  ${
                    d.attentionCount > 0
                      ? `<span class="count-badge warn">${d.attentionCount}</span>`
                      : `<span class="count-badge">${d.skillCount}</span>`
                  }
                </button>`
                )
                .join('')
        }
      </div>

      <div class="rail-footer">
        <button class="btn btn-sm" data-action="open-connect-modal">+ Connect Device</button>
        <button class="btn btn-sm" data-action="refresh-now">Refresh Snapshot</button>
      </div>
    </aside>
  `;
}

function renderMiddlePane() {
  if (state.nav === 'vault') return renderVaultListPane();
  return renderDeviceListPane();
}

function renderVaultListPane() {
  const ov = state.overview || { skills: [] };
  const q = state.searchQuery.toLowerCase().trim();
  const filtered = ov.skills.filter(
    (s) =>
      !q ||
      s.name.toLowerCase().includes(q) ||
      s.id.toLowerCase().includes(q) ||
      (s.description || '').toLowerCase().includes(q)
  );

  return `
    <section class="list-pane">
      <div class="pane-header">
        <div class="pane-title-row">
          <h2 class="pane-title">Vault Library</h2>
          <div style="display:flex;gap:6px">
            <button class="btn btn-sm" data-action="check-all-upstream" title="Check GitHub upstreams">Check Upstream</button>
            <button class="btn btn-sm btn-primary" data-action="open-add-skill">+ New</button>
          </div>
        </div>
        <div style="display:flex;gap:6px">
          <input class="search-input" data-bind="search" placeholder="Search skills..." value="${escapeHtml(state.searchQuery)}" />
          <button class="btn btn-sm" data-action="open-import-github" title="Import from GitHub">GitHub</button>
        </div>
      </div>

      <div class="list-scroll">
        ${
          filtered.length === 0
            ? `<div class="empty-state">No skills in Vault yet.<br/><br/>Create a custom skill, import from GitHub, or adopt an unmanaged skill from any device.</div>`
            : filtered
                .map(
                  (s) => `
              <button class="skill-row ${state.selectedVaultSkillId === s.id ? 'active' : ''}" data-vault-skill="${escapeHtml(s.id)}">
                <div class="skill-row-top">
                  <span class="skill-row-name">${escapeHtml(s.name)}</span>
                  <span class="pill ${s.hasUpstreamUpdate ? 'pill-outdated' : 'pill-neutral'}">
                    v${s.currentVersionNum ?? 1}${s.hasUpstreamUpdate ? ' · upstream new' : ''}
                  </span>
                </div>
                <div class="skill-row-meta">
                  <span class="mono">${escapeHtml(s.id)} · ${escapeHtml(s.sourceType)}</span>
                  <span>${s.installedCount} inst${s.outdatedCount ? ` · ${s.outdatedCount} outdated` : ''}${s.modifiedCount ? ` · ${s.modifiedCount} drifted` : ''}</span>
                </div>
              </button>`
                )
                .join('')
        }
      </div>
    </section>
  `;
}

function renderDeviceListPane() {
  const ov = state.overview || { devices: [], installations: [] };
  const device = ov.devices.find((d) => d.id === state.selectedDeviceId);
  if (!device) {
    return `
      <section class="list-pane">
        <div class="empty-state">Select or connect a device.</div>
      </section>
    `;
  }

  const devInsts = ov.installations.filter((i) => i.deviceId === device.id);
  const q = state.searchQuery.toLowerCase().trim();

  const filtered = devInsts.filter((inst) => {
    if (q && !inst.skillName.toLowerCase().includes(q) && !inst.agentId.toLowerCase().includes(q)) {
      return false;
    }
    if (state.deviceFilter === 'attention') return !inst.ignored && inst.status !== 'synced';
    if (state.deviceFilter === 'synced') return inst.status === 'synced';
    if (state.deviceFilter === 'ignored') return inst.ignored;
    return true;
  });

  const agents = device.detectedAgents?.length
    ? device.detectedAgents
    : Array.from(new Set(devInsts.map((i) => i.agentId))).map((id) => ({
        id,
        name: id,
        skillsDir: '',
        exists: true,
      }));

  return `
    <section class="list-pane">
      <div class="pane-header">
        <div class="pane-title-row">
          <div>
            <h2 class="pane-title">${escapeHtml(device.name)}</h2>
            <div style="font-size:11px;color:var(--text-dim)">${escapeHtml(device.os)}/${escapeHtml(device.arch)} · seen ${formatTimeAgo(device.lastSeenAt)}</div>
          </div>
          <button class="btn btn-sm btn-primary" data-action="open-deploy-modal" data-device-id="${escapeHtml(device.id)}">+ Deploy</button>
        </div>

        <input class="search-input" data-bind="search" placeholder="Filter skills or agents..." value="${escapeHtml(state.searchQuery)}" />

        <div class="filter-row">
          ${['all', 'attention', 'synced', 'ignored']
            .map(
              (f) =>
                `<button class="chip ${state.deviceFilter === f ? 'active' : ''}" data-filter="${f}">${
                  f === 'all' ? 'All' : f === 'attention' ? 'Attention' : f === 'synced' ? 'Synced' : 'Ignored'
                }</button>`
            )
            .join('')}
        </div>
      </div>

      <div class="list-scroll">
        ${agents
          .map((agent) => {
            const items = filtered.filter((i) => i.agentId === agent.id);
            if (!agent.exists && items.length === 0) return '';
            return `
              <div class="agent-group">
                <div class="agent-group-header">
                  <span class="agent-group-name">
                    ${escapeHtml(agent.name || agent.id)}
                    <span class="count-badge">${items.length}</span>
                  </span>
                  <span class="agent-group-path">${escapeHtml(agent.skillsDir || '')}</span>
                </div>
                ${
                  items.length === 0
                    ? `<div style="padding:6px 10px;font-size:11.5px;color:var(--text-dim)">No matching skills</div>`
                    : items
                        .map((inst) => {
                          const key = `${inst.deviceId}:${inst.agentId}:${inst.skillName}`;
                          return `
                            <button class="skill-row ${state.selectedSkillKey === key ? 'active' : ''}" data-inst-key="${escapeHtml(key)}">
                              <div class="skill-row-top">
                                <span class="skill-row-name">${escapeHtml(inst.skillName)}</span>
                                ${statusPill(inst.status)}
                              </div>
                              <div class="skill-row-meta">
                                <span class="mono">${inst.desiredVersionNum ? `Vault v${inst.desiredVersionNum}` : 'Local only'} · ${shortHash(inst.actualHash, 8)}</span>
                                ${inst.ignored ? '<span>Ignored</span>' : ''}
                              </div>
                            </button>
                          `;
                        })
                        .join('')
                }
              </div>
            `;
          })
          .join('')}
      </div>
    </section>
  `;
}

function renderMainOrInspectorPane() {
  if (state.nav === 'matrix') return renderMatrixView();
  if (state.nav === 'actions') return renderActionsView();
  if (state.nav === 'engine') return renderEngineView();
  if (state.nav === 'vault') return renderVaultInspector();
  return renderInstallationInspector();
}

function renderMatrixView() {
  const ov = state.overview || { devices: [], skills: [], installations: [], summary: {} };
  const s = ov.summary || {};

  // Build columns = (device, agent) pairs that exist or have skills
  const columns = [];
  for (const dev of ov.devices) {
    const devAgents = (dev.detectedAgents || []).filter(
      (a) => a.exists || ov.installations.some((i) => i.deviceId === dev.id && i.agentId === a.id)
    );
    for (const ag of devAgents) {
      columns.push({ device: dev, agent: ag });
    }
  }

  const unmanagedRows = ov.installations.filter((i) => i.status === 'unmanaged');

  return `
    <main class="inspector-pane">
      <div class="inspector-inner" style="max-width:1300px">
        <div class="inspector-header">
          <div>
            <div class="breadcrumb">FLEET CONTROL PLANE</div>
            <h1 class="inspector-title">Skills Sync Matrix</h1>
            <p class="inspector-desc">Real-time view of every Coding CLI across all your macOS and Linux machines.</p>
          </div>
          <div style="display:flex;gap:8px">
            <button class="btn" data-action="check-all-upstream">Check GitHub Upstreams</button>
            <button class="btn btn-primary" data-action="open-deploy-modal">+ Deploy Skill</button>
          </div>
        </div>

        <div class="cards-grid">
          <div class="stat-card"><div class="stat-label">Vault Skills</div><div class="stat-value">${s.totalSkills ?? 0}</div><div class="stat-sub">${s.upstreamUpdates ?? 0} upstream updates</div></div>
          <div class="stat-card"><div class="stat-label">Synced</div><div class="stat-value" style="color:var(--emerald)">${s.syncedCount ?? 0}</div><div class="stat-sub">of ${s.totalInstallations ?? 0} installations</div></div>
          <div class="stat-card"><div class="stat-label">Outdated</div><div class="stat-value" style="color:var(--sky)">${s.outdatedCount ?? 0}</div><div class="stat-sub">ready for safe update</div></div>
          <div class="stat-card"><div class="stat-label">Drifted / Unmanaged</div><div class="stat-value" style="color:var(--amber)">${(s.modifiedCount ?? 0) + (s.unmanagedCount ?? 0)}</div><div class="stat-sub">${s.modifiedCount ?? 0} modified · ${s.unmanagedCount ?? 0} unmanaged</div></div>
        </div>

        ${
          ov.skills.length === 0
            ? `<div class="empty-state">Your Vault is empty. Import a skill from GitHub, create one manually, or adopt an unmanaged local skill below.</div>`
            : columns.length === 0
              ? `<div class="empty-state">No devices connected yet. Click <strong>+ Connect Device</strong> in the left sidebar.</div>`
              : `
              <div style="overflow-x:auto">
                <table class="data-table">
                  <thead>
                    <tr>
                      <th>Vault Skill</th>
                      <th>Latest</th>
                      ${columns
                        .map(
                          (col) => `
                        <th>
                          <div>${escapeHtml(col.device.name)}</div>
                          <div style="font-size:10.5px;color:var(--text-dim);font-weight:500">${escapeHtml(col.agent.name || col.agent.id)}</div>
                        </th>`
                        )
                        .join('')}
                    </tr>
                  </thead>
                  <tbody>
                    ${ov.skills
                      .map(
                        (skill) => `
                      <tr>
                        <td>
                          <a href="#" data-jump-vault="${escapeHtml(skill.id)}" style="font-weight:600">${escapeHtml(skill.name)}</a>
                          <div class="mono" style="font-size:11px;color:var(--text-dim)">${escapeHtml(skill.id)}</div>
                        </td>
                        <td>
                          <span class="pill ${skill.hasUpstreamUpdate ? 'pill-outdated' : 'pill-neutral'}">v${skill.currentVersionNum ?? 1}</span>
                        </td>
                        ${columns
                          .map((col) => {
                            const inst = ov.installations.find(
                              (i) =>
                                i.deviceId === col.device.id &&
                                i.agentId === col.agent.id &&
                                i.vaultSkillId === skill.id
                            );
                            if (!inst) {
                              return `<td><button class="btn btn-sm" data-quick-deploy="${escapeHtml(skill.id)}|${escapeHtml(col.device.id)}|${escapeHtml(col.agent.id)}">+ Install</button></td>`;
                            }
                            const key = `${inst.deviceId}:${inst.agentId}:${inst.skillName}`;
                            return `<td><button class="matrix-cell-btn" data-jump-inst="${escapeHtml(key)}">${statusPill(inst.status)}</button></td>`;
                          })
                          .join('')}
                      </tr>`
                      )
                      .join('')}
                  </tbody>
                </table>
              </div>
            `
        }

        ${
          unmanagedRows.length > 0
            ? `
            <h3 style="margin:24px 0 10px;font-size:14px">Unmanaged Skills Discovered on Devices (${unmanagedRows.length})</h3>
            <table class="data-table">
              <thead>
                <tr><th>Skill Name</th><th>Device</th><th>Agent</th><th>Hash</th><th>Action</th></tr>
              </thead>
              <tbody>
                ${unmanagedRows
                  .map((inst) => {
                    const key = `${inst.deviceId}:${inst.agentId}:${inst.skillName}`;
                    return `
                    <tr>
                      <td style="font-weight:600">${escapeHtml(inst.skillName)}</td>
                      <td>${escapeHtml(inst.deviceName || inst.deviceId)}</td>
                      <td>${escapeHtml(inst.agentId)}</td>
                      <td class="mono" style="font-size:11.5px">${shortHash(inst.actualHash, 10)}</td>
                      <td>
                        <button class="btn btn-sm" data-jump-inst="${escapeHtml(key)}">Inspect / Adopt</button>
                      </td>
                    </tr>`;
                  })
                  .join('')}
              </tbody>
            </table>`
            : ''
        }
      </div>
    </main>
  `;
}

function renderActionsView() {
  const ov = state.overview || { actions: [], devices: [] };
  const deviceMap = new Map(ov.devices.map((d) => [d.id, d.name]));

  return `
    <main class="inspector-pane">
      <div class="inspector-inner">
        <div class="inspector-header">
          <div>
            <div class="breadcrumb">CONTROL PLANE QUEUE</div>
            <h1 class="inspector-title">Activity & Sync Actions</h1>
            <p class="inspector-desc">Commands dispatched to Mac and Linux daemons.</p>
          </div>
          <button class="btn" data-action="refresh-now">Refresh</button>
        </div>

        <div style="margin-top:16px">
          ${
            ov.actions.length === 0
              ? `<div class="empty-state">No sync actions recorded yet.</div>`
              : `
              <table class="data-table">
                <thead>
                  <tr><th>Time</th><th>Device</th><th>Agent / Skill</th><th>Action</th><th>Status</th><th>Detail</th></tr>
                </thead>
                <tbody>
                  ${ov.actions
                    .map(
                      (a) => `
                    <tr>
                      <td style="color:var(--text-dim);font-size:11.5px">${formatTimeAgo(a.createdAt)}</td>
                      <td>${escapeHtml(deviceMap.get(a.deviceId) || a.deviceId)}</td>
                      <td><span class="mono">${escapeHtml(a.agentId)} / ${escapeHtml(a.skillName)}</span></td>
                      <td><span class="pill pill-neutral">${escapeHtml(a.type)}</span></td>
                      <td>
                        <span class="pill ${
                          a.status === 'completed'
                            ? 'pill-synced'
                            : a.status === 'failed'
                              ? 'pill-missing'
                              : a.status === 'cancelled'
                                ? 'pill-neutral'
                                : 'pill-outdated'
                        }">${escapeHtml(a.status)}</span>
                      </td>
                      <td>
                        ${a.errorMessage ? `<span style="color:var(--rose)">${escapeHtml(a.errorMessage)}</span>` : ''}
                        ${
                          a.status === 'pending' || a.status === 'claimed'
                            ? `<button class="btn btn-sm" data-cancel-action="${escapeHtml(a.id)}">Cancel</button>`
                            : ''
                        }
                      </td>
                    </tr>`
                    )
                    .join('')}
                </tbody>
              </table>`
          }
        </div>
      </div>
    </main>
  `;
}

function renderEngineView() {
  const rt = state.runtimeData;
  if (!rt) {
    return `
      <main class="inspector-pane">
        <div class="empty-state">Loading Go + SQLite runtime telemetry...</div>
      </main>
    `;
  }
  if (rt.unsupported) {
    return `
      <main class="inspector-pane">
        <div class="empty-state">Engine & Quotas telemetry is available when running on the native Go + SQLite server binary.</div>
      </main>
    `;
  }

  const p = rt.pragmas || {};
  const ws = rt.writerStats || {};
  const cfg = rt.config || {};
  const accounts = rt.accounts || [];
  const routes = rt.routes || [];
  const auditLogs = state.auditData?.accessLogs || [];

  return `
    <main class="inspector-pane">
      <div class="inspector-inner" style="max-width:1300px">
        <div class="inspector-header">
          <div>
            <div class="breadcrumb">HIGH-CONCURRENCY SINGLE-BINARY ENGINE</div>
            <h1 class="inspector-title">SQLite Split-Pool &amp; Hot-Path Quotas</h1>
            <p class="inspector-desc">Native Go + statically linked SQLite (WAL, Read/Write pool split, per-account locks, 8192 group-commit queue).</p>
          </div>
          <div style="display:flex;gap:8px">
            <button class="btn" data-action="refresh-engine">Refresh Metrics</button>
            <button class="btn btn-primary" data-action="open-create-account">+ Create API Token</button>
          </div>
        </div>

        <div class="cards-grid">
          <div class="stat-card">
            <div class="stat-label">ReadDB / WriteDB Pools</div>
            <div class="stat-value">${rt.readPool?.maxOpen ?? 6} RO · ${rt.writePool?.maxOpen ?? 1} RW</div>
            <div class="stat-sub">mode=ro&amp;_query_only=${p.readQueryOnly ?? 1} · _txlock=immediate · ${rt.writeTxTotal ?? 0} tx</div>
          </div>
          <div class="stat-card">
            <div class="stat-label">SQLite PRAGMAs</div>
            <div class="stat-value">${escapeHtml((p.writeJournalMode || 'wal').toUpperCase())} · NORMAL</div>
            <div class="stat-sub">mmap=${Math.round((p.writeMmapSize || 0) / 1048576)}MB · cache=${p.writeCacheSize ?? -32000} · autockpt=${p.writeWalAutocheckpoint ?? 0}</div>
          </div>
          <div class="stat-card">
            <div class="stat-label">Async Group Commit</div>
            <div class="stat-value">${ws.queueLength ?? 0} / ${ws.queueCapacity ?? 8192}</div>
            <div class="stat-sub">${ws.flushedEvents ?? 0} events in ${ws.flushedBatches ?? 0} tx (${ws.flushIntervalMs ?? 15}ms / ${ws.batchMaxSize ?? 64} batch)</div>
          </div>
          <div class="stat-card">
            <div class="stat-label">Hot Config Snapshot</div>
            <div class="stat-value">v${cfg.version ?? 1} · ${cfg.globalRateLimitRps ?? 500} RPS</div>
            <div class="stat-sub">Cost: read=${cfg.readCostMicros ?? 1} · write=${cfg.writeCostMicros ?? 5} · upstream=${cfg.upstreamCostMicros ?? 20}</div>
          </div>
        </div>

        <h3 style="margin:22px 0 10px;font-size:14px">API Accounts, Rate Limits &amp; Pre-Deduction Quotas (${accounts.length})</h3>
        <table class="data-table">
          <thead>
            <tr><th>ID</th><th>Name</th><th>Token Prefix</th><th>Role</th><th>Quota Balance</th><th>Consumed</th><th>Rate Limit</th><th>Requests / Throttled</th><th>Actions</th></tr>
          </thead>
          <tbody>
            ${accounts
              .map(
                (a) => `
              <tr>
                <td class="mono" style="font-size:11.5px">${escapeHtml(a.id)}</td>
                <td style="font-weight:600">${escapeHtml(a.name)}</td>
                <td class="mono">${escapeHtml(a.tokenPrefix)}...</td>
                <td><span class="pill pill-neutral">${escapeHtml(a.role)}</span></td>
                <td class="mono">${a.unlimitedQuota ? '<span class="pill pill-synced">Unlimited</span>' : `${a.balanceMicros} µ`}</td>
                <td class="mono">${a.totalConsumed} µ</td>
                <td class="mono">${a.rateLimitRps} rps / ${a.rateLimitBurst} burst</td>
                <td class="mono">${a.requestCount} ok / ${a.rateLimitedCount + a.quotaRejectedCount} rej</td>
                <td>
                  ${
                    a.unlimitedQuota
                      ? ''
                      : `<button class="btn btn-sm" data-topup-account="${escapeHtml(a.id)}">+1000 µ</button>`
                  }
                  ${
                    a.id !== 'root'
                      ? `<button class="btn btn-sm btn-danger" data-delete-account="${escapeHtml(a.id)}">Delete</button>`
                      : ''
                  }
                </td>
              </tr>`
              )
              .join('')}
          </tbody>
        </table>

        ${
          routes.length > 0
            ? `
          <h3 style="margin:22px 0 10px;font-size:14px">In-Memory Route States (${routes.length})</h3>
          <table class="data-table">
            <thead>
              <tr><th>Route</th><th>State</th><th>In-Flight</th><th>Requests</th><th>Errors</th><th>Avg Latency</th></tr>
            </thead>
            <tbody>
              ${routes
                .map(
                  (r) => `
                <tr>
                  <td class="mono" style="font-size:11.5px">${escapeHtml(r.route)}</td>
                  <td><span class="pill ${r.enabled ? 'pill-synced' : 'pill-missing'}">${r.enabled ? 'Enabled' : 'Disabled'}</span></td>
                  <td class="mono">${r.inFlight}</td>
                  <td class="mono">${r.totalRequests}</td>
                  <td class="mono">${r.totalErrors}</td>
                  <td class="mono">${Number(r.avgLatencyMs || 0).toFixed(2)} ms</td>
                </tr>`
                )
                .join('')}
            </tbody>
          </table>`
            : ''
        }

        ${
          auditLogs.length > 0
            ? `
          <h3 style="margin:22px 0 10px;font-size:14px">Latest Async-Committed Access Logs (${auditLogs.length})</h3>
          <table class="data-table">
            <thead>
              <tr><th>Time</th><th>Account</th><th>Route</th><th>Status</th><th>Latency</th><th>Cost</th></tr>
            </thead>
            <tbody>
              ${auditLogs
                .slice(0, 20)
                .map(
                  (l) => `
                <tr>
                  <td style="color:var(--text-dim);font-size:11.5px">${formatTimeAgo(l.createdAt)}</td>
                  <td class="mono">${escapeHtml(l.accountId)}</td>
                  <td class="mono" style="font-size:11.5px">${escapeHtml(l.route)}</td>
                  <td><span class="pill ${l.statusCode < 400 ? 'pill-synced' : 'pill-missing'}">${l.statusCode}</span></td>
                  <td class="mono">${l.latencyUs} µs</td>
                  <td class="mono">${l.costMicros} µ</td>
                </tr>`
                )
                .join('')}
            </tbody>
          </table>`
            : ''
        }
      </div>
    </main>
  `;
}

function renderInstallationInspector() {
  const ov = state.overview || { installations: [], skills: [], devices: [] };
  const inst = ov.installations.find(
    (i) => `${i.deviceId}:${i.agentId}:${i.skillName}` === state.selectedSkillKey
  );

  if (!inst) {
    return `
      <main class="inspector-pane">
        <div class="empty-state">Select a skill from the middle list to inspect versions, drift diffs, and files.</div>
      </main>
    `;
  }

  const vaultSkill = inst.vaultSkillId ? ov.skills.find((s) => s.id === inst.vaultSkillId) : null;
  const diffData = state.diffCacheKey === state.selectedSkillKey ? state.diffData : null;
  const displayFiles =
    diffData?.localFiles && Object.keys(diffData.localFiles).length > 0
      ? diffData.localFiles
      : diffData?.targetFiles || {};

  return `
    <main class="inspector-pane">
      <div class="inspector-inner">
        <div class="inspector-header">
          <div>
            <div class="breadcrumb">${escapeHtml(inst.deviceName || inst.deviceId)} &rsaquo; ${escapeHtml(inst.agentId)}</div>
            <h1 class="inspector-title">
              ${escapeHtml(inst.skillName)}
              ${statusPill(inst.status)}
            </h1>
            <p class="inspector-desc">${escapeHtml(inst.actualMeta?.description || vaultSkill?.description || 'Agent Skill directory')}</p>
          </div>
          <div style="display:flex;gap:8px">
            <button class="btn btn-sm" data-action="toggle-ignore">${inst.ignored ? 'Unignore' : 'Ignore'}</button>
            ${vaultSkill ? `<button class="btn btn-sm" data-jump-vault="${escapeHtml(vaultSkill.id)}">Open in Vault</button>` : ''}
          </div>
        </div>

        ${renderInstallationBanner(inst, vaultSkill)}

        <div class="actions-bar">
          ${renderInstallationActionButtons(inst, vaultSkill)}
        </div>

        <div class="cards-grid">
          <div class="stat-card">
            <div class="stat-label">Local Disk Hash</div>
            <div class="stat-value">${shortHash(inst.actualHash, 12)}</div>
            <div class="stat-sub">scanned ${formatTimeAgo(inst.lastScannedAt)}</div>
          </div>
          <div class="stat-card">
            <div class="stat-label">Last Synced Base</div>
            <div class="stat-value">${shortHash(inst.baseHash, 12)}</div>
            <div class="stat-sub">${inst.baseHash ? 'Tracked in state.json' : 'Not yet tracked'}</div>
          </div>
          <div class="stat-card">
            <div class="stat-label">Vault Target</div>
            <div class="stat-value">${vaultSkill ? `v${vaultSkill.currentVersionNum} · ${shortHash(vaultSkill.currentContentHash, 8)}` : 'Unlinked'}</div>
            <div class="stat-sub">${vaultSkill ? escapeHtml(vaultSkill.sourceType) : 'Not in Vault'}</div>
          </div>
        </div>

        <div class="tabs">
          <button class="tab-btn ${state.inspectorTab === 'diff' ? 'active' : ''}" data-tab="diff">
            Diff ${diffData?.diff?.hasChanges ? `(${diffData.diff.changedCount})` : ''}
          </button>
          <button class="tab-btn ${state.inspectorTab === 'files' ? 'active' : ''}" data-tab="files">
            Files (${Object.keys(displayFiles).length})
          </button>
        </div>

        ${
          !diffData
            ? `<div class="empty-state">Loading inspection data...</div>`
            : state.inspectorTab === 'diff'
              ? renderDiffSection(diffData)
              : renderFilesBrowser(displayFiles)
        }
      </div>
    </main>
  `;
}

function renderInstallationBanner(inst, vaultSkill) {
  if (inst.status === 'modified') {
    return `
      <div class="banner warn">
        <div>
          <strong>Local drift detected.</strong> Someone or an AI modified files on this device outside Skilld.
          Auto-sync will never overwrite your local changes without confirmation.
        </div>
      </div>
    `;
  }
  if (inst.status === 'outdated') {
    return `
      <div class="banner info">
        <div>
          <strong>Vault has a newer version (v${vaultSkill?.currentVersionNum ?? '?'}).</strong>
          Local files are untouched and ready for safe update.
        </div>
      </div>
    `;
  }
  if (inst.status === 'unmanaged') {
    return `
      <div class="banner info">
        <div>
          <strong>Unmanaged local skill.</strong> Found on disk without a Skilld ledger entry. Adopt it into your Vault to sync across devices.
        </div>
      </div>
    `;
  }
  if (inst.status === 'missing') {
    return `
      <div class="banner error">
        <div>
          <strong>Directory missing on disk.</strong> Recorded in local ledger, but the skill folder was deleted outside Skilld.
        </div>
      </div>
    `;
  }
  return '';
}

function renderInstallationActionButtons(inst, vaultSkill) {
  const buttons = [];
  if (inst.status === 'outdated') {
    buttons.push(
      `<button class="btn btn-primary" data-inst-op="update">Update to Vault v${vaultSkill?.currentVersionNum ?? '?'}</button>`
    );
  }
  if (inst.status === 'modified') {
    buttons.push(
      `<button class="btn btn-amber" data-inst-op="promote">Promote Local → Vault v${(vaultSkill?.currentVersionNum ?? 0) + 1}</button>`
    );
    buttons.push(
      `<button class="btn" data-inst-op="reset">Reset to Vault v${vaultSkill?.currentVersionNum ?? '?'} (backs up local)</button>`
    );
  }
  if (inst.status === 'unmanaged') {
    buttons.push(`<button class="btn btn-primary" data-action="open-adopt-modal">Adopt into Vault</button>`);
  }
  if (inst.status === 'missing') {
    if (vaultSkill) {
      buttons.push(`<button class="btn btn-primary" data-inst-op="reset">Restore from Vault</button>`);
    }
    buttons.push(`<button class="btn btn-danger" data-inst-op="remove">Forget Ledger Entry</button>`);
  } else {
    buttons.push(`<button class="btn btn-danger" data-inst-op="remove">Remove from Agent</button>`);
  }
  return buttons.join('');
}

function renderVaultInspector() {
  if (!state.selectedVaultSkillId || !state.vaultDetail) {
    return `
      <main class="inspector-pane">
        <div class="empty-state">Select a Vault skill from the list, or add a new one.</div>
      </main>
    `;
  }

  const { skill, installations } = state.vaultDetail;
  const files = state.vaultDetailVersionFiles || skill.currentVersion?.files || {};
  const selectedVer =
    skill.versions?.find((v) => v.id === state.vaultDetailVersionId) || skill.currentVersion;

  return `
    <main class="inspector-pane">
      <div class="inspector-inner">
        <div class="inspector-header">
          <div>
            <div class="breadcrumb">VAULT LIBRARY &rsaquo; ${escapeHtml(skill.id)}</div>
            <h1 class="inspector-title">
              ${escapeHtml(skill.name)}
              <span class="pill pill-neutral">v${skill.currentVersionNum ?? 1}</span>
              ${skill.hasUpstreamUpdate ? `<span class="pill pill-outdated">Upstream Update Available</span>` : ''}
            </h1>
            <p class="inspector-desc">${escapeHtml(skill.description || 'No description')}</p>
          </div>
          <button class="btn btn-danger btn-sm" data-action="delete-vault-skill">Delete</button>
        </div>

        <div class="actions-bar">
          <button class="btn btn-primary" data-action="open-deploy-modal" data-skill-id="${escapeHtml(skill.id)}">Deploy to Devices...</button>
          <button class="btn" data-action="open-edit-skill">Edit Files (New Version)</button>
          ${
            skill.upstreamUrl
              ? `
              <button class="btn" data-action="check-single-upstream">Check Upstream</button>
              <button class="btn ${skill.hasUpstreamUpdate ? 'btn-primary' : ''}" data-action="upgrade-upstream">
                ${skill.hasUpstreamUpdate ? 'Upgrade from Upstream' : 'Pull Upstream'}
              </button>`
              : ''
          }
        </div>

        <div class="cards-grid">
          <div class="stat-card">
            <div class="stat-label">Vault Content Hash</div>
            <div class="stat-value">${shortHash(skill.currentContentHash, 12)}</div>
            <div class="stat-sub">Source: ${escapeHtml(skill.sourceType)}</div>
          </div>
          <div class="stat-card">
            <div class="stat-label">Upstream Repository</div>
            <div class="stat-value" style="font-size:12px">${
              skill.upstreamUrl
                ? `<a href="${escapeHtml(skill.upstreamUrl)}" target="_blank" rel="noreferrer">${escapeHtml(skill.upstreamUrl.replace('https://github.com/', ''))}</a>`
                : 'Manual / Adopted'
            }</div>
            <div class="stat-sub">${
              skill.upstreamLatestRev
                ? `Remote ${shortHash(skill.upstreamLatestRev, 7)} · Local ${shortHash(skill.currentSourceRev, 7)}`
                : 'No upstream linked'
            }</div>
          </div>
          <div class="stat-card">
            <div class="stat-label">Version Selector</div>
            <select id="version-select" class="field-input" style="padding:4px 8px;margin-top:2px">
              ${(skill.versions || [])
                .map(
                  (v) =>
                    `<option value="${escapeHtml(v.id)}" ${v.id === selectedVer?.id ? 'selected' : ''}>v${v.versionNum} · ${shortHash(v.contentHash, 8)} (${escapeHtml(v.note || 'snapshot')})</option>`
                )
                .join('')}
            </select>
          </div>
        </div>

        <h3 style="margin:18px 0 8px;font-size:13.5px">Fleet Installations (${installations.length})</h3>
        ${
          installations.length === 0
            ? `<div class="banner info" style="margin-top:0">Not deployed to any device yet. Click <strong>Deploy to Devices...</strong> above.</div>`
            : `
            <table class="data-table" style="margin-bottom:18px">
              <thead>
                <tr><th>Device</th><th>Agent</th><th>Status</th><th>Local Hash</th><th>Action</th></tr>
              </thead>
              <tbody>
                ${installations
                  .map((inst) => {
                    const key = `${inst.deviceId}:${inst.agentId}:${inst.skillName}`;
                    return `
                    <tr>
                      <td>${escapeHtml(inst.deviceName || inst.deviceId)}</td>
                      <td class="mono">${escapeHtml(inst.agentId)}</td>
                      <td>${statusPill(inst.status)}</td>
                      <td class="mono" style="font-size:11.5px">${shortHash(inst.actualHash, 10)}</td>
                      <td><button class="btn btn-sm" data-jump-inst="${escapeHtml(key)}">Inspect</button></td>
                    </tr>`;
                  })
                  .join('')}
              </tbody>
            </table>`
        }

        <h3 style="margin:18px 0 8px;font-size:13.5px">Files in v${selectedVer?.versionNum ?? '?'}</h3>
        ${renderFilesBrowser(files)}
      </div>
    </main>
  `;
}

function renderDiffSection(diffData) {
  const diff = diffData.diff;
  if (!diff || !diff.hasChanges) {
    return `<div class="empty-state">No file differences between local disk and Vault target.</div>`;
  }

  const label =
    diffData.mode === 'modified'
      ? `Comparing Vault v${diffData.targetVersionNum ?? '?'} (-) vs Local Disk (+)`
      : diffData.mode === 'outdated'
        ? `Comparing Local Disk (-) vs Vault v${diffData.targetVersionNum ?? '?'} (+)`
        : 'Files on Local Disk';

  return `
    <div style="margin-bottom:10px;color:var(--text-muted);font-size:12px">${escapeHtml(label)}</div>
    ${diff.files
      .filter((f) => f.status !== 'unchanged')
      .map(
        (f) => `
      <div class="diff-file-card">
        <div class="diff-file-header">
          <span>${escapeHtml(f.path)}</span>
          <span class="pill ${f.status === 'added' ? 'pill-synced' : f.status === 'removed' ? 'pill-missing' : 'pill-modified'}">${escapeHtml(f.status)}</span>
        </div>
        ${
          f.binary
            ? `<div style="padding:12px;color:var(--text-dim)">Binary file changed</div>`
            : `
            <table class="diff-table">
              ${f.hunks
                .map(
                  (h) => `
                <tr class="diff-hunk-row"><td colspan="4">${escapeHtml(h.header)}</td></tr>
                ${h.lines
                  .map(
                    (line) => `
                  <tr class="${line.type === 'add' ? 'diff-line-add' : line.type === 'del' ? 'diff-line-del' : ''}">
                    <td class="diff-ln">${line.leftLine ?? ''}</td>
                    <td class="diff-ln">${line.rightLine ?? ''}</td>
                    <td class="diff-sign">${line.type === 'add' ? '+' : line.type === 'del' ? '-' : ' '}</td>
                    <td class="diff-code">${escapeHtml(line.text)}</td>
                  </tr>`
                  )
                  .join('')}
              `
                )
                .join('')}
            </table>`
        }
      </div>`
      )
      .join('')}
  `;
}

function renderFilesBrowser(files) {
  const paths = Object.keys(files || {}).sort();
  if (paths.length === 0) {
    return `<div class="empty-state">No files to preview.</div>`;
  }
  const activePath = paths.includes(state.selectedFilePath) ? state.selectedFilePath : paths[0];
  const entry = files[activePath];
  const preview =
    entry?.encoding === 'base64'
      ? `[Binary file, base64 length: ${(entry.content || '').length}]`
      : entry?.content ?? '';

  return `
    <div class="file-browser">
      <div class="file-sidebar">
        ${paths
          .map(
            (p) =>
              `<button class="file-item ${p === activePath ? 'active' : ''}" data-select-file="${escapeHtml(p)}">${escapeHtml(p)}</button>`
          )
          .join('')}
      </div>
      <pre class="file-content">${escapeHtml(preview)}</pre>
    </div>
  `;
}

// --- Modals ---
function renderModal() {
  const m = state.modal;
  if (m.type === 'connect') return renderConnectModal();
  if (m.type === 'add-skill') return renderSkillEditorModal(false);
  if (m.type === 'edit-skill') return renderSkillEditorModal(true);
  if (m.type === 'import-github') return renderImportGitHubModal();
  if (m.type === 'deploy') return renderDeployModal();
  if (m.type === 'adopt') return renderAdoptModal();
  if (m.type === 'create-account') return renderCreateAccountModal();
  return '';
}

function renderCreateAccountModal() {
  const m = state.modal;
  if (m.createdToken) {
    return `
      <div class="modal-backdrop">
        <div class="modal-card">
          <div class="modal-header">
            <h3 class="modal-title">API Token Created</h3>
            <button class="btn btn-sm" data-action="close-modal">Close</button>
          </div>
          <div class="modal-body">
            <div class="banner warn" style="margin-top:0">
              <div>Save this token now — it is hashed with SHA-256 on the server and will not be shown again.</div>
            </div>
            <div class="code-block">${escapeHtml(m.createdToken)}</div>
          </div>
          <div class="modal-footer">
            <button class="btn btn-primary" data-action="close-modal">Done</button>
          </div>
        </div>
      </div>
    `;
  }
  return `
    <div class="modal-backdrop">
      <div class="modal-card">
        <div class="modal-header">
          <h3 class="modal-title">Create Metered API Token</h3>
          <button class="btn btn-sm" data-action="close-modal">Close</button>
        </div>
        <form id="create-account-form" class="modal-body">
          <div>
            <label class="field-label">Account Name</label>
            <input name="name" class="field-input" required placeholder="e.g. CI Deployer" />
          </div>
          <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">
            <div>
              <label class="field-label">Initial Quota Balance (µ)</label>
              <input name="balanceMicros" type="number" class="field-input mono" value="1000" min="0" />
            </div>
            <div>
              <label class="field-label">Role</label>
              <select name="role" class="field-input">
                <option value="device">device</option>
                <option value="admin">admin</option>
              </select>
            </div>
          </div>
          <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">
            <div>
              <label class="field-label">Rate Limit (RPS)</label>
              <input name="rateLimitRps" type="number" class="field-input mono" value="50" min="1" />
            </div>
            <div>
              <label class="field-label">Burst Capacity</label>
              <input name="rateLimitBurst" type="number" class="field-input mono" value="100" min="1" />
            </div>
          </div>
          <div class="modal-footer" style="padding:10px 0 0;border:none">
            <button type="button" class="btn" data-action="close-modal">Cancel</button>
            <button type="submit" class="btn btn-primary">Create Token</button>
          </div>
        </form>
      </div>
    </div>
  `;
}

function renderConnectModal() {
  const origin = window.location.origin;
  const initCmd = `npx skilld init --server ${origin} --token ${state.token} --name "$(hostname -s)"`;
  const daemonCmd = `npx skilld daemon`;
  const repoCmd = `node packages/agent/bin/skilld.mjs init --server ${origin} --token ${state.token}\nnode packages/agent/bin/skilld.mjs daemon`;

  return `
    <div class="modal-backdrop">
      <div class="modal-card">
        <div class="modal-header">
          <h3 class="modal-title">Connect Mac or Linux Machine</h3>
          <button class="btn btn-sm" data-action="close-modal">Close</button>
        </div>
        <div class="modal-body">
          <p style="margin:0;color:var(--text-muted)">
            Run this once on your macOS or Linux development machine to register it and start syncing skills:
          </p>
          <div>
            <label class="field-label">1. Initialize Device</label>
            <div class="code-block">${escapeHtml(initCmd)}</div>
          </div>
          <div>
            <label class="field-label">2. Start Background Sync Daemon (or generate launchd/systemd unit with <code>skilld service</code>)</label>
            <div class="code-block">${escapeHtml(daemonCmd)}</div>
          </div>
          <div>
            <label class="field-label">Running directly from source checkout</label>
            <div class="code-block">${escapeHtml(repoCmd)}</div>
          </div>
        </div>
        <div class="modal-footer">
          <button class="btn btn-primary" data-action="close-modal">Done</button>
        </div>
      </div>
    </div>
  `;
}

function renderSkillEditorModal(isEdit) {
  const m = state.modal;
  return `
    <div class="modal-backdrop">
      <div class="modal-card" style="max-width:740px">
        <div class="modal-header">
          <h3 class="modal-title">${isEdit ? `Edit "${escapeHtml(m.name)}" (New Version)` : 'Create Custom Skill'}</h3>
          <button class="btn btn-sm" data-action="close-modal">Close</button>
        </div>
        <form id="skill-editor-form" class="modal-body">
          ${
            !isEdit
              ? `
            <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">
              <div>
                <label class="field-label">Skill Name</label>
                <input name="name" class="field-input" required placeholder="e.g. Code Reviewer" value="${escapeHtml(m.name || '')}" />
              </div>
              <div>
                <label class="field-label">Slug ID (optional)</label>
                <input name="id" class="field-input mono" placeholder="auto-generated" value="${escapeHtml(m.id || '')}" />
              </div>
            </div>
            <div>
              <label class="field-label">Description</label>
              <input name="description" class="field-input" placeholder="Short summary of what this skill teaches the agent" value="${escapeHtml(m.description || '')}" />
            </div>`
              : `
            <div>
              <label class="field-label">Version Note</label>
              <input name="note" class="field-input" placeholder="What changed in this version?" value="Updated in web editor" />
            </div>`
          }
          <div>
            <label class="field-label">SKILL.md</label>
            <textarea name="skillMd" class="field-textarea" required>${escapeHtml(m.skillMd || '')}</textarea>
          </div>
          <div class="modal-footer" style="padding:10px 0 0;border:none">
            <button type="button" class="btn" data-action="close-modal">Cancel</button>
            <button type="submit" class="btn btn-primary">${isEdit ? 'Publish New Version' : 'Save to Vault'}</button>
          </div>
        </form>
      </div>
    </div>
  `;
}

function renderImportGitHubModal() {
  return `
    <div class="modal-backdrop">
      <div class="modal-card">
        <div class="modal-header">
          <h3 class="modal-title">Import Skill from GitHub</h3>
          <button class="btn btn-sm" data-action="close-modal">Close</button>
        </div>
        <form id="import-github-form" class="modal-body">
          <p style="margin:0;color:var(--text-muted)">
            Paste a GitHub repository or subdirectory URL containing a <code class="mono">SKILL.md</code>.
          </p>
          <div>
            <label class="field-label">GitHub URL</label>
            <input name="url" class="field-input mono" required placeholder="https://github.com/owner/repo/tree/main/skills/my-skill" />
          </div>
          <div>
            <label class="field-label">Custom Name (optional, defaults to SKILL.md frontmatter)</label>
            <input name="name" class="field-input" placeholder="Auto-detected from SKILL.md" />
          </div>
          <div class="modal-footer" style="padding:10px 0 0;border:none">
            <button type="button" class="btn" data-action="close-modal">Cancel</button>
            <button type="submit" class="btn btn-primary">Import Skill</button>
          </div>
        </form>
      </div>
    </div>
  `;
}

function renderDeployModal() {
  const ov = state.overview || { skills: [], devices: [] };
  const m = state.modal;
  const skillId = m.skillId || ov.skills[0]?.id || '';

  return `
    <div class="modal-backdrop">
      <div class="modal-card">
        <div class="modal-header">
          <h3 class="modal-title">Deploy Skill to Devices & Agents</h3>
          <button class="btn btn-sm" data-action="close-modal">Close</button>
        </div>
        <form id="deploy-form" class="modal-body">
          <div>
            <label class="field-label">Vault Skill</label>
            <select name="skillId" class="field-input">
              ${ov.skills
                .map(
                  (s) =>
                    `<option value="${escapeHtml(s.id)}" ${s.id === skillId ? 'selected' : ''}>${escapeHtml(s.name)} (v${s.currentVersionNum})</option>`
                )
                .join('')}
            </select>
          </div>
          <div>
            <label class="field-label">Target Devices & Coding CLIs</label>
            <div style="display:flex;flex-direction:column;gap:8px;max-height:300px;overflow-y:auto">
              ${ov.devices
                .map((dev) => {
                  const agents = (dev.detectedAgents || []).filter((a) => a.exists);
                  const list = agents.length ? agents : dev.detectedAgents || [];
                  return `
                    <div style="border:1px solid var(--border);border-radius:8px;padding:10px;background:var(--bg-elevated)">
                      <div style="font-weight:600;margin-bottom:6px">${escapeHtml(dev.name)} <span style="color:var(--text-dim);font-weight:400">(${escapeHtml(dev.os)})</span></div>
                      <div style="display:flex;flex-wrap:wrap;gap:10px">
                        ${list
                          .map((ag) => {
                            const val = `${dev.id}|${ag.id}`;
                            const checked = !m.preselectDeviceId || m.preselectDeviceId === dev.id;
                            return `
                              <label style="display:flex;align-items:center;gap:5px;cursor:pointer">
                                <input type="checkbox" name="target" value="${escapeHtml(val)}" ${checked && ag.exists ? 'checked' : ''} />
                                <span>${escapeHtml(ag.name || ag.id)}</span>
                              </label>`;
                          })
                          .join('')}
                      </div>
                    </div>`;
                })
                .join('')}
            </div>
          </div>
          <div class="modal-footer" style="padding:10px 0 0;border:none">
            <button type="button" class="btn" data-action="close-modal">Cancel</button>
            <button type="submit" class="btn btn-primary">Queue Deployment</button>
          </div>
        </form>
      </div>
    </div>
  `;
}

function renderAdoptModal() {
  const ov = state.overview || { skills: [] };
  const m = state.modal;
  return `
    <div class="modal-backdrop">
      <div class="modal-card">
        <div class="modal-header">
          <h3 class="modal-title">Adopt "${escapeHtml(m.skillName)}" into Vault</h3>
          <button class="btn btn-sm" data-action="close-modal">Close</button>
        </div>
        <form id="adopt-form" class="modal-body">
          <div>
            <label class="field-label">Mode</label>
            <select name="targetSkillId" class="field-input">
              <option value="">Create new Vault Skill (${escapeHtml(m.skillName)})</option>
              ${ov.skills
                .map(
                  (s) =>
                    `<option value="${escapeHtml(s.id)}">Link to existing: ${escapeHtml(s.name)} (v${s.currentVersionNum})</option>`
                )
                .join('')}
            </select>
          </div>
          <div>
            <label class="field-label">Display Name (when creating new)</label>
            <input name="name" class="field-input" value="${escapeHtml(m.displayName || m.skillName)}" />
          </div>
          <label style="display:flex;align-items:center;gap:8px;cursor:pointer">
            <input type="checkbox" name="promoteNewVersion" checked />
            <span>If linking to existing Vault skill and content differs, promote local files as new Vault version</span>
          </label>
          <div class="modal-footer" style="padding:10px 0 0;border:none">
            <button type="button" class="btn" data-action="close-modal">Cancel</button>
            <button type="submit" class="btn btn-primary">Adopt into Vault</button>
          </div>
        </form>
      </div>
    </div>
  `;
}

// --- Event Wiring ---
function bindEvents() {
  const unlockForm = document.getElementById('unlock-form');
  if (unlockForm) {
    unlockForm.onsubmit = (e) => {
      e.preventDefault();
      const input = document.getElementById('token-input');
      if (input) unlockWithToken(input.value);
    };
  }

  document.querySelectorAll('[data-nav]').forEach((el) => {
    el.onclick = () => {
      state.nav = el.dataset.nav;
      if (state.nav === 'vault' && !state.selectedVaultSkillId && state.overview?.skills?.length) {
        state.selectedVaultSkillId = state.overview.skills[0].id;
        loadVaultSkillDetail(state.selectedVaultSkillId);
      } else if (state.nav === 'engine') {
        loadEngineTelemetry();
      }
      render();
    };
  });

  document.querySelectorAll('[data-device-id]').forEach((el) => {
    if (el.dataset.action) return;
    el.onclick = () => {
      state.nav = 'device';
      state.selectedDeviceId = el.dataset.deviceId;
      const firstInst = state.overview?.installations.find((i) => i.deviceId === state.selectedDeviceId);
      state.selectedSkillKey = firstInst
        ? `${firstInst.deviceId}:${firstInst.agentId}:${firstInst.skillName}`
        : null;
      render();
      if (state.selectedSkillKey) loadDiffForSelected();
    };
  });

  document.querySelectorAll('[data-inst-key]').forEach((el) => {
    el.onclick = () => {
      state.selectedSkillKey = el.dataset.instKey;
      render();
      loadDiffForSelected();
    };
  });

  document.querySelectorAll('[data-vault-skill]').forEach((el) => {
    el.onclick = () => {
      state.selectedVaultSkillId = el.dataset.vaultSkill;
      state.vaultDetailVersionId = null;
      render();
      loadVaultSkillDetail(state.selectedVaultSkillId);
    };
  });

  document.querySelectorAll('[data-jump-inst]').forEach((el) => {
    el.onclick = (e) => {
      e.preventDefault();
      const key = el.dataset.jumpInst;
      const [deviceId] = key.split(':');
      state.nav = 'device';
      state.selectedDeviceId = deviceId;
      state.selectedSkillKey = key;
      render();
      loadDiffForSelected();
    };
  });

  document.querySelectorAll('[data-jump-vault]').forEach((el) => {
    el.onclick = (e) => {
      e.preventDefault();
      state.nav = 'vault';
      state.selectedVaultSkillId = el.dataset.jumpVault;
      state.vaultDetailVersionId = null;
      render();
      loadVaultSkillDetail(state.selectedVaultSkillId);
    };
  });

  document.querySelectorAll('[data-filter]').forEach((el) => {
    el.onclick = () => {
      state.deviceFilter = el.dataset.filter;
      render();
    };
  });

  document.querySelectorAll('[data-bind="search"]').forEach((el) => {
    el.oninput = (e) => {
      state.searchQuery = e.target.value;
      render();
      const refocused = document.querySelector('[data-bind="search"]');
      if (refocused) {
        refocused.focus();
        refocused.setSelectionRange(refocused.value.length, refocused.value.length);
      }
    };
  });

  document.querySelectorAll('[data-tab]').forEach((el) => {
    el.onclick = () => {
      state.inspectorTab = el.dataset.tab;
      render();
    };
  });

  document.querySelectorAll('[data-select-file]').forEach((el) => {
    el.onclick = () => {
      state.selectedFilePath = el.dataset.selectFile;
      render();
    };
  });

  const versionSelect = document.getElementById('version-select');
  if (versionSelect) {
    versionSelect.onchange = async (e) => {
      const verId = e.target.value;
      state.vaultDetailVersionId = verId;
      try {
        const res = await api(`/api/versions/${encodeURIComponent(verId)}`);
        state.vaultDetailVersionFiles = res.version.files;
        render();
      } catch (err) {
        showToast(err.message, true);
      }
    };
  }

  document.querySelectorAll('[data-quick-deploy]').forEach((el) => {
    el.onclick = async () => {
      const [skillId, deviceId, agentId] = el.dataset.quickDeploy.split('|');
      try {
        await api('/api/installations/deploy', {
          method: 'POST',
          body: { skillId, targets: [{ deviceId, agentId }] },
        });
        showToast(`Queued install of "${skillId}" to ${agentId}`);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    };
  });

  document.querySelectorAll('[data-inst-op]').forEach((el) => {
    el.onclick = () => handleInstallationOp(el.dataset.instOp);
  });

  document.querySelectorAll('[data-cancel-action]').forEach((el) => {
    el.onclick = async () => {
      try {
        await api(`/api/actions/${encodeURIComponent(el.dataset.cancelAction)}/cancel`, {
          method: 'POST',
          body: {},
        });
        showToast('Action cancelled');
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    };
  });

  document.querySelectorAll('[data-action]').forEach((el) => {
    el.onclick = () => handleGlobalAction(el.dataset.action, el.dataset);
  });

  document.querySelectorAll('[data-topup-account]').forEach((el) => {
    el.onclick = async () => {
      try {
        await api(`/api/accounts/${encodeURIComponent(el.dataset.topupAccount)}/topup`, {
          method: 'POST',
          body: { amountMicros: 1000 },
        });
        showToast('Credited +1000 µ in memory and ledger');
        await loadEngineTelemetry();
      } catch (err) {
        showToast(err.message, true);
      }
    };
  });

  document.querySelectorAll('[data-delete-account]').forEach((el) => {
    el.onclick = async () => {
      try {
        await api(`/api/accounts/${encodeURIComponent(el.dataset.deleteAccount)}`, {
          method: 'DELETE',
        });
        showToast('Account token revoked');
        await loadEngineTelemetry();
      } catch (err) {
        showToast(err.message, true);
      }
    };
  });

  bindModalForms();
}

async function handleInstallationOp(op) {
  if (!state.selectedSkillKey) return;
  const [deviceId, agentId, ...rest] = state.selectedSkillKey.split(':');
  const skillName = rest.join(':');
  const payload = { deviceId, agentId, skillName };

  try {
    if (op === 'update') {
      await api('/api/installations/update', { method: 'POST', body: payload });
      showToast('Queued safe update for next device sync');
    } else if (op === 'reset') {
      await api('/api/installations/reset', { method: 'POST', body: payload });
      showToast('Queued reset to Vault version (local backup will be created)');
    } else if (op === 'promote') {
      await api('/api/installations/promote', { method: 'POST', body: payload });
      showToast('Promoted local changes to new Vault version');
    } else if (op === 'remove') {
      await api('/api/installations/remove', { method: 'POST', body: payload });
      showToast('Queued removal from agent');
    }
    await refreshAll();
  } catch (err) {
    showToast(err.message, true);
  }
}

async function handleGlobalAction(action, dataset) {
  try {
    if (action === 'lock-vault') {
      lockVault();
      return;
    }
    if (action === 'refresh-now') {
      await refreshAll();
      showToast('Refreshed');
      return;
    }
    if (action === 'close-modal') {
      state.modal = null;
      render();
      return;
    }
    if (action === 'open-connect-modal') {
      state.modal = { type: 'connect' };
      render();
      return;
    }
    if (action === 'open-add-skill') {
      state.modal = {
        type: 'add-skill',
        skillMd: `---\nname: My Custom Skill\ndescription: Describe when the agent should use this skill\n---\n\n# Instructions\n\nWrite clear step-by-step guidance here.\n`,
      };
      render();
      return;
    }
    if (action === 'open-edit-skill') {
      const skill = state.vaultDetail?.skill;
      if (!skill) return;
      const skillMd = skill.currentVersion?.files?.['SKILL.md']?.content || '';
      state.modal = { type: 'edit-skill', skillId: skill.id, name: skill.name, skillMd };
      render();
      return;
    }
    if (action === 'open-import-github') {
      state.modal = { type: 'import-github' };
      render();
      return;
    }
    if (action === 'open-deploy-modal') {
      state.modal = {
        type: 'deploy',
        skillId: dataset.skillId || state.selectedVaultSkillId || null,
        preselectDeviceId: dataset.deviceId || null,
      };
      render();
      return;
    }
    if (action === 'open-adopt-modal') {
      const [deviceId, agentId, ...rest] = state.selectedSkillKey.split(':');
      const skillName = rest.join(':');
      const inst = state.overview?.installations.find(
        (i) => i.deviceId === deviceId && i.agentId === agentId && i.skillName === skillName
      );
      state.modal = {
        type: 'adopt',
        deviceId,
        agentId,
        skillName,
        displayName: inst?.actualMeta?.name || skillName,
      };
      render();
      return;
    }
    if (action === 'toggle-ignore') {
      const [deviceId, agentId, ...rest] = state.selectedSkillKey.split(':');
      const skillName = rest.join(':');
      const inst = state.overview?.installations.find(
        (i) => i.deviceId === deviceId && i.agentId === agentId && i.skillName === skillName
      );
      await api('/api/installations/ignore', {
        method: 'POST',
        body: { deviceId, agentId, skillName, ignored: !inst?.ignored },
      });
      await refreshAll();
      return;
    }
    if (action === 'check-all-upstream') {
      showToast('Checking GitHub upstreams...');
      const res = await api('/api/skills/check-all-upstream', { method: 'POST', body: {} });
      const updates = (res.results || []).filter((r) => r.hasUpstreamUpdate).length;
      showToast(`Checked ${res.results?.length ?? 0} skills (${updates} upstream update(s))`);
      await refreshAll();
      return;
    }
    if (action === 'check-single-upstream') {
      const res = await api(`/api/skills/${encodeURIComponent(state.selectedVaultSkillId)}/check-upstream`, {
        method: 'POST',
        body: {},
      });
      showToast(
        res.skill.hasUpstreamUpdate ? 'Newer commit available upstream!' : 'Already at latest upstream commit'
      );
      await refreshAll();
      return;
    }
    if (action === 'upgrade-upstream') {
      const res = await api(`/api/skills/${encodeURIComponent(state.selectedVaultSkillId)}/upgrade-upstream`, {
        method: 'POST',
        body: {},
      });
      showToast(res.upgraded ? `Upgraded to v${res.version.versionNum}` : 'Content unchanged from upstream');
      await refreshAll();
      return;
    }
    if (action === 'delete-vault-skill') {
      await api(`/api/skills/${encodeURIComponent(state.selectedVaultSkillId)}`, { method: 'DELETE' });
      state.selectedVaultSkillId = null;
      state.vaultDetail = null;
      showToast('Skill deleted from Vault');
      await refreshAll();
      return;
    }
    if (action === 'refresh-engine') {
      await loadEngineTelemetry();
      showToast('Engine metrics refreshed');
      return;
    }
    if (action === 'open-create-account') {
      state.modal = { type: 'create-account' };
      render();
    }
  } catch (err) {
    showToast(err.message, true);
  }
}

function bindModalForms() {
  const skillEditor = document.getElementById('skill-editor-form');
  if (skillEditor) {
    skillEditor.onsubmit = async (e) => {
      e.preventDefault();
      const fd = new FormData(skillEditor);
      try {
        if (state.modal.type === 'edit-skill') {
          const currentFiles = { ...(state.vaultDetail?.skill?.currentVersion?.files || {}) };
          currentFiles['SKILL.md'] = {
            encoding: 'utf8',
            content: String(fd.get('skillMd') || ''),
            mode: '0644',
          };
          const res = await api(`/api/skills/${encodeURIComponent(state.modal.skillId)}/versions`, {
            method: 'POST',
            body: { files: currentFiles, note: String(fd.get('note') || '') },
          });
          state.modal = null;
          showToast(res.created ? `Published v${res.version.versionNum}` : 'No content changes');
        } else {
          const res = await api('/api/skills', {
            method: 'POST',
            body: {
              id: String(fd.get('id') || '').trim() || undefined,
              name: String(fd.get('name') || ''),
              description: String(fd.get('description') || ''),
              markdown: String(fd.get('skillMd') || ''),
            },
          });
          state.modal = null;
          state.nav = 'vault';
          state.selectedVaultSkillId = res.skill.id;
          showToast(`Created "${res.skill.name}"`);
        }
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    };
  }

  const ghForm = document.getElementById('import-github-form');
  if (ghForm) {
    ghForm.onsubmit = async (e) => {
      e.preventDefault();
      const fd = new FormData(ghForm);
      try {
        const res = await api('/api/skills/import-github', {
          method: 'POST',
          body: {
            url: String(fd.get('url') || ''),
            name: String(fd.get('name') || '').trim() || undefined,
          },
        });
        state.modal = null;
        state.nav = 'vault';
        state.selectedVaultSkillId = res.skill.id;
        showToast(`Imported "${res.skill.name}" from GitHub`);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    };
  }

  const deployForm = document.getElementById('deploy-form');
  if (deployForm) {
    deployForm.onsubmit = async (e) => {
      e.preventDefault();
      const fd = new FormData(deployForm);
      const skillId = String(fd.get('skillId') || '');
      const targets = fd.getAll('target').map((val) => {
        const [deviceId, agentId] = String(val).split('|');
        return { deviceId, agentId };
      });
      if (targets.length === 0) {
        showToast('Select at least one target device/agent', true);
        return;
      }
      try {
        const res = await api('/api/installations/deploy', {
          method: 'POST',
          body: { skillId, targets },
        });
        state.modal = null;
        showToast(`Queued ${res.actions.length} deployment action(s)`);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    };
  }

  const adoptForm = document.getElementById('adopt-form');
  if (adoptForm) {
    adoptForm.onsubmit = async (e) => {
      e.preventDefault();
      const fd = new FormData(adoptForm);
      try {
        const res = await api('/api/installations/adopt', {
          method: 'POST',
          body: {
            deviceId: state.modal.deviceId,
            agentId: state.modal.agentId,
            skillName: state.modal.skillName,
            targetSkillId: String(fd.get('targetSkillId') || '').trim() || undefined,
            name: String(fd.get('name') || '').trim() || undefined,
            promoteNewVersion: fd.get('promoteNewVersion') === 'on',
          },
        });
        state.modal = null;
        showToast(`Adopted into Vault as "${res.skill.name}"`);
        await refreshAll();
      } catch (err) {
        showToast(err.message, true);
      }
    };
  }

  const createAccountForm = document.getElementById('create-account-form');
  if (createAccountForm) {
    createAccountForm.onsubmit = async (e) => {
      e.preventDefault();
      const fd = new FormData(createAccountForm);
      try {
        const res = await api('/api/accounts', {
          method: 'POST',
          body: {
            name: String(fd.get('name') || ''),
            role: String(fd.get('role') || 'device'),
            balanceMicros: Number(fd.get('balanceMicros') || 0),
            rateLimitRps: Number(fd.get('rateLimitRps') || 50),
            rateLimitBurst: Number(fd.get('rateLimitBurst') || 100),
            unlimitedQuota: false,
          },
        });
        state.modal = { type: 'create-account', createdToken: res.token };
        showToast(`Created API token for "${res.account.name}"`);
        await loadEngineTelemetry(true);
        render();
      } catch (err) {
        showToast(err.message, true);
      }
    };
  }
}

// --- Boot ---
if (state.token) {
  unlockWithToken(state.token);
} else {
  render();
}
