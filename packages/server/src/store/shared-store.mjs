import {
  ACTION_STATUS,
  ACTION_TYPE,
  computeInstallationStatus,
  generateId,
  isoNow,
} from '../../../shared/src/index.mjs';
import { SCHEMA_STATEMENTS } from './schema.mjs';

export function normalizeParams(params) {
  return params.map((v) => (v === undefined ? null : v));
}

function parseJsonSafe(str, fallback) {
  if (!str) return fallback;
  try {
    return JSON.parse(str);
  } catch {
    return fallback;
  }
}

function mapVersionRow(row, includeFiles = false) {
  if (!row) return null;
  const out = {
    id: row.id,
    skillId: row.skill_id,
    versionNum: Number(row.version_num),
    contentHash: row.content_hash,
    sourceRev: row.source_rev ?? null,
    note: row.note ?? '',
    createdAt: row.created_at,
  };
  if (includeFiles) {
    out.files = parseJsonSafe(row.files_json, {});
  }
  return out;
}

function mapSkillRow(row) {
  if (!row) return null;
  const currentSourceRev = row.current_source_rev ?? null;
  const upstreamLatestRev = row.upstream_latest_rev ?? null;
  const hasUpstreamUpdate = Boolean(
    upstreamLatestRev && currentSourceRev && upstreamLatestRev !== currentSourceRev
  );
  return {
    id: row.id,
    name: row.name,
    description: row.description ?? '',
    sourceType: row.source_type,
    upstreamUrl: row.upstream_url ?? null,
    upstreamRef: row.upstream_ref ?? null,
    upstreamSubpath: row.upstream_subpath ?? null,
    upstreamLatestRev,
    upstreamLatestMessage: row.upstream_latest_message ?? null,
    upstreamCheckedAt: row.upstream_checked_at ?? null,
    currentVersionId: row.current_version_id ?? null,
    currentVersionNum:
      row.current_version_num !== undefined && row.current_version_num !== null
        ? Number(row.current_version_num)
        : null,
    currentContentHash: row.current_content_hash ?? null,
    currentSourceRev,
    hasUpstreamUpdate,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

function mapDeviceRow(row) {
  if (!row) return null;
  return {
    id: row.id,
    name: row.name,
    os: row.os,
    arch: row.arch,
    hostname: row.hostname ?? null,
    detectedAgents: parseJsonSafe(row.detected_agents_json, []),
    lastSeenAt: row.last_seen_at,
    createdAt: row.created_at,
  };
}

function mapInstallationRow(row, includeFiles = false) {
  if (!row) return null;
  const out = {
    id: row.id,
    deviceId: row.device_id,
    deviceName: row.device_name ?? null,
    deviceOs: row.device_os ?? null,
    agentId: row.agent_id,
    skillName: row.skill_name,
    vaultSkillId: row.vault_skill_id ?? null,
    vaultSkillName: row.vault_skill_name ?? null,
    desiredVersionId: row.desired_version_id ?? null,
    desiredVersionNum:
      row.desired_version_num !== undefined && row.desired_version_num !== null
        ? Number(row.desired_version_num)
        : null,
    desiredHash: row.desired_hash ?? null,
    baseHash: row.base_hash ?? null,
    actualHash: row.actual_hash ?? null,
    status: row.status,
    ignored: Boolean(row.ignored),
    actualMeta: parseJsonSafe(row.actual_meta_json, null),
    hasActualFiles: Boolean(row.actual_files_json),
    lastScannedAt: row.last_scanned_at,
  };
  if (includeFiles) {
    out.actualFiles = parseJsonSafe(row.actual_files_json, null);
  }
  return out;
}

function mapActionRow(row) {
  if (!row) return null;
  return {
    id: row.id,
    deviceId: row.device_id,
    agentId: row.agent_id,
    skillName: row.skill_name,
    type: row.type,
    payload: parseJsonSafe(row.payload_json, {}),
    status: row.status,
    errorMessage: row.error_message ?? null,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

export function createSqlStore(adapter) {
  async function ensureSchema() {
    for (const stmt of SCHEMA_STATEMENTS) {
      await adapter.run(stmt, []);
    }
  }

  async function listSkills() {
    const rows = await adapter.all(
      `SELECT s.*,
              sv.version_num AS current_version_num,
              sv.content_hash AS current_content_hash,
              sv.source_rev AS current_source_rev
       FROM skills s
       LEFT JOIN skill_versions sv ON sv.id = s.current_version_id
       ORDER BY s.updated_at DESC, s.name ASC`,
      []
    );
    return rows.map(mapSkillRow);
  }

  async function getSkill(skillId, { includeVersions = true, includeCurrentFiles = true } = {}) {
    const row = await adapter.first(
      `SELECT s.*,
              sv.version_num AS current_version_num,
              sv.content_hash AS current_content_hash,
              sv.source_rev AS current_source_rev
       FROM skills s
       LEFT JOIN skill_versions sv ON sv.id = s.current_version_id
       WHERE s.id = ?`,
      [skillId]
    );
    if (!row) return null;
    const skill = mapSkillRow(row);

    if (skill.currentVersionId) {
      skill.currentVersion = await getVersion(skill.currentVersionId, {
        includeFiles: includeCurrentFiles,
      });
    } else {
      skill.currentVersion = null;
    }

    if (includeVersions) {
      skill.versions = await listSkillVersions(skillId, { includeFiles: false });
    }

    return skill;
  }

  async function listSkillVersions(skillId, { includeFiles = false } = {}) {
    const rows = await adapter.all(
      `SELECT * FROM skill_versions WHERE skill_id = ? ORDER BY version_num DESC`,
      [skillId]
    );
    return rows.map((r) => mapVersionRow(r, includeFiles));
  }

  async function getVersion(versionId, { includeFiles = true } = {}) {
    const row = await adapter.first(`SELECT * FROM skill_versions WHERE id = ?`, [versionId]);
    return mapVersionRow(row, includeFiles);
  }

  async function findVersionByHash(skillId, contentHash) {
    if (!skillId || !contentHash) return null;
    const row = await adapter.first(
      `SELECT * FROM skill_versions WHERE skill_id = ? AND content_hash = ? ORDER BY version_num DESC LIMIT 1`,
      [skillId, contentHash]
    );
    return mapVersionRow(row, true);
  }

  async function createSkillWithVersion({
    id,
    name,
    description = '',
    sourceType = 'custom',
    upstreamUrl = null,
    upstreamRef = null,
    upstreamSubpath = null,
    upstreamLatestRev = null,
    upstreamLatestMessage = null,
    upstreamCheckedAt = null,
    sourceRev = null,
    note = 'Initial version',
    files,
    contentHash,
  }) {
    const now = isoNow();
    const versionId = generateId('ver');
    const filesJson = JSON.stringify(files);

    await adapter.batch([
      {
        sql: `INSERT INTO skills (
                id, name, description, source_type,
                upstream_url, upstream_ref, upstream_subpath,
                upstream_latest_rev, upstream_latest_message, upstream_checked_at,
                current_version_id, created_at, updated_at
              ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
        params: [
          id,
          name,
          description,
          sourceType,
          upstreamUrl,
          upstreamRef,
          upstreamSubpath,
          upstreamLatestRev,
          upstreamLatestMessage,
          upstreamCheckedAt,
          versionId,
          now,
          now,
        ],
      },
      {
        sql: `INSERT INTO skill_versions (
                id, skill_id, version_num, content_hash, source_rev, note, files_json, created_at
              ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
        params: [versionId, id, 1, contentHash, sourceRev, note, filesJson, now],
      },
    ]);

    return getSkill(id);
  }

  async function recomputeInstallationsForSkill(skillId, desiredVersionId, desiredHash) {
    const rows = await adapter.all(
      `SELECT * FROM installations WHERE vault_skill_id = ?`,
      [skillId]
    );
    if (rows.length === 0) return;

    const stmts = [];
    for (const row of rows) {
      const nextStatus = computeInstallationStatus({
        baseHash: row.base_hash,
        actualHash: row.actual_hash,
        desiredHash,
        isManaged: true,
      });
      stmts.push({
        sql: `UPDATE installations SET desired_version_id = ?, status = ? WHERE id = ?`,
        params: [desiredVersionId, nextStatus, row.id],
      });
    }
    await adapter.batch(stmts);
  }

  async function addSkillVersion(skillId, { files, contentHash, sourceRev = null, note = '' }) {
    const existing = await getSkill(skillId, { includeVersions: false, includeCurrentFiles: false });
    if (!existing) return null;

    const maxRow = await adapter.first(
      `SELECT COALESCE(MAX(version_num), 0) AS max_num FROM skill_versions WHERE skill_id = ?`,
      [skillId]
    );
    const nextNum = Number(maxRow?.max_num ?? 0) + 1;
    const versionId = generateId('ver');
    const now = isoNow();

    await adapter.batch([
      {
        sql: `INSERT INTO skill_versions (
                id, skill_id, version_num, content_hash, source_rev, note, files_json, created_at
              ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
        params: [versionId, skillId, nextNum, contentHash, sourceRev, note, JSON.stringify(files), now],
      },
      {
        sql: `UPDATE skills SET current_version_id = ?, updated_at = ? WHERE id = ?`,
        params: [versionId, now, skillId],
      },
    ]);

    await recomputeInstallationsForSkill(skillId, versionId, contentHash);
    return getVersion(versionId, { includeFiles: true });
  }

  async function updateSkillMeta(skillId, { name, description, upstreamUrl, upstreamRef, upstreamSubpath }) {
    const existing = await getSkill(skillId, { includeVersions: false, includeCurrentFiles: false });
    if (!existing) return null;
    const now = isoNow();
    await adapter.run(
      `UPDATE skills
       SET name = ?, description = ?, upstream_url = ?, upstream_ref = ?, upstream_subpath = ?, updated_at = ?
       WHERE id = ?`,
      [
        name ?? existing.name,
        description ?? existing.description,
        upstreamUrl !== undefined ? upstreamUrl : existing.upstreamUrl,
        upstreamRef !== undefined ? upstreamRef : existing.upstreamRef,
        upstreamSubpath !== undefined ? upstreamSubpath : existing.upstreamSubpath,
        now,
        skillId,
      ]
    );
    return getSkill(skillId);
  }

  async function updateSkillUpstream(
    skillId,
    { upstreamLatestRev, upstreamLatestMessage, upstreamCheckedAt }
  ) {
    await adapter.run(
      `UPDATE skills
       SET upstream_latest_rev = ?, upstream_latest_message = ?, upstream_checked_at = ?
       WHERE id = ?`,
      [upstreamLatestRev, upstreamLatestMessage, upstreamCheckedAt ?? isoNow(), skillId]
    );
    return getSkill(skillId);
  }

  async function deleteSkill(skillId) {
    const existing = await getSkill(skillId, { includeVersions: false, includeCurrentFiles: false });
    if (!existing) return false;

    const affected = await adapter.all(
      `SELECT * FROM installations WHERE vault_skill_id = ?`,
      [skillId]
    );
    const stmts = [];
    for (const inst of affected) {
      if (inst.actual_hash) {
        stmts.push({
          sql: `UPDATE installations
                SET vault_skill_id = NULL, desired_version_id = NULL, base_hash = NULL, status = 'unmanaged'
                WHERE id = ?`,
          params: [inst.id],
        });
      } else {
        stmts.push({
          sql: `DELETE FROM installations WHERE id = ?`,
          params: [inst.id],
        });
      }
    }
    stmts.push({ sql: `DELETE FROM skills WHERE id = ?`, params: [skillId] });
    await adapter.batch(stmts);
    return true;
  }

  async function upsertDevice({ id, name, os, arch, hostname = null, detectedAgents = [] }) {
    const now = isoNow();
    const agentsJson = JSON.stringify(detectedAgents);
    await adapter.run(
      `INSERT INTO devices (id, name, os, arch, hostname, detected_agents_json, last_seen_at, created_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET
         name = excluded.name,
         os = excluded.os,
         arch = excluded.arch,
         hostname = excluded.hostname,
         detected_agents_json = excluded.detected_agents_json,
         last_seen_at = excluded.last_seen_at`,
      [id, name, os, arch, hostname, agentsJson, now, now]
    );
    return getDevice(id);
  }

  async function listDevices() {
    const rows = await adapter.all(`SELECT * FROM devices ORDER BY last_seen_at DESC, name ASC`, []);
    return rows.map(mapDeviceRow);
  }

  async function getDevice(deviceId) {
    const row = await adapter.first(`SELECT * FROM devices WHERE id = ?`, [deviceId]);
    return mapDeviceRow(row);
  }

  async function deleteDevice(deviceId) {
    const existing = await getDevice(deviceId);
    if (!existing) return false;
    await adapter.run(`DELETE FROM devices WHERE id = ?`, [deviceId]);
    return true;
  }

  async function replaceDeviceInstallations(deviceId, scannedSkills) {
    const now = isoNow();
    const existingRows = await adapter.all(
      `SELECT * FROM installations WHERE device_id = ?`,
      [deviceId]
    );
    const existingByKey = new Map();
    for (const row of existingRows) {
      existingByKey.set(`${row.agent_id}:${row.skill_name}`, row);
    }

    const skillRows = await adapter.all(
      `SELECT s.id, s.current_version_id, sv.content_hash AS current_content_hash
       FROM skills s
       LEFT JOIN skill_versions sv ON sv.id = s.current_version_id`,
      []
    );
    const skillsById = new Map();
    for (const row of skillRows) {
      skillsById.set(row.id, row);
    }

    const activeAdoptRows = await adapter.all(
      `SELECT agent_id, skill_name, payload_json
       FROM actions
       WHERE device_id = ? AND type = ? AND status IN (?, ?)`,
      [deviceId, ACTION_TYPE.ADOPT_CONFIRM, ACTION_STATUS.PENDING, ACTION_STATUS.CLAIMED]
    );
    const activeAdoptByKey = new Map();
    for (const act of activeAdoptRows) {
      activeAdoptByKey.set(`${act.agent_id}:${act.skill_name}`, parseJsonSafe(act.payload_json, {}));
    }

    const seenKeys = new Set();
    const stmts = [];

    for (const item of scannedSkills) {
      const key = `${item.agentId}:${item.skillName}`;
      seenKeys.add(key);
      const prev = existingByKey.get(key);

      let vaultSkillId = null;
      if (item.vaultSkillId && skillsById.has(item.vaultSkillId)) {
        vaultSkillId = item.vaultSkillId;
      } else if (prev?.vault_skill_id && skillsById.has(prev.vault_skill_id)) {
        vaultSkillId = prev.vault_skill_id;
      }

      const skill = vaultSkillId ? skillsById.get(vaultSkillId) : null;
      const desiredVersionId = skill ? skill.current_version_id : null;
      const desiredHash = skill ? skill.current_content_hash : null;

      let baseHash = item.baseHash ?? null;
      const actualHash = item.actualHash ?? null;

      // Avoid brief status flicker when adopt_confirm is in-flight for matching actualHash
      const pendingAdopt = activeAdoptByKey.get(key);
      if (
        pendingAdopt &&
        actualHash &&
        pendingAdopt.expectedHash === actualHash &&
        pendingAdopt.vaultSkillId &&
        skillsById.has(pendingAdopt.vaultSkillId)
      ) {
        vaultSkillId = pendingAdopt.vaultSkillId;
        baseHash = actualHash;
      }

      const isManaged = Boolean(vaultSkillId || baseHash);
      const status = computeInstallationStatus({
        baseHash,
        actualHash,
        desiredHash,
        isManaged,
      });

      let actualFilesJson = null;
      if (status === 'unmanaged' || status === 'modified') {
        if (item.files) {
          actualFilesJson = JSON.stringify(item.files);
        } else if (prev?.actual_files_json) {
          actualFilesJson = prev.actual_files_json;
        }
      }

      const rowId = `${deviceId}:${item.agentId}:${item.skillName}`;
      const ignored = prev ? Number(prev.ignored) : 0;
      const actualMetaJson = item.meta ? JSON.stringify(item.meta) : prev?.actual_meta_json ?? null;

      stmts.push({
        sql: `INSERT INTO installations (
                id, device_id, agent_id, skill_name,
                vault_skill_id, desired_version_id, base_hash, actual_hash,
                status, ignored, actual_meta_json, actual_files_json, last_scanned_at
              ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
              ON CONFLICT(device_id, agent_id, skill_name) DO UPDATE SET
                vault_skill_id = excluded.vault_skill_id,
                desired_version_id = excluded.desired_version_id,
                base_hash = excluded.base_hash,
                actual_hash = excluded.actual_hash,
                status = excluded.status,
                actual_meta_json = excluded.actual_meta_json,
                actual_files_json = excluded.actual_files_json,
                last_scanned_at = excluded.last_scanned_at`,
        params: [
          rowId,
          deviceId,
          item.agentId,
          item.skillName,
          vaultSkillId,
          desiredVersionId,
          baseHash,
          actualHash,
          status,
          ignored,
          actualMetaJson,
          actualFilesJson,
          now,
        ],
      });
    }

    for (const prev of existingRows) {
      const key = `${prev.agent_id}:${prev.skill_name}`;
      if (!seenKeys.has(key)) {
        stmts.push({
          sql: `DELETE FROM installations WHERE id = ?`,
          params: [prev.id],
        });
      }
    }

    if (stmts.length > 0) {
      await adapter.batch(stmts);
    }
  }

  async function listInstallations({ deviceId = null, skillId = null } = {}) {
    const conditions = [];
    const params = [];
    if (deviceId) {
      conditions.push(`i.device_id = ?`);
      params.push(deviceId);
    }
    if (skillId) {
      conditions.push(`i.vault_skill_id = ?`);
      params.push(skillId);
    }
    const whereClause = conditions.length > 0 ? `WHERE ${conditions.join(' AND ')}` : '';
    const rows = await adapter.all(
      `SELECT i.*,
              d.name AS device_name,
              d.os AS device_os,
              s.name AS vault_skill_name,
              sv.version_num AS desired_version_num,
              sv.content_hash AS desired_hash
       FROM installations i
       LEFT JOIN devices d ON d.id = i.device_id
       LEFT JOIN skills s ON s.id = i.vault_skill_id
       LEFT JOIN skill_versions sv ON sv.id = i.desired_version_id
       ${whereClause}
       ORDER BY i.device_id ASC, i.agent_id ASC, i.skill_name ASC`,
      params
    );
    return rows.map((r) => mapInstallationRow(r, false));
  }

  async function getInstallation(deviceId, agentId, skillName, { includeFiles = true } = {}) {
    const row = await adapter.first(
      `SELECT i.*,
              d.name AS device_name,
              d.os AS device_os,
              s.name AS vault_skill_name,
              sv.version_num AS desired_version_num,
              sv.content_hash AS desired_hash
       FROM installations i
       LEFT JOIN devices d ON d.id = i.device_id
       LEFT JOIN skills s ON s.id = i.vault_skill_id
       LEFT JOIN skill_versions sv ON sv.id = i.desired_version_id
       WHERE i.device_id = ? AND i.agent_id = ? AND i.skill_name = ?`,
      [deviceId, agentId, skillName]
    );
    return mapInstallationRow(row, includeFiles);
  }

  async function updateInstallation(deviceId, agentId, skillName, patch) {
    const existing = await getInstallation(deviceId, agentId, skillName, { includeFiles: true });
    if (!existing) return null;

    const nextVaultSkillId =
      patch.vaultSkillId !== undefined ? patch.vaultSkillId : existing.vaultSkillId;
    const nextDesiredVersionId =
      patch.desiredVersionId !== undefined ? patch.desiredVersionId : existing.desiredVersionId;
    const nextBaseHash = patch.baseHash !== undefined ? patch.baseHash : existing.baseHash;
    const nextStatus = patch.status !== undefined ? patch.status : existing.status;
    const nextIgnored = patch.ignored !== undefined ? (patch.ignored ? 1 : 0) : existing.ignored ? 1 : 0;
    const nextActualFilesJson =
      patch.clearActualFiles
        ? null
        : patch.actualFiles !== undefined
          ? patch.actualFiles
            ? JSON.stringify(patch.actualFiles)
            : null
          : existing.actualFiles
            ? JSON.stringify(existing.actualFiles)
            : null;

    await adapter.run(
      `UPDATE installations
       SET vault_skill_id = ?, desired_version_id = ?, base_hash = ?, status = ?, ignored = ?, actual_files_json = ?
       WHERE device_id = ? AND agent_id = ? AND skill_name = ?`,
      [
        nextVaultSkillId,
        nextDesiredVersionId,
        nextBaseHash,
        nextStatus,
        nextIgnored,
        nextActualFilesJson,
        deviceId,
        agentId,
        skillName,
      ]
    );

    return getInstallation(deviceId, agentId, skillName, { includeFiles: true });
  }

  async function deleteInstallation(deviceId, agentId, skillName) {
    await adapter.run(
      `DELETE FROM installations WHERE device_id = ? AND agent_id = ? AND skill_name = ?`,
      [deviceId, agentId, skillName]
    );
  }

  async function createAction({ deviceId, agentId, skillName, type, payload = {} }) {
    const id = generateId('act');
    const now = isoNow();
    await adapter.run(
      `INSERT INTO actions (
        id, device_id, agent_id, skill_name, type, payload_json, status, error_message, created_at, updated_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      [
        id,
        deviceId,
        agentId,
        skillName,
        type,
        JSON.stringify(payload),
        ACTION_STATUS.PENDING,
        null,
        now,
        now,
      ]
    );
    return getAction(id);
  }

  async function getAction(actionId) {
    const row = await adapter.first(`SELECT * FROM actions WHERE id = ?`, [actionId]);
    return mapActionRow(row);
  }

  async function listActions({ deviceId = null, status = null, limit = 100 } = {}) {
    const conditions = [];
    const params = [];
    if (deviceId) {
      conditions.push(`device_id = ?`);
      params.push(deviceId);
    }
    if (status) {
      conditions.push(`status = ?`);
      params.push(status);
    }
    const whereClause = conditions.length > 0 ? `WHERE ${conditions.join(' AND ')}` : '';
    params.push(limit);
    const rows = await adapter.all(
      `SELECT * FROM actions ${whereClause} ORDER BY created_at DESC LIMIT ?`,
      params
    );
    return rows.map(mapActionRow);
  }

  async function countPendingActions(deviceId) {
    const row = await adapter.first(
      `SELECT COUNT(*) AS cnt FROM actions WHERE device_id = ? AND status = ?`,
      [deviceId, ACTION_STATUS.PENDING]
    );
    return Number(row?.cnt ?? 0);
  }

  async function claimPendingActions(deviceId) {
    const rows = await adapter.all(
      `SELECT * FROM actions WHERE device_id = ? AND status = ? ORDER BY created_at ASC`,
      [deviceId, ACTION_STATUS.PENDING]
    );
    if (rows.length === 0) return [];

    const now = isoNow();
    const stmts = rows.map((r) => ({
      sql: `UPDATE actions SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
      params: [ACTION_STATUS.CLAIMED, now, r.id, ACTION_STATUS.PENDING],
    }));
    await adapter.batch(stmts);

    const actions = [];
    for (const row of rows) {
      const action = mapActionRow({ ...row, status: ACTION_STATUS.CLAIMED, updated_at: now });
      if (
        action.type === ACTION_TYPE.INSTALL ||
        action.type === ACTION_TYPE.UPDATE ||
        action.type === ACTION_TYPE.RESET
      ) {
        let versionId = action.payload.versionId;
        if (!versionId && action.payload.vaultSkillId) {
          const sk = await getSkill(action.payload.vaultSkillId, {
            includeVersions: false,
            includeCurrentFiles: false,
          });
          versionId = sk?.currentVersionId;
        }
        if (versionId) {
          const ver = await getVersion(versionId, { includeFiles: true });
          if (ver) {
            action.payload = {
              ...action.payload,
              versionId: ver.id,
              versionNum: ver.versionNum,
              contentHash: ver.contentHash,
              files: ver.files,
            };
          }
        }
      }
      actions.push(action);
    }
    return actions;
  }

  async function completeAction(actionId, { status, errorMessage = null }) {
    const existing = await getAction(actionId);
    if (!existing) return null;
    const nextStatus =
      status === ACTION_STATUS.COMPLETED ? ACTION_STATUS.COMPLETED : ACTION_STATUS.FAILED;
    const now = isoNow();
    await adapter.run(
      `UPDATE actions SET status = ?, error_message = ?, updated_at = ? WHERE id = ?`,
      [nextStatus, errorMessage, now, actionId]
    );
    return getAction(actionId);
  }

  async function cancelAction(actionId) {
    const existing = await getAction(actionId);
    if (!existing) return null;
    if (existing.status !== ACTION_STATUS.PENDING && existing.status !== ACTION_STATUS.CLAIMED) {
      return existing;
    }
    const now = isoNow();
    await adapter.run(
      `UPDATE actions SET status = ?, updated_at = ? WHERE id = ?`,
      [ACTION_STATUS.CANCELLED, now, actionId]
    );
    return getAction(actionId);
  }

  return {
    ensureSchema,
    listSkills,
    getSkill,
    listSkillVersions,
    getVersion,
    findVersionByHash,
    createSkillWithVersion,
    addSkillVersion,
    updateSkillMeta,
    updateSkillUpstream,
    deleteSkill,
    upsertDevice,
    listDevices,
    getDevice,
    deleteDevice,
    replaceDeviceInstallations,
    listInstallations,
    getInstallation,
    updateInstallation,
    deleteInstallation,
    createAction,
    getAction,
    listActions,
    countPendingActions,
    claimPendingActions,
    completeAction,
    cancelAction,
  };
}
