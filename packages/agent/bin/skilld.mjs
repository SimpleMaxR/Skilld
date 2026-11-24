#!/usr/bin/env node
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { shortHash } from '../../shared/src/index.mjs';
import { restoreBackup } from '../src/applier.mjs';
import { createVaultClient } from '../src/client.mjs';
import {
  listBackups,
  loadConfig,
  loadState,
  resolveVaultHome,
  saveConfig,
} from '../src/config.mjs';
import { generateServiceConfig, runSyncCycle, startDaemon } from '../src/daemon.mjs';
import { scanDeviceSkills } from '../src/scanner.mjs';

const __filename = fileURLToPath(import.meta.url);

function parseArgs(argv) {
  const positionals = [];
  const flags = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg.startsWith('--')) {
      const eqIdx = arg.indexOf('=');
      if (eqIdx !== -1) {
        flags[arg.slice(2, eqIdx)] = arg.slice(eqIdx + 1);
      } else {
        const key = arg.slice(2);
        const next = argv[i + 1];
        if (next && !next.startsWith('--')) {
          flags[key] = next;
          i += 1;
        } else {
          flags[key] = true;
        }
      }
    } else {
      positionals.push(arg);
    }
  }
  return { command: positionals[0] || 'help', args: positionals.slice(1), flags };
}

function printHelp() {
  console.log(`Skilld Device Agent CLI

Usage:
  skilld init --server <url> --token <token> [--name <device-name>] [--interval <sec>]
  skilld status                      Show configured device & detected agents
  skilld scan [--json]               Scan local agent skills without syncing
  skilld sync                        Run one sync cycle (report + apply actions)
  skilld daemon                      Run continuous background sync + file watcher
  skilld backups                     List local pre-overwrite backup snapshots
  skilld restore <backup-id>         Restore a local backup snapshot
  skilld service [launchd|systemd]   Print macOS launchd or Linux systemd service file
`);
}

async function main() {
  const { command, args, flags } = parseArgs(process.argv.slice(2));
  const vaultHome = resolveVaultHome(flags.home || null);

  if (command === 'help' || command === '--help' || command === '-h') {
    printHelp();
    return;
  }

  if (command === 'init') {
    const serverUrl = flags.server;
    const token = flags.token;
    if (!serverUrl || !token) {
      console.error('Error: --server and --token are required');
      process.exitCode = 1;
      return;
    }

    const client = createVaultClient({ serverUrl, token });
    await client.verifyAuth();

    let existing = {};
    try {
      existing = await loadConfig(vaultHome);
    } catch {
      // first init
    }

    const saved = await saveConfig(
      {
        ...existing,
        serverUrl,
        token,
        deviceName: flags.name || existing.deviceName || os.hostname().replace(/\.local$/i, ''),
        pollIntervalSec: flags.interval ? Number(flags.interval) : existing.pollIntervalSec || 10,
      },
      vaultHome
    );

    console.log(`Initialized Skilld device "${saved.deviceName}" (${saved.deviceId})`);
    console.log(`Config saved to: ${path.join(vaultHome, 'config.json')}`);
    const res = await runSyncCycle({ config: saved, vaultHome });
    console.log(`Initial sync complete: ${res.scannedCount} skill(s) reported.`);
    return;
  }

  if (command === 'service') {
    const platformFlag = args[0];
    const platform =
      platformFlag === 'launchd' ? 'darwin' : platformFlag === 'systemd' ? 'linux' : os.platform();
    const svc = generateServiceConfig({
      platform,
      binPath: __filename,
      vaultHome,
    });
    console.log(`# Save to: ${svc.targetPath}\n# Activate: ${svc.installHint}\n`);
    console.log(svc.content);
    return;
  }

  if (command === 'backups') {
    const backups = await listBackups(vaultHome);
    if (backups.length === 0) {
      console.log('No local backups stored yet.');
      return;
    }
    for (const b of backups) {
      console.log(
        `${b.id}  ${b.agentId}/${b.skillName}  ${b.reason}  ${shortHash(b.contentHash, 10)}  (${b.createdAt})`
      );
    }
    return;
  }

  const config = await loadConfig(vaultHome);

  if (command === 'status') {
    const state = await loadState(vaultHome);
    const scan = await scanDeviceSkills({ config, state });
    console.log(`Device: ${config.deviceName} (${config.deviceId})`);
    console.log(`Server: ${config.serverUrl}`);
    console.log(`Vault Home: ${vaultHome}`);
    console.log('\nDetected Agents:');
    for (const a of scan.detectedAgents) {
      console.log(`  - ${a.id.padEnd(14)} ${a.exists ? 'FOUND  ' : 'absent '} ${a.skillsDir}`);
    }
    console.log(`\nScanned Skills: ${scan.scannedSkills.length}`);
    return;
  }

  if (command === 'scan') {
    const state = await loadState(vaultHome);
    const scan = await scanDeviceSkills({ config, state });
    if (flags.json) {
      console.log(JSON.stringify(scan, null, 2));
      return;
    }
    for (const item of scan.scannedSkills) {
      const stateTag = !item.actualHash
        ? 'missing'
        : !item.baseHash
          ? 'unmanaged'
          : item.actualHash === item.baseHash
            ? 'clean'
            : 'drifted';
      console.log(
        `[${stateTag.padEnd(9)}] ${item.agentId}/${item.skillName}  actual=${shortHash(item.actualHash, 8)}  base=${shortHash(item.baseHash, 8)}`
      );
    }
    return;
  }

  if (command === 'sync') {
    const res = await runSyncCycle({ config, vaultHome });
    console.log(
      `Sync complete: scanned=${res.scannedCount}, claimed=${res.actionsClaimed}, applied=${res.actionsApplied}, failed=${res.actionsFailed}`
    );
    return;
  }

  if (command === 'restore') {
    const backupId = args[0];
    if (!backupId) {
      console.error('Error: backup ID is required. Run "skilld backups" to list available backups.');
      process.exitCode = 1;
      return;
    }
    const res = await restoreBackup({ backupId, config, vaultHome });
    console.log(
      `Restored ${res.agentId}/${res.skillName} to ${res.targetDir} (${shortHash(res.restoredHash, 10)})`
    );
    return;
  }

  if (command === 'daemon') {
    console.log(
      `Starting Skilld daemon for "${config.deviceName}" (${config.deviceId}) -> ${config.serverUrl} (every ${config.pollIntervalSec}s)`
    );
    const handle = startDaemon({ config, vaultHome });
    const shutdown = () => {
      console.log('\nStopping Skilld daemon...');
      handle.stop();
      process.exit(0);
    };
    process.on('SIGINT', shutdown);
    process.on('SIGTERM', shutdown);
    return;
  }

  console.error(`Unknown command: ${command}`);
  printHelp();
  process.exitCode = 1;
}

main().catch((err) => {
  console.error(`Error: ${err.message}`);
  process.exitCode = 1;
});
