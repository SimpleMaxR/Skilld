import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {
  DEFAULT_ADAPTERS,
  computeFilesHash,
  normalizeFileMap,
  parseSkillFrontmatter,
} from '../../shared/src/index.mjs';
import { expandHomePath, getLedgerEntry } from './config.mjs';

const IGNORED_NAMES = new Set(['.git', 'node_modules', '__pycache__', '.DS_Store']);
const MAX_FILE_SIZE_BYTES = 2 * 1024 * 1024;
const textDecoderFatal = new TextDecoder('utf-8', { fatal: true });

async function pathExists(p) {
  try {
    await fs.stat(p);
    return true;
  } catch {
    return false;
  }
}

export async function detectAgents({ adapters = DEFAULT_ADAPTERS, homeDir = os.homedir() } = {}) {
  const results = [];
  for (const adapter of adapters) {
    const resolvedSkillsDir = expandHomePath(adapter.skillsDir, homeDir);
    const resolvedConfigRoot = expandHomePath(adapter.configRoot, homeDir);
    const exists = (await pathExists(resolvedSkillsDir)) || (await pathExists(resolvedConfigRoot));
    results.push({
      id: adapter.id,
      name: adapter.name,
      skillsDir: adapter.skillsDir,
      configRoot: adapter.configRoot,
      resolvedSkillsDir,
      exists,
    });
  }
  return results;
}

export async function readSkillDir(skillDir) {
  const rawFiles = {};
  const visitedRealDirs = new Set();

  async function walk(currentDir, relPrefix) {
    const realDir = await fs.realpath(currentDir);
    if (visitedRealDirs.has(realDir)) return;
    visitedRealDirs.add(realDir);

    const entries = await fs.readdir(currentDir, { withFileTypes: true });
    for (const entry of entries) {
      if (IGNORED_NAMES.has(entry.name)) continue;

      const fullPath = path.join(currentDir, entry.name);
      const relPath = relPrefix ? `${relPrefix}/${entry.name}` : entry.name;

      let stat;
      try {
        stat = await fs.stat(fullPath);
      } catch {
        continue;
      }

      if (stat.isDirectory()) {
        await walk(fullPath, relPath);
      } else if (stat.isFile()) {
        if (stat.size > MAX_FILE_SIZE_BYTES) continue;
        const buf = await fs.readFile(fullPath);
        const decoded = decodeBuffer(buf);
        rawFiles[relPath] = {
          encoding: decoded.encoding,
          content: decoded.content,
          mode: (stat.mode & 0o111) !== 0 ? '0755' : '0644',
        };
      }
    }
  }

  await walk(skillDir, '');
  const files = normalizeFileMap(rawFiles);
  const actualHash = await computeFilesHash(files);
  const skillMd = files['SKILL.md'];
  const meta =
    skillMd && skillMd.encoding === 'utf8'
      ? parseSkillFrontmatter(skillMd.content)
      : { name: '', description: '', version: '', raw: {} };

  return {
    files,
    actualHash,
    meta: {
      name: meta.name,
      description: meta.description,
      version: meta.version,
    },
  };
}

function decodeBuffer(buf) {
  const hasNullByte = buf.includes(0);
  if (!hasNullByte) {
    try {
      const content = textDecoderFatal.decode(buf);
      return { encoding: 'utf8', content };
    } catch {
      // Non-UTF8 falls back to base64
    }
  }
  return { encoding: 'base64', content: buf.toString('base64') };
}

export async function scanDeviceSkills({ config, state, homeDir = os.homedir() }) {
  const adapters = config?.adapters || DEFAULT_ADAPTERS;
  const detectedAgents = await detectAgents({ adapters, homeDir });
  const scannedSkills = [];
  const seenKeys = new Set();

  for (const agent of detectedAgents) {
    if (!(await pathExists(agent.resolvedSkillsDir))) continue;

    let entries;
    try {
      entries = await fs.readdir(agent.resolvedSkillsDir, { withFileTypes: true });
    } catch {
      continue;
    }

    for (const entry of entries) {
      if (entry.name.startsWith('.') || IGNORED_NAMES.has(entry.name)) continue;
      const skillDir = path.join(agent.resolvedSkillsDir, entry.name);

      let stat;
      try {
        stat = await fs.stat(skillDir);
      } catch {
        continue;
      }
      if (!stat.isDirectory()) continue;

      const ledger = getLedgerEntry(state, agent.id, entry.name);
      const { files, actualHash, meta } = await readSkillDir(skillDir);

      seenKeys.add(`${agent.id}:${entry.name}`);
      scannedSkills.push({
        agentId: agent.id,
        skillName: entry.name,
        actualHash,
        baseHash: ledger?.baseHash ?? null,
        vaultSkillId: ledger?.vaultSkillId ?? null,
        meta,
        files,
      });
    }
  }

  // Emit `missing` records for ledger entries whose folder disappeared on disk
  for (const [agentId, skillsMap] of Object.entries(state?.installations || {})) {
    for (const [skillName, entry] of Object.entries(skillsMap || {})) {
      const key = `${agentId}:${skillName}`;
      if (seenKeys.has(key)) continue;
      scannedSkills.push({
        agentId,
        skillName,
        actualHash: null,
        baseHash: entry.baseHash ?? null,
        vaultSkillId: entry.vaultSkillId ?? null,
        meta: null,
        files: null,
      });
    }
  }

  return {
    detectedAgents: detectedAgents.map((a) => ({
      id: a.id,
      name: a.name,
      skillsDir: a.skillsDir,
      exists: a.exists,
    })),
    scannedSkills,
  };
}
