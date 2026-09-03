// Package authz decides what a caller may do.
//
// The shape here is deliberate and is the subject of ADR-0035. Three things make a route that
// forgets to check fail loudly rather than quietly, which is what B5's CHECK constraint does for
// retention:
//
//  1. Compile time. The decorators in this package do not embed the generated
//     `UnimplementedXServiceServer`, so `var _ fwv1.InventoryServiceServer = (*inventoryGuard)(nil)`
//     stops building the moment the contract grows a method. The mistake is refused by the
//     toolchain rather than caught by a reviewer.
//  2. A fail-closed table. Check on a method with no entry in Policies returns PermissionDenied. A
//     new RPC is denied to everybody until somebody writes down what it needs.
//  3. The coverage test ADR-0024 §4 asked for, which enumerates every method on every generated
//     service interface by reflection and asserts each one has a policy and refuses an anonymous
//     caller.
//
// The other decision worth knowing before reading further: **scope comes from the request, and a
// request that names no scope is asking about the whole tenant.** `ListBackups` with an
// `instance_id` is a question about one instance and an instance-scoped grant answers it;
// `ListBackups` with nothing set is a question about the estate and needs a grant that covers the
// estate. That one rule replaces what would otherwise have been per-RPC special cases, and it means
// no list endpoint can leak a row from outside the caller's scope.
package authz

import (
	"fmt"
	"sort"
)

// The four seeded roles, by name. The *names* are constants because they are vocabulary — they
// appear in the CLI, in the API and in this table. The *ranks* are not: they live in the `roles`
// table, seeded by migration 000001, and are read from it at startup. A Go constant that disagreed
// with that table would be a bug nothing surfaced until somebody edited one of the two.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleDBA      = "dba"
	RoleAdmin    = "admin"
)

// ScopeSource says where a request's scope comes from.
type ScopeSource int

const (
	// ScopeTenant means the request is about the whole tenant, so only a tenant-wide grant covers
	// it. Used both for RPCs that have no scope field at all and for those whose scope field the
	// caller left empty.
	ScopeTenant ScopeSource = iota
	// ScopeRequestInstance reads `instance_id` from the request message.
	ScopeRequestInstance
	// ScopeRequestEnvironment reads `environment_id` from the request message.
	ScopeRequestEnvironment
	// ScopeRequestInstanceOrEnvironment reads whichever of the two the caller supplied, preferring
	// the instance. Both empty means the estate.
	ScopeRequestInstanceOrEnvironment
	// ScopeBackup reads `backup_id` and resolves it to the instance that backup belongs to.
	ScopeBackup
	// ScopeSchedule reads `schedule_id` and resolves it to its instance.
	ScopeSchedule
	// ScopeVerification reads `verification_id` and resolves it through its backup to an instance.
	ScopeVerification
)

// Rule is what one RPC requires.
type Rule struct {
	// MinRole is the least role that may call this method within the scope it acts on.
	MinRole string
	// Scope says where to find what the method acts on.
	Scope ScopeSource
	// Mutating marks a method that changes something. Every mutating call produces an audit record
	// on both outcomes; a refusal of one is the most interesting row in the log (ADR-0035).
	Mutating bool
	// AuditRead marks a read that is itself worth recording. Exactly one method sets it today:
	// listing who has access to a monitored database is a security-relevant act even though it
	// changes nothing.
	AuditRead bool
	// Action is what lands in audit_log.action. Set on everything audited.
	Action string
	// ResourceType is what lands in audit_log.resource_type.
	ResourceType string
	// AnyAuthenticated exempts a method from the role check, but not from authentication. Exactly
	// one method sets it: any caller may ask who it is, and a caller who cannot ask cannot be shown
	// a useful error either.
	AnyAuthenticated bool
}

