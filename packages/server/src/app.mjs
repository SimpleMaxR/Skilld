import {
  ACTION_STATUS,
  ACTION_TYPE,
  INSTALL_STATUS,
  computeFilesHash,
  diffFileMaps,
  isoNow,
  normalizeFileMap,
  parseSkillFrontmatter,
  slugifySkillId,
} from '../../shared/src/index.mjs';
import {
  HttpError,
  createRouter,
  jsonResponse,
  readJsonBody,
  textResponse,
} from './router.mjs';
import {
  checkGitHubUpstream,
  fetchGitHubSkillFiles,
  parseGitHubUrl,
} from './upstream.mjs';

const ONLINE_THRESHOLD_MS = 65_000;

export function createApp({
  store,
  token,
  githubToken = null,
  webAssets = {},
  readLiveWebFile = null,
  fetchImpl = globalThis.fetch,
  runtime = 'node',
}) {
  const router = createRouter();

  // --- Health & Auth ---
  router.get('/api/health', async () => {
    return jsonResponse({ ok: true, runtime, version: '0.1.0', time: isoNow() });
  });

  router.get('/api/auth/verify', async () => {
    return jsonResponse({ ok: true });
  });

  // --- Overview ---
  router.get('/api/overview', async () => {
    const [devices, skills, installations, actions] = await Promise.all([
      store.listDevices(),
      store.listSkills(),
      store.listInstallations(),
      store.listActions({ limit: 80 }),
    ]);

    const nowMs = Date.now();
    const enrichedDevices = devices.map((d) => {
      const devInsts = installations.filter((i) => i.deviceId === d.id);
      const attentionCount = devInsts.filter(
        (i) => !i.ignored && i.status !== INSTALL_STATUS.SYNCED
      ).length;
      const lastSeenMs = Date.parse(d.lastSeenAt || 0);
      return {
        ...d,
        online: Number.isFinite(lastSeenMs) && nowMs - lastSeenMs < ONLINE_THRESHOLD_MS,
        skillCount: devInsts.length,
        attentionCount,
      };
    });

    const enrichedSkills = skills.map((s) => {
      const linked = installations.filter((i) => i.vaultSkillId === s.id);
      const outdatedCount = linked.filter((i) => i.status === INSTALL_STATUS.OUTDATED).length;
      const modifiedCount = linked.filter((i) => i.status === INSTALL_STATUS.MODIFIED).length;
      return {
        ...s,
        installedCount: linked.length,
        outdatedCount,
        modifiedCount,
      };
    });

    const pendingActions = actions.filter(
      (a) => a.status === ACTION_STATUS.PENDING || a.status === ACTION_STATUS.CLAIMED
    );

    const summary = {
      totalDevices: enrichedDevices.length,
      onlineDevices: enrichedDevices.filter((d) => d.online).length,
      totalSkills: enrichedSkills.length,
      upstreamUpdates: enrichedSkills.filter((s) => s.hasUpstreamUpdate).length,
      totalInstallations: installations.length,
      syncedCount: installations.filter((i) => i.status === INSTALL_STATUS.SYNCED).length,
      outdatedCount: installations.filter((i) => i.status === INSTALL_STATUS.OUTDATED).length,
      modifiedCount: installations.filter((i) => i.status === INSTALL_STATUS.MODIFIED).length,
      unmanagedCount: installations.filter((i) => i.status === INSTALL_STATUS.UNMANAGED).length,
      missingCount: installations.filter((i) => i.status === INSTALL_STATUS.MISSING).length,
      pendingActionCount: pendingActions.length,
    };

    return jsonResponse({
      devices: enrichedDevices,
      skills: enrichedSkills,
      installations,
      actions,
      summary,
    });
  });

  // --- Vault Skills ---
  router.get('/api/skills', async () => {
    const [skills, installations] = await Promise.all([
      store.listSkills(),
      store.listInstallations(),
    ]);
    const data = skills.map((s) => ({
      ...s,
      installedCount: installations.filter((i) => i.vaultSkillId === s.id).length,
    }));
    return jsonResponse({ skills: data });
  });

  router.get('/api/skills/:id', async ({ params }) => {
    const skill = await store.getSkill(params.id, {
      includeVersions: true,
      includeCurrentFiles: true,
    });
    if (!skill) throw new HttpError(404, 'Skill not found', 'SKILL_NOT_FOUND');
    const installations = await store.listInstallations({ skillId: skill.id });
    return jsonResponse({ skill, installations });
  });

  router.get('/api/versions/:id', async ({ params }) => {
    const version = await store.getVersion(params.id, { includeFiles: true });
    if (!version) throw new HttpError(404, 'Version not found', 'VERSION_NOT_FOUND');
    return jsonResponse({ version });
  });

  router.post('/api/skills', async ({ request }) => {
    const body = await readJsonBody(request);
    const rawFiles = buildFilesInput(body);
    const files = normalizeFileMap(rawFiles);
    const contentHash = await computeFilesHash(files);

    const skillMd = files['SKILL.md'];
    const meta =
      skillMd && skillMd.encoding === 'utf8'
        ? parseSkillFrontmatter(skillMd.content)
        : { name: '', description: '' };

    const name = String(body.name || meta.name || '').trim();
    if (!name) {
      throw new HttpError(400, 'Skill name is required', 'VALIDATION_ERROR');
    }
    const description = String(body.description ?? meta.description ?? '').trim();

    let id = body.id ? slugifySkillId(body.id) : await allocateUniqueSkillId(store, name);
    const existing = await store.getSkill(id, { includeVersions: false, includeCurrentFiles: false });
    if (existing) {
      throw new HttpError(409, `Skill ID "${id}" already exists`, 'SKILL_EXISTS');
    }

    let upstream = null;
    if (body.upstreamUrl) {
      upstream = parseGitHubUrl(body.upstreamUrl, body.upstreamRef, body.upstreamSubpath);
    }

    const skill = await store.createSkillWithVersion({
      id,
      name,
      description,
      sourceType: upstream ? 'github' : body.sourceType || 'custom',
      upstreamUrl: upstream ? upstream.canonicalUrl : null,
      upstreamRef: upstream ? upstream.ref : null,
      upstreamSubpath: upstream ? upstream.subpath : null,
      note: body.note || 'Initial version',
      files,
      contentHash,
    });

    return jsonResponse({ skill }, 201);
  });

  router.post('/api/skills/import-github', async ({ request }) => {
    const body = await readJsonBody(request);
    const parsed = parseGitHubUrl(body.url, body.ref, body.subpath);
    const fetched = await fetchGitHubSkillFiles({
      owner: parsed.owner,
      repo: parsed.repo,
      ref: parsed.ref,
      subpath: parsed.subpath,
      token: body.githubToken || githubToken,
      fetchImpl,
    });

    const fallbackName = parsed.subpath
      ? parsed.subpath.split('/').pop()
      : parsed.repo;
    const name = String(body.name || fetched.meta.name || fallbackName || 'github-skill').trim();
    const description = String(body.description ?? fetched.meta.description ?? '').trim();
    const id = body.id ? slugifySkillId(body.id) : await allocateUniqueSkillId(store, name);

    const existing = await store.getSkill(id, { includeVersions: false, includeCurrentFiles: false });
    if (existing) {
      throw new HttpError(409, `Skill ID "${id}" already exists`, 'SKILL_EXISTS');
    }

    const skill = await store.createSkillWithVersion({
      id,
      name,
      description,
      sourceType: 'github',
      upstreamUrl: parsed.canonicalUrl,
      upstreamRef: parsed.ref,
      upstreamSubpath: parsed.subpath,
      upstreamLatestRev: fetched.rev,
      upstreamLatestMessage: fetched.message,
      upstreamCheckedAt: fetched.checkedAt,
      sourceRev: fetched.rev,
      note: `Imported from GitHub (${fetched.rev.slice(0, 7)})`,
      files: fetched.files,
      contentHash: fetched.contentHash,
    });

    return jsonResponse({ skill }, 201);
  });

  router.put('/api/skills/:id', async ({ params, request }) => {
    const body = await readJsonBody(request);
    let upstreamPatch = {};
    if (body.upstreamUrl !== undefined) {
      if (!body.upstreamUrl) {
        upstreamPatch = { upstreamUrl: null, upstreamRef: null, upstreamSubpath: null };
      } else {
        const parsed = parseGitHubUrl(body.upstreamUrl, body.upstreamRef, body.upstreamSubpath);
        upstreamPatch = {
          upstreamUrl: parsed.canonicalUrl,
          upstreamRef: parsed.ref,
          upstreamSubpath: parsed.subpath,
        };
      }
    }
    const skill = await store.updateSkillMeta(params.id, {
      name: body.name,
      description: body.description,
      ...upstreamPatch,
    });
    if (!skill) throw new HttpError(404, 'Skill not found', 'SKILL_NOT_FOUND');
    return jsonResponse({ skill });
  });

  router.post('/api/skills/:id/versions', async ({ params, request }) => {
    const existing = await store.getSkill(params.id);
    if (!existing) throw new HttpError(404, 'Skill not found', 'SKILL_NOT_FOUND');

    const body = await readJsonBody(request);
    const files = normalizeFileMap(buildFilesInput(body));
    const contentHash = await computeFilesHash(files);

    if (existing.currentContentHash === contentHash && !body.allowDuplicate) {
      return jsonResponse({ created: false, version: existing.currentVersion, skill: existing });
    }

    const version = await store.addSkillVersion(params.id, {
      files,
      contentHash,
      note: body.note || 'Manual edit',
    });
    const skill = await store.getSkill(params.id);
    return jsonResponse({ created: true, version, skill }, 201);
  });

  router.post('/api/skills/:id/check-upstream', async ({ params }) => {
    const skill = await store.getSkill(params.id, {
      includeVersions: false,
      includeCurrentFiles: false,
    });
    if (!skill) throw new HttpError(404, 'Skill not found', 'SKILL_NOT_FOUND');
    if (!skill.upstreamUrl) {
      throw new HttpError(400, 'Skill does not have a GitHub upstream URL', 'NO_UPSTREAM');
    }
    const updated = await checkSingleSkillUpstream({ store, skill, githubToken, fetchImpl });
    return jsonResponse({ skill: updated });
  });

  router.post('/api/skills/check-all-upstream', async () => {
    const skills = await store.listSkills();
    const results = [];
    for (const skill of skills) {
      if (!skill.upstreamUrl) continue;
      try {
        const updated = await checkSingleSkillUpstream({ store, skill, githubToken, fetchImpl });
        results.push({ id: skill.id, ok: true, hasUpstreamUpdate: updated.hasUpstreamUpdate });
      } catch (err) {
        results.push({ id: skill.id, ok: false, error: err.message });
      }
    }
    return jsonResponse({ results });
  });

  router.post('/api/skills/:id/upgrade-upstream', async ({ params }) => {
    const skill = await store.getSkill(params.id);
    if (!skill) throw new HttpError(404, 'Skill not found', 'SKILL_NOT_FOUND');
    if (!skill.upstreamUrl) {
      throw new HttpError(400, 'Skill does not have a GitHub upstream URL', 'NO_UPSTREAM');
    }

    const parsed = parseGitHubUrl(skill.upstreamUrl, skill.upstreamRef, skill.upstreamSubpath);
    const fetched = await fetchGitHubSkillFiles({
      owner: parsed.owner,
      repo: parsed.repo,
      ref: parsed.ref,
      subpath: parsed.subpath,
      token: githubToken,
      fetchImpl,
    });

    await store.updateSkillUpstream(skill.id, {
      upstreamLatestRev: fetched.rev,
      upstreamLatestMessage: fetched.message,
      upstreamCheckedAt: fetched.checkedAt,
    });

    if (skill.currentContentHash === fetched.contentHash) {
      const refreshed = await store.getSkill(skill.id);
      return jsonResponse({ upgraded: false, skill: refreshed });
    }

    const version = await store.addSkillVersion(skill.id, {
      files: fetched.files,
      contentHash: fetched.contentHash,
      sourceRev: fetched.rev,
      note: `Upstream ${fetched.rev.slice(0, 7)}: ${fetched.message || 'sync'}`,
    });
    const refreshed = await store.getSkill(skill.id);
    return jsonResponse({ upgraded: true, version, skill: refreshed });
  });

  router.delete('/api/skills/:id', async ({ params }) => {
    const deleted = await store.deleteSkill(params.id);
    if (!deleted) throw new HttpError(404, 'Skill not found', 'SKILL_NOT_FOUND');
    return jsonResponse({ ok: true });
  });

  // --- Devices & Daemon Protocol ---
  router.get('/api/devices', async () => {
    const devices = await store.listDevices();
    return jsonResponse({ devices });
  });

  router.delete('/api/devices/:id', async ({ params }) => {
    const deleted = await store.deleteDevice(params.id);
    if (!deleted) throw new HttpError(404, 'Device not found', 'DEVICE_NOT_FOUND');
    return jsonResponse({ ok: true });
  });

  router.post('/api/devices/report', async ({ request }) => {
    const body = await readJsonBody(request);
    const dev = body.device;
    if (!dev || !dev.id || !dev.name) {
      throw new HttpError(400, 'device.id and device.name are required', 'VALIDATION_ERROR');
    }

    const device = await store.upsertDevice({
      id: String(dev.id),
      name: String(dev.name),
      os: String(dev.os || 'unknown'),
      arch: String(dev.arch || 'unknown'),
      hostname: dev.hostname ? String(dev.hostname) : null,
      detectedAgents: Array.isArray(body.detectedAgents) ? body.detectedAgents : [],
    });

    const scannedSkills = Array.isArray(body.scannedSkills) ? body.scannedSkills : [];
    await store.replaceDeviceInstallations(device.id, scannedSkills);
    const pendingCount = await store.countPendingActions(device.id);

    return jsonResponse({ ok: true, device, pendingCount });
  });

  router.post('/api/devices/:id/actions/claim', async ({ params }) => {
    const device = await store.getDevice(params.id);
    if (!device) throw new HttpError(404, 'Device not found', 'DEVICE_NOT_FOUND');
    const actions = await store.claimPendingActions(device.id);
    return jsonResponse({ actions });
  });

  router.post('/api/actions/:id/complete', async ({ params, request }) => {
    const body = await readJsonBody(request);
    const status =
      body.status === ACTION_STATUS.COMPLETED ? ACTION_STATUS.COMPLETED : ACTION_STATUS.FAILED;
    const action = await store.completeAction(params.id, {
      status,
      errorMessage: body.errorMessage ? String(body.errorMessage) : null,
    });
    if (!action) throw new HttpError(404, 'Action not found', 'ACTION_NOT_FOUND');
    return jsonResponse({ action });
  });

  // --- Installation Operations & Diff ---
  router.get('/api/installations/diff', async ({ url }) => {
    const deviceId = url.searchParams.get('deviceId');
    const agentId = url.searchParams.get('agentId');
    const skillName = url.searchParams.get('skillName');
    if (!deviceId || !agentId || !skillName) {
      throw new HttpError(400, 'deviceId, agentId, and skillName are required', 'VALIDATION_ERROR');
    }

    const inst = await store.getInstallation(deviceId, agentId, skillName, {
      includeFiles: true,
    });
    if (!inst) throw new HttpError(404, 'Installation not found', 'INSTALLATION_NOT_FOUND');

    let targetVersion = null;
    if (inst.vaultSkillId) {
      const skill = await store.getSkill(inst.vaultSkillId, {
        includeVersions: false,
        includeCurrentFiles: true,
      });
      targetVersion = skill?.currentVersion ?? null;
    }

    const baseVersion =
      inst.vaultSkillId && inst.baseHash
        ? await store.findVersionByHash(inst.vaultSkillId, inst.baseHash)
        : null;

    if (inst.status === INSTALL_STATUS.MODIFIED) {
      const localFiles = inst.actualFiles ?? {};
      const targetFiles = targetVersion?.files ?? {};
      return jsonResponse({
        mode: INSTALL_STATUS.MODIFIED,
        installation: omitFiles(inst),
        localFiles,
        targetFiles,
        targetVersionNum: targetVersion?.versionNum ?? null,
        diff: diffFileMaps(targetFiles, localFiles),
        baseToLocalDiff: baseVersion ? diffFileMaps(baseVersion.files, localFiles) : null,
      });
    }

    if (inst.status === INSTALL_STATUS.OUTDATED) {
      const localFiles = baseVersion?.files ?? {};
      const targetFiles = targetVersion?.files ?? {};
      return jsonResponse({
        mode: INSTALL_STATUS.OUTDATED,
        installation: omitFiles(inst),
        localFiles,
        targetFiles,
        baseVersionNum: baseVersion?.versionNum ?? null,
        targetVersionNum: targetVersion?.versionNum ?? null,
        diff: diffFileMaps(localFiles, targetFiles),
      });
    }

    if (inst.status === INSTALL_STATUS.UNMANAGED) {
      const localFiles = inst.actualFiles ?? {};
      return jsonResponse({
        mode: INSTALL_STATUS.UNMANAGED,
        installation: omitFiles(inst),
        localFiles,
        targetFiles: {},
        diff: diffFileMaps({}, localFiles),
      });
    }

    const files = targetVersion?.files ?? baseVersion?.files ?? inst.actualFiles ?? {};
    return jsonResponse({
      mode: inst.status,
      installation: omitFiles(inst),
      localFiles: files,
      targetFiles: files,
      targetVersionNum: targetVersion?.versionNum ?? null,
      diff: { hasChanges: false, changedCount: 0, files: [] },
    });
  });

  router.post('/api/installations/deploy', async ({ request }) => {
    const body = await readJsonBody(request);
    const skillId = body.skillId;
    if (!skillId) throw new HttpError(400, 'skillId is required', 'VALIDATION_ERROR');

    const skill = await store.getSkill(skillId, {
      includeVersions: false,
      includeCurrentFiles: false,
    });
    if (!skill || !skill.currentVersionId) {
      throw new HttpError(404, 'Vault skill not found', 'SKILL_NOT_FOUND');
    }

    const targets = Array.isArray(body.targets)
      ? body.targets
      : body.deviceId && body.agentId
        ? [{ deviceId: body.deviceId, agentId: body.agentId }]
        : [];
    if (targets.length === 0) {
      throw new HttpError(400, 'At least one target (deviceId, agentId) is required', 'VALIDATION_ERROR');
    }

    const createdActions = [];
    for (const target of targets) {
      const device = await store.getDevice(target.deviceId);
      if (!device) {
        throw new HttpError(404, `Device not found: ${target.deviceId}`, 'DEVICE_NOT_FOUND');
      }
      const skillName = String(target.skillName || body.skillName || skill.id);
      const action = await store.createAction({
        deviceId: device.id,
        agentId: String(target.agentId),
        skillName,
        type: ACTION_TYPE.INSTALL,
        payload: {
          vaultSkillId: skill.id,
          versionId: body.versionId || skill.currentVersionId,
          force: Boolean(body.force),
        },
      });
      createdActions.push(action);
    }

    return jsonResponse({ actions: createdActions }, 201);
  });

  router.post('/api/installations/update', async ({ request }) => {
    const body = await readJsonBody(request);
    const inst = await requireInstallation(store, body);
    if (!inst.vaultSkillId) {
      throw new HttpError(400, 'Installation is not linked to a Vault skill', 'UNMANAGED_SKILL');
    }
    if (inst.status === INSTALL_STATUS.MODIFIED && !body.force) {
      throw new HttpError(
        409,
        'Local installation has drifted; use promote or reset instead of safe update',
        'DRIFT_CONFLICT'
      );
    }
    const skill = await store.getSkill(inst.vaultSkillId, {
      includeVersions: false,
      includeCurrentFiles: false,
    });
    if (!skill?.currentVersionId) {
      throw new HttpError(404, 'Vault skill version not found', 'SKILL_NOT_FOUND');
    }

    const action = await store.createAction({
      deviceId: inst.deviceId,
      agentId: inst.agentId,
      skillName: inst.skillName,
      type: ACTION_TYPE.UPDATE,
      payload: {
        vaultSkillId: skill.id,
        versionId: body.versionId || skill.currentVersionId,
        expectedBaseHash: inst.baseHash,
        force: false,
      },
    });
    return jsonResponse({ action }, 201);
  });

  router.post('/api/installations/reset', async ({ request }) => {
    const body = await readJsonBody(request);
    const inst = await requireInstallation(store, body);
    if (!inst.vaultSkillId) {
      throw new HttpError(400, 'Installation is not linked to a Vault skill', 'UNMANAGED_SKILL');
    }
    const skill = await store.getSkill(inst.vaultSkillId, {
      includeVersions: false,
      includeCurrentFiles: false,
    });
    if (!skill?.currentVersionId) {
      throw new HttpError(404, 'Vault skill version not found', 'SKILL_NOT_FOUND');
    }

    const action = await store.createAction({
      deviceId: inst.deviceId,
      agentId: inst.agentId,
      skillName: inst.skillName,
      type: ACTION_TYPE.RESET,
      payload: {
        vaultSkillId: skill.id,
        versionId: body.versionId || skill.currentVersionId,
        force: true,
      },
    });
    return jsonResponse({ action }, 201);
  });

  router.post('/api/installations/promote', async ({ request }) => {
    const body = await readJsonBody(request);
    const inst = await requireInstallation(store, body, true);
    if (!inst.actualFiles || !inst.actualHash) {
      throw new HttpError(400, 'No local snapshot available to promote', 'NO_LOCAL_FILES');
    }
    if (!inst.vaultSkillId) {
      throw new HttpError(
        400,
        'Unmanaged skill must be adopted into Vault before promoting',
        'UNMANAGED_SKILL'
      );
    }

    const files = normalizeFileMap(inst.actualFiles);
    const contentHash = await computeFilesHash(files);
    const deviceLabel = inst.deviceName || inst.deviceId;

    const version = await store.addSkillVersion(inst.vaultSkillId, {
      files,
      contentHash,
      note: body.note || `Promoted from ${deviceLabel} (${inst.agentId})`,
    });

    await store.updateInstallation(inst.deviceId, inst.agentId, inst.skillName, {
      desiredVersionId: version.id,
      baseHash: contentHash,
      status: INSTALL_STATUS.SYNCED,
      clearActualFiles: true,
    });

    const action = await store.createAction({
      deviceId: inst.deviceId,
      agentId: inst.agentId,
      skillName: inst.skillName,
      type: ACTION_TYPE.ADOPT_CONFIRM,
      payload: {
        vaultSkillId: inst.vaultSkillId,
        versionId: version.id,
        expectedHash: contentHash,
      },
    });

    const skill = await store.getSkill(inst.vaultSkillId);
    return jsonResponse({ skill, version, action }, 201);
  });

  router.post('/api/installations/adopt', async ({ request }) => {
    const body = await readJsonBody(request);
    const inst = await requireInstallation(store, body, true);
    if (!inst.actualFiles || !inst.actualHash) {
      throw new HttpError(400, 'No local files found to adopt', 'NO_LOCAL_FILES');
    }

    const files = normalizeFileMap(inst.actualFiles);
    const contentHash = await computeFilesHash(files);

    let skill;
    let version;

    if (body.targetSkillId) {
      skill = await store.getSkill(body.targetSkillId);
      if (!skill) throw new HttpError(404, 'Target Vault skill not found', 'SKILL_NOT_FOUND');
      if (body.promoteNewVersion && skill.currentContentHash !== contentHash) {
        version = await store.addSkillVersion(skill.id, {
          files,
          contentHash,
          note: body.note || `Adopted from ${inst.deviceName || inst.deviceId}`,
        });
        skill = await store.getSkill(skill.id);
      } else {
        version = skill.currentVersion;
      }
    } else {
      const skillMd = files['SKILL.md'];
      const meta =
        skillMd && skillMd.encoding === 'utf8'
          ? parseSkillFrontmatter(skillMd.content)
          : { name: '', description: '' };
      const name = String(body.name || meta.name || inst.skillName).trim();
      const description = String(body.description ?? meta.description ?? '').trim();
      const id = body.id ? slugifySkillId(body.id) : await allocateUniqueSkillId(store, inst.skillName);

      skill = await store.createSkillWithVersion({
        id,
        name,
        description,
        sourceType: 'adopted',
        note: `Adopted from ${inst.deviceName || inst.deviceId} (${inst.agentId})`,
        files,
        contentHash,
      });
      version = skill.currentVersion;
    }

    const nextStatus =
      version && version.contentHash === contentHash
        ? INSTALL_STATUS.SYNCED
        : INSTALL_STATUS.OUTDATED;

    await store.updateInstallation(inst.deviceId, inst.agentId, inst.skillName, {
      vaultSkillId: skill.id,
      desiredVersionId: version?.id ?? null,
      baseHash: contentHash,
      status: nextStatus,
      clearActualFiles: nextStatus === INSTALL_STATUS.SYNCED,
    });

    const action = await store.createAction({
      deviceId: inst.deviceId,
      agentId: inst.agentId,
      skillName: inst.skillName,
      type: ACTION_TYPE.ADOPT_CONFIRM,
      payload: {
        vaultSkillId: skill.id,
        versionId: version?.id ?? null,
        expectedHash: contentHash,
      },
    });

    return jsonResponse({ skill, version, action }, 201);
  });

  router.post('/api/installations/ignore', async ({ request }) => {
    const body = await readJsonBody(request);
    const inst = await requireInstallation(store, body);
    const updated = await store.updateInstallation(inst.deviceId, inst.agentId, inst.skillName, {
      ignored: Boolean(body.ignored),
    });
    return jsonResponse({ installation: omitFiles(updated) });
  });

  router.post('/api/installations/remove', async ({ request }) => {
    const body = await readJsonBody(request);
    const inst = await requireInstallation(store, body);

    if (inst.status === INSTALL_STATUS.MISSING || body.forgetOnly) {
      const action = await store.createAction({
        deviceId: inst.deviceId,
        agentId: inst.agentId,
        skillName: inst.skillName,
        type: ACTION_TYPE.FORGET_STATE,
        payload: {},
      });
      return jsonResponse({ action }, 201);
    }

    const action = await store.createAction({
      deviceId: inst.deviceId,
      agentId: inst.agentId,
      skillName: inst.skillName,
      type: ACTION_TYPE.REMOVE,
      payload: {},
    });
    return jsonResponse({ action }, 201);
  });

  // --- Actions ---
  router.get('/api/actions', async ({ url }) => {
    const deviceId = url.searchParams.get('deviceId');
    const status = url.searchParams.get('status');
    const actions = await store.listActions({ deviceId, status, limit: 100 });
    return jsonResponse({ actions });
  });

  router.post('/api/actions/:id/cancel', async ({ params }) => {
    const action = await store.cancelAction(params.id);
    if (!action) throw new HttpError(404, 'Action not found', 'ACTION_NOT_FOUND');
    return jsonResponse({ action });
  });

  // --- Main Fetch Entry ---
  async function fetch(request) {
    try {
      const url = new URL(request.url);

      if (url.pathname.startsWith('/api/')) {
        if (request.method !== 'OPTIONS' && url.pathname !== '/api/health') {
          verifyBearerAuth(request, token);
        }
        const response = await router.handle(request, {});
        if (response) return response;
        return jsonResponse({ error: 'API route not found', code: 'NOT_FOUND' }, 404);
      }

      if (request.method === 'GET' || request.method === 'HEAD') {
        const assetResponse = await serveWebAsset(url.pathname, { webAssets, readLiveWebFile });
        if (assetResponse) return assetResponse;
      }

      return textResponse('Not Found', 404);
    } catch (err) {
      if (err instanceof HttpError) {
        return jsonResponse({ error: err.message, code: err.code }, err.status);
      }
      console.error('[Skilld] Unhandled server error:', err);
      return jsonResponse({ error: err?.message || 'Internal Server Error', code: 'INTERNAL_ERROR' }, 500);
    }
  }

  return { fetch };
}

