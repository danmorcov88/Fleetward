package sdk

import (
	"errors"
	"fmt"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// ValidateCapabilities checks a capability matrix for internal consistency.
//
// This runs on the plugin side, before the matrix ever reaches core, so that a contradiction is
// caught at the source rather than turning into a confusing failure hours later when a scheduled
// backup tries to use a method that does not exist.
//
// It checks coherence, not honesty. Nothing here can tell whether a plugin that claims
// supports_pitr actually delivers it — that is what the conformance suite is for (ADR-0012).
func ValidateCapabilities(caps *fwv1.Capabilities) error {
	if caps == nil {
		return InvalidArgument("capabilities are nil")
	}

	var errs []error

	if caps.GetEngineType() == "" {
		errs = append(errs, errors.New("engine_type is required"))
	}
	if caps.GetPluginVersion() == "" {
		errs = append(errs, errors.New("plugin_version is required"))
	}

	methodIDs := make(map[string]bool, len(caps.GetBackupMethods()))
	defaults := 0
	pitrCapableMethods := 0
	onlineMethods := 0

	for i, m := range caps.GetBackupMethods() {
		switch {
		case m.GetId() == "":
			errs = append(errs, fmt.Errorf("backup_methods[%d]: id is required", i))
		case methodIDs[m.GetId()]:
			errs = append(errs, fmt.Errorf("backup_methods[%d]: duplicate id %q", i, m.GetId()))
		default:
			methodIDs[m.GetId()] = true
		}

		if m.GetKind() == fwv1.BackupKind_BACKUP_KIND_UNSPECIFIED {
			errs = append(errs, fmt.Errorf("backup_methods[%q]: kind is required", m.GetId()))
		}
		if m.GetIsDefault() {
			defaults++
		}
		if m.GetEnablesPitr() {
			pitrCapableMethods++
		}
		if !m.GetRequiresDowntime() {
			onlineMethods++
		}

		for j, opt := range m.GetOptions() {
			if opt.GetName() == "" {
				errs = append(errs, fmt.Errorf("backup_methods[%q].options[%d]: name is required", m.GetId(), j))
			}
			if opt.GetType() == fwv1.OptionType_OPTION_TYPE_ENUM && len(opt.GetAllowedValues()) == 0 {
				errs = append(errs, fmt.Errorf(
					"backup_methods[%q].options[%q]: enum option must declare allowed_values",
					m.GetId(), opt.GetName()))
			}
		}
	}

	if len(caps.GetBackupMethods()) > 0 && defaults != 1 {
		errs = append(errs, fmt.Errorf(
			"exactly one backup method must set is_default, found %d", defaults))
	}

	// supports_online_backup is the flag core reads before scheduling a backup against a live
	// production server. A plugin that sets it without offering a method that runs without downtime
	// would have core scheduling an outage it never warned anybody about.
	if caps.GetSupportsOnlineBackup() && onlineMethods == 0 {
		errs = append(errs, errors.New(
			"supports_online_backup is set but every backup method requires downtime"))
	}

	// A plugin that advertises point-in-time recovery but has no method that can produce a
	// baseline would leave core scheduling backups that can never satisfy a PITR request.
	if caps.GetSupportsPitr() && pitrCapableMethods == 0 {
		errs = append(errs, errors.New(
			"supports_pitr is set but no backup method sets enables_pitr"))
	}
	if caps.GetSupportsPointInTimeRestore() && !caps.GetSupportsPitr() {
		errs = append(errs, errors.New(
			"supports_point_in_time_restore requires supports_pitr"))
	}

	// Verification is the product. A plugin that cannot restore into a sandbox cannot be verified,
	// and declaring verification checks it can never run would make core report verification as
	// failing rather than as unavailable.
	if len(caps.GetSupportedVerificationChecks()) > 0 && !caps.GetSupportsSandboxRestore() {
		errs = append(errs, errors.New(
			"verification checks are declared but supports_sandbox_restore is false"))
	}
	if caps.GetSupportsSandboxRestore() {
		if caps.GetSandboxTemplate().GetImageRepository() == "" {
			errs = append(errs, errors.New(
				"supports_sandbox_restore requires sandbox_template.image_repository"))
		}
		if caps.GetSandboxTemplate().GetContainerPort() == 0 {
			errs = append(errs, errors.New(
				"supports_sandbox_restore requires sandbox_template.container_port"))
		}
	}

	if caps.GetSupportsReplicationLag() && !caps.GetSupportsReplication() {
		errs = append(errs, errors.New("supports_replication_lag requires supports_replication"))
	}

	seenMetrics := make(map[string]bool, len(caps.GetMetrics()))
	for i, m := range caps.GetMetrics() {
		if m.GetName() == "" {
			errs = append(errs, fmt.Errorf("metrics[%d]: name is required", i))
			continue
		}
		if seenMetrics[m.GetName()] {
			errs = append(errs, fmt.Errorf("metrics[%d]: duplicate name %q", i, m.GetName()))
		}
		seenMetrics[m.GetName()] = true
		if m.GetType() == fwv1.MetricType_METRIC_TYPE_UNSPECIFIED {
			errs = append(errs, fmt.Errorf("metrics[%q]: type is required", m.GetName()))
		}
	}

	if len(errs) > 0 {
		return InvalidArgument("invalid capabilities: %v", errors.Join(errs...))
	}
	return nil
}

// DefaultBackupMethod returns the method marked default, or the first one if none is marked.
func DefaultBackupMethod(caps *fwv1.Capabilities) *fwv1.BackupMethod {
	methods := caps.GetBackupMethods()
	for _, m := range methods {
		if m.GetIsDefault() {
			return m
		}
	}
	if len(methods) > 0 {
		return methods[0]
	}
	return nil
}

// FindBackupMethod returns the method with the given id, or nil.
func FindBackupMethod(caps *fwv1.Capabilities, id string) *fwv1.BackupMethod {
	for _, m := range caps.GetBackupMethods() {
		if m.GetId() == id {
			return m
		}
	}
	return nil
}

// SupportsCheck reports whether the plugin implements a verification check.
func SupportsCheck(caps *fwv1.Capabilities, check fwv1.VerificationCheck) bool {
	for _, c := range caps.GetSupportedVerificationChecks() {
		if c == check {
			return true
		}
	}
	return false
}
