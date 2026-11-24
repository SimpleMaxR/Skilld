import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { applyAction, restoreBackup } from '../packages/agent/src/applier.mjs';
import { listBackups, loadState, saveConfig } from '../packages/agent/src/config.mjs';
import { runSyncCycle } from '../packages/agent/src/daemon.mjs';
import { ACTION_TYPE, DEFAULT_ADAPTERS } from '../packages/shared/src/index.mjs';

async function startGoServer({ binPath, dataDir, token }) {
  const child = spawn(
    binPath,
    ['--host', '127.0.0.1', '--port', '0', '--data-dir', dataDir, '--token', token],
    {
      // Run from dataDir (without source web/ or migrations/ nearby) to prove embed.FS is 100% self-contained.
      cwd: dataDir,
      stdio: ['ignore', 'pipe', 'pipe'],
    }
  );

  let logs = '';
  const baseUrl = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      reject(new Error(`Timed out waiting for Go server to start. Logs:\n${logs}`));
    }, 10_000);

    const onData = (chunk) => {
      const text = chunk.toString('utf8');
      logs += text;
      const m = logs.match(/Skilld Server running on (http:\/\/[^\s]+)/);
      if (m) {
        clearTimeout(timer);
        resolve(m[1]);
      }
    };

    child.stdout.on('data', onData);
    child.stderr.on('data', onData);
    child.once('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`Go server exited prematurely with code ${code}. Logs:\n${logs}`));
    });
  });

  async function close() {
    if (child.exitCode !== null) return logs;
    const exitPromise = new Promise((resolve) => child.once('exit', resolve));
    child.kill('SIGTERM');
    const code = await exitPromise;
    assert.equal(code, 0, `Expected graceful SIGTERM exit code 0, got ${code}. Logs:\n${logs}`);
    return logs;
  }

  return { baseUrl, token, close, getLogs: () => logs };
}

