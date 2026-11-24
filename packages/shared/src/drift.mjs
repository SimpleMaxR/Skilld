import { INSTALL_STATUS } from './protocol.mjs';

/**
 * Pure 3-way hash drift state machine for a single (device, agent, skill) installation.
 *
 * @param {object} params
 * @param {string|null|undefined} params.baseHash - Hash last synced/adopted by Skilld (from state.json)
 * @param {string|null|undefined} params.actualHash - Hash currently scanned on disk (null if folder missing)
 * @param {string|null|undefined} params.desiredHash - Hash of target Vault version
 * @param {boolean} params.isManaged - Whether this installation is linked to a Vault skill or local ledger
 * @returns {'synced'|'outdated'|'modified'|'unmanaged'|'missing'}
 */
export function computeInstallationStatus({ baseHash, actualHash, desiredHash, isManaged }) {
  const managed = Boolean(isManaged || baseHash);

  if (!managed) {
    return actualHash ? INSTALL_STATUS.UNMANAGED : INSTALL_STATUS.MISSING;
  }

  if (!actualHash) {
    return INSTALL_STATUS.MISSING;
  }

  if (baseHash && actualHash !== baseHash) {
    return INSTALL_STATUS.MODIFIED;
  }

  if (desiredHash && actualHash !== desiredHash) {
    return INSTALL_STATUS.OUTDATED;
  }

  return INSTALL_STATUS.SYNCED;
}
