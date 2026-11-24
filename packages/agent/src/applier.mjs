import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {
  ACTION_TYPE,
  DEFAULT_ADAPTERS,
  computeFilesHash,
  getAdapterMap,
  isValidRelativePath,
  normalizeFileMap,
} from '../../shared/src/index.mjs';
import {
  createBackupSnapshot,
  deleteLedgerEntry,
  expandHomePath,
  getLedgerEntry,
  loadState,
  readBackup,
  resolveVaultHome,
  saveState,
  setLedgerEntry,
} from './config.mjs';
import { readSkillDir } from './scanner.mjs';

async function pathExists(p) {
  try {
    await fs.stat(p);
    return true;
  } catch {
    return false;
  }
}

function validateSkillName(skillName) {
  if (!skillName || typeof skillName !== 'string') {
    throw new Error('Invalid skillName');
  }
  if (
    skillName.includes('/') ||
    skillName.includes('\\') ||
    skillName === '.' ||
    skillName === '..'
  ) {
    throw new Error(`Unsafe skillName: "${skillName}"`);
  }
}

async function writeFilesToSkillDir(targetDir, rawFiles) {
  const files = normalizeFileMap(rawFiles);
  await fs.mkdir(targetDir, { recursive: true });

  // Remove stale files that no longer exist in target version
  if (await pathExists(targetDir)) {
    const existing = await readSkillDir(targetDir);
    for (const oldRel of Object.keys(existing.files)) {
      if (!Object.prototype.hasOwnProperty.call(files, oldRel)) {
        await fs.unlink(path.join(targetDir, ...oldRel.split('/'))).catch(() => {});
      }
    }
  }

  for (const [relPath, entry] of Object.entries(files)) {
    if (!isValidRelativePath(relPath)) {
      throw new Error(`Refusing unsafe relative file path: "${relPath}"`);
    }
    const dest = path.join(targetDir, ...relPath.split('/'));
    await fs.mkdir(path.dirname(dest), { recursive: true });
    const buf =
      entry.encoding === 'base64'
        ? Buffer.from(entry.content, 'base64')
        : Buffer.from(entry.content, 'utf8');
    await fs.writeFile(dest, buf);
    const modeNum = entry.mode === '0755' ? 0o755 : 0o644;
    await fs.chmod(dest, modeNum).catch(() => {});
  }

  return computeFilesHash(files);
}

