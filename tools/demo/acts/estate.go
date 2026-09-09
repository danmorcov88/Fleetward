package acts

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The demo's estate.
//
// An estate view with three rows saying "never" demonstrates nothing, so the seeder builds twelve
// instances across three environments and six weeks of history. Every one of them is there to make
// a point, and the ordinary ones are there so the exceptions read as exceptions.
//
// Two things about this fixture are stated here, on screen while it runs, and in docs/demo.md,
// because a demo is the most dangerous document this repository could produce:
//
//   - **The history is seeded.** Backups that happened last Tuesday cannot be requested through an
//     API, and there should be no API for it. They are rows, inserted directly, and they carry
//     metadata only: no object exists behind a seeded artifact.
//   - **The endpoints are shared.** The stack contains exactly two reachable engines, so the nine
//     healthy instances point at those two under different names, and the two unreachable ones
//     point at a hostname reserved never to resolve. The green and red health cells are therefore
//     true — genuinely probed, not written into the column.
//
// Everything from act 3 onward is live.
const (
	demoEnvProduction  = "demo-production"
	demoEnvStaging     = "demo-staging"
	demoEnvDevelopment = "demo-development"

	// liveInstance is the one the live acts drive: a real backup, a real verification, a real
	// artifact whose real bytes are then really changed.
	liveInstance = "mssql-orders-prod"

	// The expectation every instance declares: a backup by 02:00, two hours of grace. This is the
	// "declare" half of declare-detect-gap, and it is a separate field from how often Fleetward
	// itself runs (ADR-0028).
	expectCron  = "0 2 * * *"
	expectGrace = 120 * time.Minute

	// What Fleetward's own schedule says, which is deliberately an occurrence the demo will not
	// reach: 03:00 on 1 January. A dozen backup jobs firing partway through a recording would
	// contend with the sandbox act 3 needs, and nothing in the demo depends on the scheduler
	// firing. The declared expectation above is what the product is being asked about.
	dormantCron = "0 3 1 1 *"

	demoViewerEmail = "demo-viewer@fleetward.dev"
	demoDBAEmail    = "demo-dba@fleetward.dev"
)

// historyKind is what an instance's seeded history is meant to demonstrate.
type historyKind int

const (
	// historyHealthy is the ordinary case: a nightly backup, taken and verified, for six weeks.
	historyHealthy historyKind = iota
	// historyObservedUnproven is a backup somebody else's cron took, from a source that cannot say
	// whether it finished. A file that arrived is not a backup that worked (ADR-0015).
	historyObservedUnproven
	// historyObserved is somebody else's backup, from a source that does report an outcome.
	historyObserved
	// historyBehind stopped three days ago, which is the gap the product exists to surface.
	historyBehind
	// historyUnverifiable has succeeded every night for a fortnight and failed verification every
	// night for thirteen of them — the case invisible to every tool that only checks whether the
	// job ran, and the reason retention has a floor (ADR-0032).
	historyUnverifiable
	// historyNone is an instance nothing can reach, so there is nothing to seed.
	historyNone
)

// seededInstance is one row of the fixture.
type seededInstance struct {
	name        string
	environment string
	engine      string
	host        string
	port        int32
	username    string
	password    string
	database    string
	shareEngine string // where the engine writes its backup file (ADR-0026)
	shareLocal  string // the same directory as the control plane sees it
	history     historyKind
	shows       string
}

// The two reachable engines in the development stack, as the control plane addresses them.
const (
	sqlHost = "sqlserver"
	sqlPort = 1433
	sqlUser = "sa"
	sqlPass = "Fleetward-dev-1" //nolint:gosec // G101: the development stack's own published credential
	sqlDB   = "fleetward_demo"
	// The named volume mounted into both the monitored server and the control plane. BACKUP
	// DATABASE writes to the server's own filesystem, and this is how the artifact is handed over
	// without a credential ever reaching the engine (ADR-0026).
	sqlShareEngine = "/var/opt/mssql/fleetward"
	sqlShareLocal  = "/app/share"

	pgHost = "postgres"
	pgPort = 5432
	pgUser = "fleetward"
	pgPass = "fleetward"
	pgDB   = "fleetward"
)