// Policies is the whole authorization surface of the control plane, in one place a reviewer can
// read top to bottom.
//
// Keys are gRPC full method names, which is what the generated code and the decorators both speak.
// The minimum roles come from what migration 000001 said each seeded role is for: operator "may
// acknowledge alerts and trigger discovery, cannot back up or restore"; dba "may run backups,
// verifications, and restores within the granted scope"; admin "full control including user, role,
// and instance administration".
var Policies = map[string]Rule{
	// --- Inventory -------------------------------------------------------------------------------
	"/fleetward.v1.InventoryService/ListEnvironments": {
		MinRole: RoleViewer, Scope: ScopeTenant,
		Action: "environment.list", ResourceType: "environment",
	},
	"/fleetward.v1.InventoryService/CreateEnvironment": {
		MinRole: RoleAdmin, Scope: ScopeTenant, Mutating: true,
		Action: "environment.create", ResourceType: "environment",
	},
	"/fleetward.v1.InventoryService/ListInstances": {
		MinRole: RoleViewer, Scope: ScopeRequestEnvironment,
		Action: "instance.list", ResourceType: "instance",
	},
	"/fleetward.v1.InventoryService/GetInstance": {
		MinRole: RoleViewer, Scope: ScopeRequestInstance,
		Action: "instance.get", ResourceType: "instance",
	},
	// Adding an instance stores a credential for a production database, which is administration
	// rather than operation.
	"/fleetward.v1.InventoryService/CreateInstance": {
		MinRole: RoleAdmin, Scope: ScopeRequestEnvironment, Mutating: true,
		Action: "instance.create", ResourceType: "instance",
	},
	"/fleetward.v1.InventoryService/DeleteInstance": {
		MinRole: RoleAdmin, Scope: ScopeRequestInstance, Mutating: true,
		Action: "instance.delete", ResourceType: "instance",
	},
	// TestConnection and DiscoverInstance both reach out to the monitored instance and write
	// health back. Mutating in the audit sense — they change a row — and operator's stated job.
	"/fleetward.v1.InventoryService/TestConnection": {
		MinRole: RoleOperator, Scope: ScopeRequestInstance, Mutating: true,
		Action: "instance.test_connection", ResourceType: "instance",
	},
	"/fleetward.v1.InventoryService/DiscoverInstance": {
		MinRole: RoleOperator, Scope: ScopeRequestInstance, Mutating: true,
		Action: "instance.discover", ResourceType: "instance",
	},
	// Reading who has access to a monitored database changes nothing and is still worth a record.
	"/fleetward.v1.InventoryService/ListPrincipalsForInstance": {
		MinRole: RoleDBA, Scope: ScopeRequestInstance, AuditRead: true,
		Action: "instance.list_principals", ResourceType: "instance",
	},

	// --- Schedules -------------------------------------------------------------------------------
	"/fleetward.v1.ScheduleService/ListSchedules": {
		MinRole: RoleViewer, Scope: ScopeRequestInstance,
		Action: "schedule.list", ResourceType: "schedule",
	},
	"/fleetward.v1.ScheduleService/GetSchedule": {
		MinRole: RoleViewer, Scope: ScopeSchedule,
		Action: "schedule.get", ResourceType: "schedule",
	},
	"/fleetward.v1.ScheduleService/CreateSchedule": {
		MinRole: RoleDBA, Scope: ScopeRequestInstance, Mutating: true,
		Action: "schedule.create", ResourceType: "schedule",
	},
	"/fleetward.v1.ScheduleService/SetScheduleEnabled": {
		MinRole: RoleDBA, Scope: ScopeSchedule, Mutating: true,
		Action: "schedule.set_enabled", ResourceType: "schedule",
	},
	"/fleetward.v1.ScheduleService/DeleteSchedule": {
		MinRole: RoleDBA, Scope: ScopeSchedule, Mutating: true,
		Action: "schedule.delete", ResourceType: "schedule",
	},
	"/fleetward.v1.ScheduleService/ListJobs": {
		MinRole: RoleViewer, Scope: ScopeRequestInstance,
		Action: "job.list", ResourceType: "job",
	},

	// --- Backups ---------------------------------------------------------------------------------
	"/fleetward.v1.BackupService/ListBackups": {
		MinRole: RoleViewer, Scope: ScopeRequestInstanceOrEnvironment,
		Action: "backup.list", ResourceType: "backup",
	},
	"/fleetward.v1.BackupService/GetBackup": {
		MinRole: RoleViewer, Scope: ScopeBackup,
		Action: "backup.get", ResourceType: "backup",
	},
	// §7.5 of CLAUDE.md, the acceptance criterion this whole slice exists for: a viewer cannot
	// trigger a backup, a dba can, and both attempts land in the audit log.
	"/fleetward.v1.BackupService/RunBackup": {
		MinRole: RoleDBA, Scope: ScopeRequestInstance, Mutating: true,
		Action: "backup.run", ResourceType: "instance",
	},
	"/fleetward.v1.BackupService/RunVerification": {
		MinRole: RoleDBA, Scope: ScopeBackup, Mutating: true,
		Action: "verification.run", ResourceType: "backup",
	},
	"/fleetward.v1.BackupService/GetVerification": {
		MinRole: RoleViewer, Scope: ScopeVerification,
		Action: "verification.get", ResourceType: "verification",
	},
	"/fleetward.v1.BackupService/GetPITRWindow": {
		MinRole: RoleViewer, Scope: ScopeRequestInstance,
		Action: "instance.get_pitr_window", ResourceType: "instance",
	},
	"/fleetward.v1.BackupService/ObserveBackupHistory": {
		MinRole: RoleOperator, Scope: ScopeRequestInstance, Mutating: true,
		Action: "backup.observe", ResourceType: "instance",
	},
	"/fleetward.v1.BackupService/GetBackupAdherence": {
		MinRole: RoleViewer, Scope: ScopeRequestInstanceOrEnvironment,
		Action: "backup.get_adherence", ResourceType: "instance",
	},
	// Reading what the next sweep would destroy is a dba question, not a viewer's. It changes
	// nothing, and it is the closest thing this product has to a list of what is about to be lost.
	"/fleetward.v1.BackupService/PreviewRetention": {
		MinRole: RoleDBA, Scope: ScopeRequestInstance,
		Action: "backup.preview_retention", ResourceType: "backup",
	},

	// --- Identity --------------------------------------------------------------------------------
	// Any caller may ask who it is. A caller that cannot ask cannot be shown a useful error either.
	"/fleetward.v1.IdentityService/GetMe": {
		AnyAuthenticated: true,
		Action:           "identity.get_me", ResourceType: "caller",
	},
	"/fleetward.v1.IdentityService/CreateToken": {
		MinRole: RoleAdmin, Scope: ScopeTenant, Mutating: true,
		Action: "token.create", ResourceType: "api_token",
	},
	"/fleetward.v1.IdentityService/ListTokens": {
		MinRole: RoleAdmin, Scope: ScopeTenant,
		Action: "token.list", ResourceType: "api_token",
	},
	"/fleetward.v1.IdentityService/RevokeToken": {
		MinRole: RoleAdmin, Scope: ScopeTenant, Mutating: true,
		Action: "token.revoke", ResourceType: "api_token",
	},
	"/fleetward.v1.IdentityService/ListAuditLog": {
		MinRole: RoleAdmin, Scope: ScopeTenant,
		Action: "audit.list", ResourceType: "audit_log",
	},
}

// MethodNames returns every method the policy covers, sorted. Used by the coverage test and by the
// startup validation.
func MethodNames() []string {
	names := make([]string, 0, len(Policies))
	for name := range Policies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ValidatePolicies checks the table against the role ranks the database actually holds.
//
// Called at startup so that a policy naming a role the `roles` table does not contain refuses to
// serve rather than refusing every request at runtime, one confusing 403 at a time.
func ValidatePolicies(ranks map[string]int) error {
	for _, name := range MethodNames() {
		rule := Policies[name]
		// Every rule carries an action, including the read-only ones. A refusal is audited whatever
		// the method was — a viewer refused a retention preview is a real principal reaching for
		// something they may not have — so every method needs a word to record it under.
		if rule.Action == "" || rule.ResourceType == "" {
			return fmt.Errorf("authz: %s has no audit action or resource type", name)
		}
		if rule.AnyAuthenticated {
			continue
		}
		if rule.MinRole == "" {
			return fmt.Errorf("authz: %s has no minimum role and is not AnyAuthenticated", name)
		}
		if _, ok := ranks[rule.MinRole]; !ok {
			return fmt.Errorf("authz: %s requires role %q, which the roles table does not contain",
				name, rule.MinRole)
		}
	}
	return nil
}