function verifyBearerAuth(request, expectedToken) {
  if (!expectedToken) {
    throw new HttpError(500, 'Server token is not configured', 'SERVER_MISCONFIGURED');
  }
  const header = request.headers.get('authorization') || '';
  const match = header.match(/^Bearer\s+(.+)$/i);
  if (!match || !constantTimeEqual(match[1], expectedToken)) {
    throw new HttpError(401, 'Unauthorized: invalid or missing Bearer token', 'UNAUTHORIZED');
  }
}

function constantTimeEqual(a, b) {
  const strA = String(a);
  const strB = String(b);
  const maxLen = Math.max(strA.length, strB.length);
  let diff = strA.length ^ strB.length;
  for (let i = 0; i < maxLen; i++) {
    diff |= (strA.charCodeAt(i) || 0) ^ (strB.charCodeAt(i) || 0);
  }
  return diff === 0;
}

function buildFilesInput(body) {
  if (body.files && typeof body.files === 'object') {
    return body.files;
  }
  if (typeof body.markdown === 'string') {
    return {
      'SKILL.md': { encoding: 'utf8', content: body.markdown, mode: '0644' },
    };
  }
  throw new HttpError(400, 'Either files map or markdown content is required', 'VALIDATION_ERROR');
}

async function allocateUniqueSkillId(store, name) {
  const base = slugifySkillId(name);
  let candidate = base;
  let suffix = 2;
  while (await store.getSkill(candidate, { includeVersions: false, includeCurrentFiles: false })) {
    candidate = `${base}-${suffix}`;
    suffix += 1;
  }
  return candidate;
}