func sqlServerAt(name, env string, history historyKind, shows string) seededInstance {
	return seededInstance{
		name: name, environment: env, engine: "sqlserver",
		host: sqlHost, port: sqlPort, username: sqlUser, password: sqlPass, database: sqlDB,
		shareEngine: sqlShareEngine, shareLocal: sqlShareLocal,
		history: history, shows: shows,
	}
}

func postgresAt(name, env string, history historyKind, shows string) seededInstance {
	return seededInstance{
		name: name, environment: env, engine: "postgresql",
		host: pgHost, port: pgPort, username: pgUser, password: pgPass, database: pgDB,
		history: history, shows: shows,
	}
}

// estate is the fixture. One good fixture beats a knob nobody turns, so it is a literal rather
// than configuration.
var estate = []seededInstance{
	sqlServerAt(liveInstance, demoEnvProduction, historyHealthy, "the ordinary case, and the one the live acts drive"),
	sqlServerAt("mssql-billing-prod", demoEnvProduction, historyHealthy, "the ordinary case"),
	sqlServerAt("mssql-crm-staging", demoEnvStaging, historyHealthy, "the ordinary case"),
	postgresAt("pg-catalog-prod", demoEnvProduction, historyHealthy, "the ordinary case"),
	postgresAt("pg-identity-prod", demoEnvProduction, historyHealthy, "the ordinary case"),
	postgresAt("pg-analytics-staging", demoEnvStaging, historyHealthy, "the ordinary case"),

	postgresAt("pg-legacy-reports-prod", demoEnvProduction, historyObservedUnproven,
		"somebody else's cron, from a source that cannot say whether it finished"),
	postgresAt("pg-warehouse-dev", demoEnvDevelopment, historyObserved,
		"somebody else's cron, reported on without changing anything"),

	postgresAt("pg-invoices-prod", demoEnvProduction, historyBehind,
		"behind its window: the gap this product exists to surface"),
	sqlServerAt("mssql-archive-prod", demoEnvProduction, historyUnverifiable,
		"backing up nightly and failing verification nightly"),

	{
		name: "pg-hr-prod", environment: demoEnvProduction, engine: "postgresql",
		host: "pg-hr-prod.example.invalid", port: pgPort, username: pgUser, password: pgPass, database: pgDB,
		history: historyNone, shows: "unreachable, genuinely",
	},
	{
		name: "mssql-edi-staging", environment: demoEnvStaging, engine: "sqlserver",
		host: "mssql-edi-staging.example.invalid", port: sqlPort, username: sqlUser, password: sqlPass, database: sqlDB,
		history: historyNone, shows: "unreachable, genuinely",
	},
}

// seeded is what act 1 hands to the acts after it.
type seeded struct {
	environments map[string]string // demo environment name -> id
	instances    map[string]string // instance name -> id
	viewerToken  string
	dbaToken     string
}