test('End-to-End Multi-Device Workflow against Native Go + Static SQLite Binary', async () => {
  const binPath = path.resolve('bin/skilld-server');
  await fs.access(binPath);

  const tmpRoot = await fs.mkdtemp(path.join(os.tmpdir(), 'skilld-go-e2e-'));
  const dataDir = path.join(tmpRoot, 'server-data');
  await fs.mkdir(dataDir, { recursive: true });
  const macHome = path.join(tmpRoot, 'mac-home');
  const macVaultHome = path.join(tmpRoot, 'mac-vault');
  const linuxHome = path.join(tmpRoot, 'linux-home');
  const linuxVaultHome = path.join(tmpRoot, 'linux-vault');

  const srv = await startGoServer({
    binPath,
    dataDir,
    token: 'e2e-secret-token',
  });
  const baseUrl = srv.baseUrl;

  const api = async (pathname, { method = 'GET', body } = {}) => {
    const res = await fetch(`${baseUrl}${pathname}`, {
      method,
      headers: {
        'content-type': 'application/json',
        authorization: `Bearer ${srv.token}`,
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    const json = await res.json();
    return { status: res.status, json };
  };

  try {
    // Prepare simulated Mac & Linux agent skill directories
    await fs.mkdir(path.join(macHome, '.claude', 'skills', 'my-helper', 'scripts'), {
      recursive: true,
    });
    await fs.mkdir(path.join(macHome, '.codex', 'skills'), { recursive: true });
    await fs.writeFile(
      path.join(macHome, '.claude', 'skills', 'my-helper', 'SKILL.md'),
      '---\nname: My Helper\ndescription: Cross-machine helper\n---\n# My Helper v1\nStep 1\n',
      'utf8'
    );
    const scriptPath = path.join(macHome, '.claude', 'skills', 'my-helper', 'scripts', 'tool.sh');
    await fs.writeFile(scriptPath, '#!/usr/bin/env bash\necho helper\n', 'utf8');
    await fs.chmod(scriptPath, 0o755);

    // Symlinked skill on Mac
    const externalSourceDir = path.join(tmpRoot, 'external-skill-src');
    await fs.mkdir(externalSourceDir, { recursive: true });
    await fs.writeFile(
      path.join(externalSourceDir, 'SKILL.md'),
      '---\nname: Linked Skill\n---\nSymlinked content\n',
      'utf8'
    );
    await fs.symlink(
      externalSourceDir,
      path.join(macHome, '.claude', 'skills', 'linked-skill')
    );

    await fs.mkdir(path.join(linuxHome, '.codex', 'skills'), { recursive: true });
    await fs.mkdir(path.join(linuxHome, '.codebuddy', 'skills'), { recursive: true });

    const macConfig = await saveConfig(
      {
        deviceId: 'mac-e2e',
        deviceName: 'MacBook-Pro',
        serverUrl: baseUrl,
        token: srv.token,
        adapters: DEFAULT_ADAPTERS,
      },
      macVaultHome
    );

    const linuxConfig = await saveConfig(
      {
        deviceId: 'linux-e2e',
        deviceName: 'linux-dev-01',
        serverUrl: baseUrl,
        token: srv.token,
        adapters: DEFAULT_ADAPTERS,
      },
      linuxVaultHome
    );

    // 0. Smoke check embedded HTTP serving & SQLite/GroupCommit runtime telemetry
    for (const assetPath of ['/api/health', '/', '/styles.css', '/app.mjs']) {
      const res = await fetch(`${baseUrl}${assetPath}`);
      assert.equal(res.status, 200, `Expected HTTP 200 for ${assetPath}`);
    }
    const rt = await api('/api/system/runtime');
    assert.equal(rt.status, 200);
    assert.equal(rt.json.pragmas.writeWalAutocheckpoint, 0);
    assert.equal(rt.json.pragmas.readQueryOnly, 1);
    assert.equal(rt.json.pragmas.writeMmapSize, 134217728);
    assert.equal(rt.json.pragmas.writeCacheSize, -32000);
    assert.equal(rt.json.readPool.maxOpen, 6);
    assert.equal(rt.json.writePool.maxOpen, 1);
    assert.equal(rt.json.writerStats.queueCapacity, 8192);

    // 1. Initial sync registers both devices
    const macSync1 = await runSyncCycle({ config: macConfig, vaultHome: macVaultHome, homeDir: macHome });
    assert.equal(macSync1.scannedCount, 2);

    const linuxInitSync = await runSyncCycle({
      config: linuxConfig,
      vaultHome: linuxVaultHome,
      homeDir: linuxHome,
    });
    assert.equal(linuxInitSync.scannedCount, 0);

    let ov = (await api('/api/overview')).json;
    const macHelper = ov.installations.find(
      (i) => i.deviceId === 'mac-e2e' && i.skillName === 'my-helper'
    );
    assert.equal(macHelper.status, 'unmanaged');

    // 2. Adopt `my-helper` into Vault
    const adoptRes = await api('/api/installations/adopt', {
      method: 'POST',
      body: { deviceId: 'mac-e2e', agentId: 'claude-code', skillName: 'my-helper' },
    });
    assert.equal(adoptRes.status, 201);
    assert.equal(adoptRes.json.skill.id, 'my-helper');

    const macSync2 = await runSyncCycle({ config: macConfig, vaultHome: macVaultHome, homeDir: macHome });
    assert.equal(macSync2.actionsApplied, 1);

    const macStateAfterAdopt = await loadState(macVaultHome);
    assert.equal(macStateAfterAdopt.installations['claude-code']['my-helper'].vaultSkillId, 'my-helper');

    // 3. Deploy `my-helper` to Linux codex and codebuddy
    const deployRes = await api('/api/installations/deploy', {
      method: 'POST',
      body: {
        skillId: 'my-helper',
        targets: [
          { deviceId: 'linux-e2e', agentId: 'codex' },
          { deviceId: 'linux-e2e', agentId: 'codebuddy' },
        ],
      },
    });
    assert.equal(deployRes.status, 201);

    const linuxSync1 = await runSyncCycle({
      config: linuxConfig,
      vaultHome: linuxVaultHome,
      homeDir: linuxHome,
    });
    assert.equal(linuxSync1.actionsApplied, 2);

    const linuxCodexScriptStat = await fs.stat(
      path.join(linuxHome, '.codex', 'skills', 'my-helper', 'scripts', 'tool.sh')
    );
    assert.ok((linuxCodexScriptStat.mode & 0o111) !== 0, 'Executable permission must be preserved');

    ov = (await api('/api/overview')).json;
    assert.equal(
      ov.installations.find((i) => i.deviceId === 'linux-e2e' && i.agentId === 'codex').status,
      'synced'
    );
    assert.equal(
      ov.installations.find((i) => i.deviceId === 'linux-e2e' && i.agentId === 'codebuddy').status,
      'synced'
    );

    // 4. Simulate external AI modifying SKILL.md on Mac -> drift detected
    const aiEditedContent =
      '---\nname: My Helper\ndescription: Cross-machine helper\n---\n# My Helper v2 (AI improved)\nStep 1\nStep 2: Verify output\n';
    await fs.writeFile(
      path.join(macHome, '.claude', 'skills', 'my-helper', 'SKILL.md'),
      aiEditedContent,
      'utf8'
    );

    await runSyncCycle({ config: macConfig, vaultHome: macVaultHome, homeDir: macHome });
    ov = (await api('/api/overview')).json;
    assert.equal(
      ov.installations.find((i) => i.deviceId === 'mac-e2e' && i.skillName === 'my-helper').status,
      'modified'
    );

    const diffRes = await api(
      '/api/installations/diff?deviceId=mac-e2e&agentId=claude-code&skillName=my-helper'
    );
    assert.equal(diffRes.status, 200);
    assert.equal(diffRes.json.mode, 'modified');
    assert.equal(diffRes.json.diff.hasChanges, true);

    // 5. Safety guard: direct safe update on drifted Mac installation MUST fail without overwriting disk
    await assert.rejects(
      () =>
        applyAction({
          action: {
            id: 'test-guard',
            agentId: 'claude-code',
            skillName: 'my-helper',
            type: ACTION_TYPE.UPDATE,
            payload: {
              vaultSkillId: 'my-helper',
              versionId: 'fake',
              expectedBaseHash: macStateAfterAdopt.installations['claude-code']['my-helper'].baseHash,
              files: {
                'SKILL.md': { encoding: 'utf8', content: 'SHOULD NOT OVERWRITE', mode: '0644' },
              },
            },
          },
          config: macConfig,
          vaultHome: macVaultHome,
          homeDir: macHome,
        }),
      /Local drift detected/
    );
    const macDiskStillIntact = await fs.readFile(
      path.join(macHome, '.claude', 'skills', 'my-helper', 'SKILL.md'),
      'utf8'
    );
    assert.equal(macDiskStillIntact, aiEditedContent);

    // 6. Promote Mac's AI edit to Vault v2
    const promoteRes = await api('/api/installations/promote', {
      method: 'POST',
      body: { deviceId: 'mac-e2e', agentId: 'claude-code', skillName: 'my-helper' },
    });
    assert.equal(promoteRes.status, 201);
    assert.equal(promoteRes.json.version.versionNum, 2);

    await runSyncCycle({ config: macConfig, vaultHome: macVaultHome, homeDir: macHome });
    ov = (await api('/api/overview')).json;
    assert.equal(
      ov.installations.find((i) => i.deviceId === 'mac-e2e' && i.skillName === 'my-helper').status,
      'synced'
    );
    assert.equal(
      ov.installations.find((i) => i.deviceId === 'linux-e2e' && i.agentId === 'codex').status,
      'outdated'
    );

    // 7. Update Linux codex to v2
    const updateRes = await api('/api/installations/update', {
      method: 'POST',
      body: { deviceId: 'linux-e2e', agentId: 'codex', skillName: 'my-helper' },
    });
    assert.equal(updateRes.status, 201);

    const linuxSync2 = await runSyncCycle({
      config: linuxConfig,
      vaultHome: linuxVaultHome,
      homeDir: linuxHome,
    });
    assert.equal(linuxSync2.actionsApplied, 1);

    const linuxCodexMd = await fs.readFile(
      path.join(linuxHome, '.codex', 'skills', 'my-helper', 'SKILL.md'),
      'utf8'
    );
    assert.equal(linuxCodexMd, aiEditedContent);

    // 8. Accidental drift on Linux codex -> Reset -> Verify backup + restoreBackup
    const accidentalText = 'ACCIDENTAL DRIFT ON LINUX';
    await fs.writeFile(
      path.join(linuxHome, '.codex', 'skills', 'my-helper', 'SKILL.md'),
      accidentalText,
      'utf8'
    );
    await runSyncCycle({ config: linuxConfig, vaultHome: linuxVaultHome, homeDir: linuxHome });

    const resetRes = await api('/api/installations/reset', {
      method: 'POST',
      body: { deviceId: 'linux-e2e', agentId: 'codex', skillName: 'my-helper' },
    });
    assert.equal(resetRes.status, 201);

    await runSyncCycle({ config: linuxConfig, vaultHome: linuxVaultHome, homeDir: linuxHome });
    const afterReset = await fs.readFile(
      path.join(linuxHome, '.codex', 'skills', 'my-helper', 'SKILL.md'),
      'utf8'
    );
    assert.equal(afterReset, aiEditedContent);

    const backups = await listBackups(linuxVaultHome);
    const resetBackup = backups.find((b) => b.reason === 'pre-reset');
    assert.ok(resetBackup, 'pre-reset backup snapshot must exist');

    await restoreBackup({
      backupId: resetBackup.id,
      config: linuxConfig,
      vaultHome: linuxVaultHome,
      homeDir: linuxHome,
    });
    const restoredDisk = await fs.readFile(
      path.join(linuxHome, '.codex', 'skills', 'my-helper', 'SKILL.md'),
      'utf8'
    );
    assert.equal(restoredDisk, accidentalText);

    // 9. External deletion on Linux codebuddy -> missing -> forget_state cleanup
    await fs.rm(path.join(linuxHome, '.codebuddy', 'skills', 'my-helper'), {
      recursive: true,
      force: true,
    });
    await runSyncCycle({ config: linuxConfig, vaultHome: linuxVaultHome, homeDir: linuxHome });
    ov = (await api('/api/overview')).json;
    assert.equal(
      ov.installations.find((i) => i.deviceId === 'linux-e2e' && i.agentId === 'codebuddy').status,
      'missing'
    );

    const removeMissing = await api('/api/installations/remove', {
      method: 'POST',
      body: { deviceId: 'linux-e2e', agentId: 'codebuddy', skillName: 'my-helper' },
    });
    assert.equal(removeMissing.status, 201);

    await runSyncCycle({ config: linuxConfig, vaultHome: linuxVaultHome, homeDir: linuxHome });
    ov = (await api('/api/overview')).json;
    assert.equal(
      ov.installations.find((i) => i.deviceId === 'linux-e2e' && i.agentId === 'codebuddy'),
      undefined
    );
  } finally {
    const finalLogs = await srv.close();
    assert.match(finalLogs, /Async queue drained cleanly/);
    await fs.rm(tmpRoot, { recursive: true, force: true });
  }
});