export async function applyAction({
  action,
  config,
  vaultHome = resolveVaultHome(),
  homeDir = os.homedir(),
}) {
  validateSkillName(action.skillName);

  const adapterMap = getAdapterMap(config?.adapters || DEFAULT_ADAPTERS);
  const adapter = adapterMap.get(action.agentId);
  if (!adapter) {
    throw new Error(`Unknown agent adapter: ${action.agentId}`);
  }

  const skillsDir = expandHomePath(adapter.skillsDir, homeDir);
  const targetDir = path.join(skillsDir, action.skillName);
  const state = await loadState(vaultHome);
  const payload = action.payload || {};

  if (action.type === ACTION_TYPE.INSTALL) {
    if (!payload.files) {
      throw new Error('Install action is missing target skill files');
    }
    const exists = await pathExists(targetDir);
    if (exists && !payload.force) {
      throw new Error(
        `Target directory already exists at ${targetDir}; refusing to overwrite without force`
      );
    }
    if (exists) {
      const current = await readSkillDir(targetDir);
      await createBackupSnapshot({
        vaultHome,
        agentId: action.agentId,
        skillName: action.skillName,
        reason: 'pre-force-install',
        files: current.files,
        contentHash: current.actualHash,
      });
    }
    const writtenHash = await writeFilesToSkillDir(targetDir, payload.files);
    setLedgerEntry(state, action.agentId, action.skillName, {
      vaultSkillId: payload.vaultSkillId,
      versionId: payload.versionId,
      baseHash: writtenHash,
    });
    await saveState(state, vaultHome);
    return { applied: true, hash: writtenHash };
  }

  if (action.type === ACTION_TYPE.UPDATE) {
    if (!payload.files) {
      throw new Error('Update action is missing target skill files');
    }
    if (!(await pathExists(targetDir))) {
      throw new Error(`Cannot safe-update missing directory: ${targetDir}`);
    }
    const current = await readSkillDir(targetDir);
    const ledger = getLedgerEntry(state, action.agentId, action.skillName);

    if (payload.expectedBaseHash && current.actualHash !== payload.expectedBaseHash) {
      throw new Error(
        `Local drift detected (expected ${payload.expectedBaseHash}, found ${current.actualHash}); refusing safe update`
      );
    }
    if (ledger?.baseHash && current.actualHash !== ledger.baseHash) {
      throw new Error(
        `Local drift detected against state ledger (ledger ${ledger.baseHash}, disk ${current.actualHash}); refusing safe update`
      );
    }

    const backup = await createBackupSnapshot({
      vaultHome,
      agentId: action.agentId,
      skillName: action.skillName,
      reason: 'pre-update',
      files: current.files,
      contentHash: current.actualHash,
    });

    const writtenHash = await writeFilesToSkillDir(targetDir, payload.files);
    setLedgerEntry(state, action.agentId, action.skillName, {
      vaultSkillId: payload.vaultSkillId,
      versionId: payload.versionId,
      baseHash: writtenHash,
    });
    await saveState(state, vaultHome);
    return { applied: true, hash: writtenHash, backupId: backup.backupId };
  }

  if (action.type === ACTION_TYPE.RESET) {
    if (!payload.files) {
      throw new Error('Reset action is missing target skill files');
    }
    let backupId = null;
    if (await pathExists(targetDir)) {
      const current = await readSkillDir(targetDir);
      const backup = await createBackupSnapshot({
        vaultHome,
        agentId: action.agentId,
        skillName: action.skillName,
        reason: 'pre-reset',
        files: current.files,
        contentHash: current.actualHash,
      });
      backupId = backup.backupId;
    }

    const writtenHash = await writeFilesToSkillDir(targetDir, payload.files);
    setLedgerEntry(state, action.agentId, action.skillName, {
      vaultSkillId: payload.vaultSkillId,
      versionId: payload.versionId,
      baseHash: writtenHash,
    });
    await saveState(state, vaultHome);
    return { applied: true, hash: writtenHash, backupId };
  }

  if (action.type === ACTION_TYPE.ADOPT_CONFIRM) {
    if (!(await pathExists(targetDir))) {
      throw new Error(`Cannot adopt missing directory: ${targetDir}`);
    }
    const current = await readSkillDir(targetDir);
    if (payload.expectedHash && current.actualHash !== payload.expectedHash) {
      throw new Error(
        `Local files changed before adopt confirmation (expected ${payload.expectedHash}, found ${current.actualHash})`
      );
    }
    setLedgerEntry(state, action.agentId, action.skillName, {
      vaultSkillId: payload.vaultSkillId,
      versionId: payload.versionId,
      baseHash: current.actualHash,
    });
    await saveState(state, vaultHome);
    return { applied: true, hash: current.actualHash };
  }

  if (action.type === ACTION_TYPE.REMOVE) {
    let backupId = null;
    if (await pathExists(targetDir)) {
      const current = await readSkillDir(targetDir);
      const backup = await createBackupSnapshot({
        vaultHome,
        agentId: action.agentId,
        skillName: action.skillName,
        reason: 'pre-remove',
        files: current.files,
        contentHash: current.actualHash,
      });
      backupId = backup.backupId;
      await fs.rm(targetDir, { recursive: true, force: true });
    }
    deleteLedgerEntry(state, action.agentId, action.skillName);
    await saveState(state, vaultHome);
    return { applied: true, backupId };
  }

  if (action.type === ACTION_TYPE.FORGET_STATE) {
    deleteLedgerEntry(state, action.agentId, action.skillName);
    await saveState(state, vaultHome);
    return { applied: true };
  }

  throw new Error(`Unsupported action type: ${action.type}`);
}

export async function restoreBackup({
  backupId,
  config,
  vaultHome = resolveVaultHome(),
  homeDir = os.homedir(),
}) {
  const snapshot = await readBackup(backupId, vaultHome);
  validateSkillName(snapshot.skillName);

  const adapterMap = getAdapterMap(config?.adapters || DEFAULT_ADAPTERS);
  const adapter = adapterMap.get(snapshot.agentId);
  if (!adapter) {
    throw new Error(`Unknown agent adapter in backup: ${snapshot.agentId}`);
  }

  const skillsDir = expandHomePath(adapter.skillsDir, homeDir);
  const targetDir = path.join(skillsDir, snapshot.skillName);

  if (await pathExists(targetDir)) {
    const current = await readSkillDir(targetDir);
    await createBackupSnapshot({
      vaultHome,
      agentId: snapshot.agentId,
      skillName: snapshot.skillName,
      reason: 'pre-restore',
      files: current.files,
      contentHash: current.actualHash,
    });
    await fs.rm(targetDir, { recursive: true, force: true });
  }

  const restoredHash = await writeFilesToSkillDir(targetDir, snapshot.files);
  return {
    agentId: snapshot.agentId,
    skillName: snapshot.skillName,
    targetDir,
    restoredHash,
  };
}