// actEstate is act 1: an estate somebody can believe, built through the product's own API wherever
// there is one and by SQL exactly where there is not.
func actEstate(ctx context.Context, cfg Config, n *Narrator, c *client, db *metadb) (*seeded, error) {
	n.Act(1, "An estate you can believe",
		"Twelve instances across three environments, and six weeks of history. The ordinary "+
			"instances are here so the exceptions read as exceptions.")

	n.Step("clearing any estate a previous run left behind")
	if err := db.resetDemoEstate(ctx); err != nil {
		return nil, err
	}
	if err := seedMonitoredDatabase(ctx, cfg, n); err != nil {
		return nil, err
	}

	out := &seeded{environments: map[string]string{}, instances: map[string]string{}}
	for _, env := range []struct {
		name         string
		description  string
		isProduction bool
	}{
		{demoEnvProduction, "Production estate", true},
		{demoEnvStaging, "Pre-production", false},
		{demoEnvDevelopment, "Developer sandboxes", false},
	} {
		var resp struct {
			Environment environmentRow `json:"environment"`
		}
		body := map[string]any{"name": env.name, "description": env.description, "is_production": env.isProduction}
		if err := c.post(ctx, "/api/v1/environments", body, &resp); err != nil {
			return nil, fmt.Errorf("create environment %s: %w", env.name, err)
		}
		out.environments[env.name] = resp.Environment.ID
	}
	n.Step("three environments")

	for _, inst := range estate {
		id, err := createInstance(ctx, c, out.environments[inst.environment], inst)
		if err != nil {
			return nil, err
		}
		out.instances[inst.name] = id
		if err := declareSchedule(ctx, c, id, inst); err != nil {
			return nil, err
		}
	}
	n.Step("twelve instances, each declaring a backup by 02:00 with two hours of grace")

	// Health is probed rather than written. The two unreachable instances point at a hostname under
	// .example.invalid, which is reserved never to resolve, so the red cell is a real answer.
	n.Step("probing all twelve — the unreachable ones are genuinely unreachable")
	probeAll(ctx, c, out)

	n.Seeded("six weeks of backup history, inserted as rows: you cannot ask Fleetward to have taken " +
		"a backup last Tuesday, and there should be no API that lets you")
	counts, err := db.seedHistory(ctx, out)
	if err != nil {
		return nil, err
	}
	n.Seeded("%d backups, %d of them verified, %d observed rather than taken by Fleetward",
		counts.backups, counts.verified, counts.observed)

	// Every seeded backup keeps expires_at NULL except five on one instance. Those five are what
	// gives the retention sweep something to do while the demo runs, and act 6 something to refuse
	// to do.
	stamped, err := db.stampSeededExpiries(ctx, out.instances["mssql-archive-prod"])
	if err != nil {
		return nil, err
	}
	n.Seeded("%d of those expiries stamped into the past on mssql-archive-prod; every other seeded "+
		"backup has no expiry at all, which is what stops a sweep blanking the screen mid-demo", stamped)

	// Two credentials, because act 5 is the same request from two callers.
	if out.viewerToken, err = issueToken(ctx, c, demoViewerEmail, "viewer"); err != nil {
		return nil, err
	}
	if out.dbaToken, err = issueToken(ctx, c, demoDBAEmail, "dba"); err != nil {
		return nil, err
	}
	n.Step("two credentials issued: a tenant-wide viewer and a tenant-wide dba")

	// Read the estate back through the API rather than reporting what was just written. The health
	// column is the answer twelve probes actually returned, which is the only version of it worth
	// showing.
	listed, err := listInstances(ctx, c)
	if err != nil {
		return nil, err
	}
	health := map[string]string{}
	for _, inst := range demoInstances(listed, out) {
		health[inst.Name] = trimEnum("HEALTH_STATE_", inst.Health)
	}

	n.Say("")
	rows := make([][]string, 0, len(estate))
	up, unreachable := 0, 0
	for _, inst := range estate {
		state := health[inst.name]
		switch {
		case state == "UP":
			up++
		case inst.history == historyNone:
			unreachable++
		}
		rows = append(rows, []string{inst.name, inst.environment, inst.engine, state, inst.shows})
	}
	n.Table([]string{"INSTANCE", "ENVIRONMENT", "ENGINE", "HEALTH", "WHAT IT SHOWS"}, rows)

	if up < 6 {
		return nil, fmt.Errorf("only %d of the ten reachable instances answered a health probe; "+
			"the estate view has nothing ordinary to contrast the exceptions against", up)
	}
	if unreachable != 2 {
		return nil, fmt.Errorf("%d instances are unreachable, want the 2 pointed at a hostname "+
			"reserved never to resolve", unreachable)
	}

	n.Say("")
	n.Say("The estate view is at %s. The history above is seeded so the screen has something to "+
		"say; everything from act 3 onward is live.", webURL())
	n.Beat()
	return out, nil
}

// createInstance stores an instance and its credentials through the API, exactly as `fleetward-cli
// instance add` does. The password goes in the body and nowhere near a command line.
func createInstance(ctx context.Context, c *client, environmentID string, inst seededInstance) (string, error) {
	connection := map[string]any{
		"username": inst.username,
		"password": inst.password,
		"database": inst.database,
	}
	if inst.shareEngine != "" {
		connection["shared_directory"] = map[string]any{
			"engine_path": inst.shareEngine,
			"local_path":  inst.shareLocal,
		}
	}
	body := map[string]any{
		"environment_id": environmentID,
		"name":           inst.name,
		"engine_type":    inst.engine,
		"host":           inst.host,
		"port":           inst.port,
		"connection":     connection,
		"labels":         map[string]string{"fleetward.demo": "true"},
	}

	var resp struct {
		Instance instanceRow `json:"instance"`
	}
	if err := c.post(ctx, "/api/v1/instances", body, &resp); err != nil {
		return "", fmt.Errorf("create instance %s: %w", inst.name, err)
	}
	return resp.Instance.ID, nil
}

