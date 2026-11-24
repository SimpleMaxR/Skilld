import crypto from 'node:crypto';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { DEFAULT_ADAPTERS, isoNow } from '../../shared/src/index.mjs';

export function resolveVaultHome(explicitHome = null) {
  if (explicitHome) return path.resolve(explicitHome);
  if (process.env.SKILLD_HOME) return path.resolve(process.env.SKILLD_HOME);
  return path.join(os.homedir(), '.skilld');
}

export function expandHomePath(inputPath, homeDir = os.homedir()) {
  if (!inputPath) return inputPath;
  if (inputPath === '~') return homeDir;
  if (inputPath.startsWith('~/') || inputPath.startsWith('~\\')) {
    return path.join(homeDir, inputPath.slice(2));
  }
  return path.resolve(inputPath);
}

export async function loadConfig(vaultHome = resolveVaultHome()) {
  const configPath = path.join(vaultHome, 'config.json');
  const raw = await fs.readFile(configPath, 'utf8');
  const parsed = JSON.parse(raw);
  return {
    pollIntervalSec: 10,
    adapters: DEFAULT_ADAPTERS,
    ...parsed,
  };
}

export async function saveConfig(config, vaultHome = resolveVaultHome()) {
  await fs.mkdir(vaultHome, { recursive: true });
  const configPath = path.join(vaultHome, 'config.json');
  const data = {
    deviceId: config.deviceId || `dev_${crypto.randomBytes(6).toString('hex')}`,
    deviceName: config.deviceName || os.hostname().replace(/\.local$/i, ''),
    serverUrl: String(config.serverUrl || '').replace(/\/+$/, ''),
    token: config.token,
    pollIntervalSec: Number(config.pollIntervalSec || 10),
    adapters: config.adapters || DEFAULT_ADAPTERS,
  };
  await fs.writeFile(configPath, `${JSON.stringify(data, null, 2)}\n`, { mode: 0o600 });
  return data;
}

export async function loadState(vaultHome = resolveVaultHome()) {
  const statePath = path.join(vaultHome, 'state.json');
  try {
    const raw = await fs.readFile(statePath, 'utf8');
    const parsed = JSON.parse(raw);
    return {
      version: 1,
      installations: parsed.installations && typeof parsed.installations === 'object' ? parsed.installations : {},
    };
  } catch (err) {
    if (err.code === 'ENOENT') {
      return { version: 1, installations: {} };
    }
    throw err;
  }
}

export async function saveState(state, vaultHome = resolveVaultHome()) {
  await fs.mkdir(vaultHome, { recursive: true });
  const statePath = path.join(vaultHome, 'state.json');
  const tmpPath = `${statePath}.tmp-${process.pid}-${Date.now()}`;
  await fs.writeFile(tmpPath, `${JSON.stringify(state, null, 2)}\n`, 'utf8');
  await fs.rename(tmpPath, statePath);
}

export function getLedgerEntry(state, agentId, skillName) {
  return state.installations?.[agentId]?.[skillName] ?? null;
}

export function setLedgerEntry(state, agentId, skillName, entry) {
  if (!state.installations) state.installations = {};
  if (!state.installations[agentId]) state.installations[agentId] = {};
  state.installations[agentId][skillName] = {
    vaultSkillId: entry.vaultSkillId ?? null,
    versionId: entry.versionId ?? null,
    baseHash: entry.baseHash ?? null,
    updatedAt: entry.updatedAt || isoNow(),
  };
}

export function deleteLedgerEntry(state, agentId, skillName) {
  if (state.installations?.[agentId]) {
    delete state.installations[agentId][skillName];
    if (Object.keys(state.installations[agentId]).length === 0) {
      delete state.installations[agentId];
    }
  }
}

export async function createBackupSnapshot({
  vaultHome = resolveVaultHome(),
  agentId,
  skillName,
  reason,
  files,
  contentHash,
  maxKeep = 30,
}) {
  const backupsDir = path.join(vaultHome, 'backups');
  await fs.mkdir(backupsDir, { recursive: true });

  const stamp = isoNow().replace(/[:.]/g, '-');
  const safeAgent = String(agentId).replace(/[^a-zA-Z0-9_-]/g, '_');
  const safeSkill = String(skillName).replace(/[^a-zA-Z0-9_-]/g, '_');
  const backupId = `${stamp}_${safeAgent}_${safeSkill}_${crypto.randomBytes(2).toString('hex')}`;
  const filePath = path.join(backupsDir, `${backupId}.json`);

  const snapshot = {
    id: backupId,
    agentId,
    skillName,
    reason,
    contentHash: contentHash ?? null,
    createdAt: isoNow(),
    files,
  };
  await fs.writeFile(filePath, `${JSON.stringify(snapshot, null, 2)}\n`, 'utf8');

  // Prune oldest backups beyond maxKeep
  const entries = (await fs.readdir(backupsDir)).filter((f) => f.endsWith('.json')).sort();
  if (entries.length > maxKeep) {
    const toDelete = entries.slice(0, entries.length - maxKeep);
    for (const old of toDelete) {
      await fs.unlink(path.join(backupsDir, old)).catch(() => {});
    }
  }

  return { backupId, path: filePath };
}

export async function listBackups(vaultHome = resolveVaultHome()) {
  const backupsDir = path.join(vaultHome, 'backups');
  try {
    const entries = (await fs.readdir(backupsDir)).filter((f) => f.endsWith('.json')).sort().reverse();
    const out = [];
    for (const file of entries) {
      try {
        const raw = await fs.readFile(path.join(backupsDir, file), 'utf8');
        const data = JSON.parse(raw);
        out.push({
          id: data.id,
          agentId: data.agentId,
          skillName: data.skillName,
          reason: data.reason,
          contentHash: data.contentHash,
          createdAt: data.createdAt,
          fileCount: Object.keys(data.files || {}).length,
        });
      } catch {
        // skip corrupted backup
      }
    }
    return out;
  } catch (err) {
    if (err.code === 'ENOENT') return [];
    throw err;
  }
}

export async function readBackup(backupId, vaultHome = resolveVaultHome()) {
  const cleanId = String(backupId).replace(/\.json$/i, '');
  if (!cleanId || cleanId.includes('/') || cleanId.includes('\\') || cleanId.includes('..')) {
    throw new Error(`Invalid backup ID: ${backupId}`);
  }
  const filePath = path.join(vaultHome, 'backups', `${cleanId}.json`);
  const raw = await fs.readFile(filePath, 'utf8');
  return JSON.parse(raw);
}
