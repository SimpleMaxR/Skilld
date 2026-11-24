package skill

const (
	InstallStatusSynced    = "synced"
	InstallStatusOutdated  = "outdated"
	InstallStatusModified  = "modified"
	InstallStatusUnmanaged = "unmanaged"
	InstallStatusMissing   = "missing"

	ActionInstall      = "install"
	ActionUpdate       = "update"
	ActionReset        = "reset"
	ActionRemove       = "remove"
	ActionAdoptConfirm = "adopt_confirm"
	ActionForgetState  = "forget_state"

	ActionStatusPending   = "pending"
	ActionStatusClaimed   = "claimed"
	ActionStatusCompleted = "completed"
	ActionStatusFailed    = "failed"
	ActionStatusCancelled = "cancelled"
)

// ComputeInstallationStatus implements the pure 3-way hash drift state machine.
func ComputeInstallationStatus(baseHash, actualHash, desiredHash string, isManaged bool) string {
	managed := isManaged || baseHash != ""
	if !managed {
		if actualHash != "" {
			return InstallStatusUnmanaged
		}
		return InstallStatusMissing
	}
	if actualHash == "" {
		return InstallStatusMissing
	}
	if baseHash != "" && actualHash != baseHash {
		return InstallStatusModified
	}
	if desiredHash != "" && actualHash != desiredHash {
		return InstallStatusOutdated
	}
	return InstallStatusSynced
}