async function requireInstallation(store, body, includeFiles = false) {
  const { deviceId, agentId, skillName } = body;
  if (!deviceId || !agentId || !skillName) {
    throw new HttpError(400, 'deviceId, agentId, and skillName are required', 'VALIDATION_ERROR');
  }
  const inst = await store.getInstallation(deviceId, agentId, skillName, { includeFiles });
  if (!inst) {
    throw new HttpError(404, 'Installation not found', 'INSTALLATION_NOT_FOUND');
  }
  return inst;
}

function omitFiles(inst) {
  if (!inst) return inst;
  const { actualFiles: _omitted, ...rest } = inst;
  return rest;
}

export async function checkSingleSkillUpstream({ store, skill, githubToken, fetchImpl }) {
  const parsed = parseGitHubUrl(skill.upstreamUrl, skill.upstreamRef, skill.upstreamSubpath);
  const info = await checkGitHubUpstream({
    owner: parsed.owner,
    repo: parsed.repo,
    ref: parsed.ref,
    subpath: parsed.subpath,
    token: githubToken,
    fetchImpl,
  });
  return store.updateSkillUpstream(skill.id, {
    upstreamLatestRev: info.latestRev,
    upstreamLatestMessage: info.latestMessage,
    upstreamCheckedAt: isoNow(),
  });
}

async function serveWebAsset(pathname, { webAssets, readLiveWebFile }) {
  const assetKey = pathname === '/' || pathname === '' ? '/index.html' : pathname;
  const contentTypes = {
    '/index.html': 'text/html; charset=utf-8',
    '/styles.css': 'text/css; charset=utf-8',
    '/app.mjs': 'application/javascript; charset=utf-8',
  };
  const contentType = contentTypes[assetKey];
  if (!contentType) return null;

  let body = null;
  if (typeof readLiveWebFile === 'function') {
    body = await readLiveWebFile(assetKey);
  }
  if (body === null && webAssets && typeof webAssets[assetKey] === 'string') {
    body = webAssets[assetKey];
  }
  if (body === null) return null;

  return new Response(body, {
    status: 200,
    headers: {
      'content-type': contentType,
      'cache-control': 'no-cache',
    },
  });
}
