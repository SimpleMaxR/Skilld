import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { ACTION_STATUS, DEFAULT_ADAPTERS } from '../../shared/src/index.mjs';
import { applyAction } from './applier.mjs';
import { createVaultClient } from './client.mjs';
import { expandHomePath, loadState, resolveVaultHome } from './config.mjs';
import { scanDeviceSkills } from './scanner.mjs';

function buildWireReport({ config, scanResult }) {
  const scannedSkills = scanResult.scannedSkills.map((item) => {
    const needsFiles =
      item.actualHash !== null && (!item.baseHash || item.actualHash !== item.baseHash);
    return {
      agentId: item.agentId,
      skillName: item.skillName,
      actualHash: item.actualHash,
      baseHash: item.baseHash,
      vaultSkillId: item.vaultSkillId,
      meta: item.meta,
      ...(needsFiles ? { files: item.files } : {}),
    };
  });

  return {
    device: {
      id: config.deviceId,
      name: config.deviceName,
      os: os.platform(),
      arch: os.arch(),
      hostname: os.hostname(),
    },
    detectedAgents: scanResult.detectedAgents,
    scannedSkills,
  };
}

export async function runSyncCycle({
  config,
  vaultHome = resolveVaultHome(),
  homeDir = os.homedir(),
  logger = console,
}) {
  const client = createVaultClient({ serverUrl: config.serverUrl, token: config.token });
  const state = await loadState(vaultHome);

  const initialScan = await scanDeviceSkills({ config, state, homeDir });
  const reportRes = await client.reportDevice(buildWireReport({ config, scanResult: initialScan }));

  const { actions = [] } = await client.claimActions(config.deviceId);
  let actionsApplied = 0;
  let actionsFailed = 0;

  for (const action of actions) {
    try {
      await applyAction({ action, config, vaultHome, homeDir });
      await client.completeAction(action.id, { status: ACTION_STATUS.COMPLETED });
      actionsApplied += 1;
      logger.log?.(`[skillvault] Applied ${action.type} for ${action.agentId}/${action.skillName}`);
    } catch (err) {
      actionsFailed += 1;
      logger.error?.(
        `[skillvault] Failed ${action.type} for ${action.agentId}/${action.skillName}: ${err.message}`
      );
      await client
        .completeAction(action.id, {
          status: ACTION_STATUS.FAILED,
          errorMessage: err.message,
        })
        .catch(() => {});
    }
  }

  // Immediately re-scan and report after applying actions so the UI reflects updated state right away
  if (actionsApplied > 0) {
    const updatedState = await loadState(vaultHome);
    const followUpScan = await scanDeviceSkills({ config, state: updatedState, homeDir });
    await client.reportDevice(buildWireReport({ config, scanResult: followUpScan }));
  }

  return {
    scannedCount: initialScan.scannedSkills.length,
    reportedPending: reportRes.pendingCount ?? 0,
    actionsClaimed: actions.length,
    actionsApplied,
    actionsFailed,
  };
}

export function startDaemon({
  config,
  vaultHome = resolveVaultHome(),
  homeDir = os.homedir(),
  logger = console,
}) {
  const intervalMs = Math.max(2000, Number(config.pollIntervalSec || 10) * 1000);
  let running = false;
  let stopped = false;
  let debounceTimer = null;
  const watchers = [];

  const triggerSync = async (reason = 'poll') => {
    if (stopped || running) return;
    running = true;
    try {
      const res = await runSyncCycle({ config, vaultHome, homeDir, logger });
      if (res.actionsClaimed > 0 || reason === 'startup') {
        logger.log?.(
          `[skillvault] Sync (${reason}): scanned=${res.scannedCount}, applied=${res.actionsApplied}, failed=${res.actionsFailed}`
        );
      }
    } catch (err) {
      logger.error?.(`[skillvault] Sync error (${reason}): ${err.message}`);
    } finally {
      running = false;
    }
  };

  // Watch existing skills directories for near-instant drift detection
  const adapters = config.adapters || DEFAULT_ADAPTERS;
  for (const adapter of adapters) {
    const resolvedDir = expandHomePath(adapter.skillsDir, homeDir);
    if (!fs.existsSync(resolvedDir)) continue;
    try {
      const watcher = fs.watch(resolvedDir, { recursive: false }, () => {
        if (stopped) return;
        clearTimeout(debounceTimer);
        debounceTimer = setTimeout(() => triggerSync('watch'), 600);
      });
      watchers.push(watcher);
    } catch {
      // fs.watch may be unsupported on some network filesystems; polling still works
    }
  }

  triggerSync('startup');
  const timer = setInterval(() => triggerSync('poll'), intervalMs);

  function stop() {
    stopped = true;
    clearInterval(timer);
    clearTimeout(debounceTimer);
    for (const w of watchers) {
      try {
        w.close();
      } catch {
        // ignore
      }
    }
  }

  return { stop, triggerSync };
}

export function generateServiceConfig({
  platform = os.platform(),
  nodePath = process.execPath,
  binPath,
  vaultHome = resolveVaultHome(),
}) {
  const resolvedBin = path.resolve(binPath);
  if (platform === 'darwin') {
    const logDir = path.join(vaultHome, 'logs');
    return {
      kind: 'launchd',
      targetPath: '~/Library/LaunchAgents/dev.skillvault.agent.plist',
      content: `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.skillvault.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>${nodePath}</string>
    <string>${resolvedBin}</string>
    <string>daemon</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>SKILLVAULT_HOME</key>
    <string>${vaultHome}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${logDir}/agent.out.log</string>
  <key>StandardErrorPath</key>
  <string>${logDir}/agent.err.log</string>
</dict>
</plist>
`,
      installHint:
        `mkdir -p "${logDir}" ~/Library/LaunchAgents && launchctl load -w ~/Library/LaunchAgents/dev.skillvault.agent.plist`,
    };
  }

  return {
    kind: 'systemd',
    targetPath: '~/.config/systemd/user/skillvault-agent.service',
    content: `[Unit]
Description=SkillVault Agent Sync Daemon
After=network-online.target

[Service]
Type=simple
Environment=SKILLVAULT_HOME=${vaultHome}
ExecStart=${nodePath} ${resolvedBin} daemon
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`,
    installHint:
      'mkdir -p ~/.config/systemd/user && systemctl --user daemon-reload && systemctl --user enable --now skillvault-agent',
  };
}