// declareSchedule writes down what a backup of this instance is supposed to look like.
//
// Two cron expressions, and they answer different questions (ADR-0028): `cron_expression` is how
// often Fleetward acts, `expected_cron` is what is supposed to have happened. Only the second one
// matters to the demo, and the first is deliberately dormant.
func declareSchedule(ctx context.Context, c *client, instanceID string, inst seededInstance) error {
	kind := "JOB_KIND_BACKUP"
	if inst.history == historyObserved || inst.history == historyObservedUnproven {
		kind = "JOB_KIND_OBSERVE"
	}
	body := map[string]any{
		"instance_id":            instanceID,
		"kind":                   kind,
		"cron_expression":        dormantCron,
		"timezone":               "UTC",
		"expected_cron":          expectCron,
		"expected_grace_minutes": int32(expectGrace / time.Minute),
		"verify_policy":          "VERIFY_POLICY_ALWAYS",
		"retention_days":         14,
	}
	if err := c.post(ctx, "/api/v1/instances/"+instanceID+"/schedules", body, nil); err != nil {
		return fmt.Errorf("declare a schedule for %s: %w", inst.name, err)
	}
	return nil
}

// probeAll calls TestConnection on every instance at once.
//
// Concurrently, because two of them are supposed not to answer and doing that serially would put a
// pair of resolver timeouts in the middle of the demo. A failure here is data, not an error: an
// instance that cannot be reached is exactly one of the rows the estate view exists to show.
func probeAll(ctx context.Context, c *client, s *seeded) {
	var wg sync.WaitGroup
	for _, inst := range estate {
		id := s.instances[inst.name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			_, _, _ = c.status(probeCtx, "POST", "/api/v1/instances/"+id+"/test-connection", map[string]any{})
		}()
	}
	wg.Wait()
}

// issueToken creates a user if it is new and returns a credential holding a tenant-wide role.
func issueToken(ctx context.Context, c *client, email, role string) (string, error) {
	var resp struct {
		Secret string `json:"secret"`
	}
	body := map[string]any{
		"email":        email,
		"role":         role,
		"display_name": email,
		"description":  "the Fleetward demo",
	}
	if err := c.post(ctx, "/api/v1/tokens", body, &resp); err != nil {
		return "", fmt.Errorf("issue a %s token: %w", role, err)
	}
	if resp.Secret == "" {
		return "", fmt.Errorf("the control plane issued a %s token with no secret in it", role)
	}
	return resp.Secret, nil
}

// listInstances reads the estate back through the API, which is how the demo asserts on health
// rather than on what it just wrote.
func listInstances(ctx context.Context, c *client) ([]instanceRow, error) {
	var resp struct {
		Instances []instanceRow `json:"instances"`
	}
	if err := c.get(ctx, "/api/v1/instances", url.Values{"page_size": {"100"}}, &resp); err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	return resp.Instances, nil
}

// demoInstances filters a listing to the fixture, so a stack somebody has already been using does
// not make the demo's own assertions ambiguous.
func demoInstances(all []instanceRow, s *seeded) []instanceRow {
	mine := map[string]bool{}
	for _, id := range s.instances {
		mine[id] = true
	}
	out := make([]instanceRow, 0, len(s.instances))
	for _, inst := range all {
		if mine[inst.ID] {
			out = append(out, inst)
		}
	}
	return out
}

// methodFor is the backup method each engine's plugin declares as its default. Named here only so
// the seeded rows carry the same vocabulary a real one would; core never branches on it.
func methodFor(engine string) string {
	if strings.EqualFold(engine, "sqlserver") {
		return "full"
	}
	return "pg_dump"
}
