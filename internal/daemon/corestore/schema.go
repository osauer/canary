package corestore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	applicationID = 0x49424b52 // "IBKR"
)

type migration struct {
	version    int
	name       string
	statements []string
	// destructive, when set, is the operator-approved exception that lets this
	// one migration run the destructive statements it names. Nil for every
	// migration that needs no exception, which is the normal case.
	destructive *destructiveApproval
	// maintenance is frozen execution metadata for a migration whose effect
	// for ordinary migrations.
	maintenance *migrationMaintenance
}

// migrationMaintenance binds one exact, typed observation discard to its
// DELETE by validation and must be covered by the same migration's exact
type migrationMaintenance struct {
	ObservationDiscard *ObservationDiscardSelector
	EventDiscard       *EventDiscardSelector
	OperationalPrune   *OperationalPruneSelector
	CompactCandidate   bool
	RetireSourceBackup bool
	// PreserveAuthorityHead is an explicit exception to the normal out-of-place
	// upgrade rule. It is valid only for a maintenance migration whose statements
	// only when every pending migration carries this reviewed exception.
	PreserveAuthorityHead bool
}

// destructiveApproval is a narrow, audited exception to the destructive
// statement guard in validateMigrationStatements. It names the exact
// statements one migration may run and records, in prose a human wrote, what
// the exception costs and why it was accepted. The exception never generalizes:
// approval that names a statement the migration does not run, or a statement
// cannot quietly outlive the migration it was written for.
type destructiveApproval struct {
	reason     string
	statements []string
}

type schemaObject struct {
	typeName string
	name     string
	table    string
	sql      string
}

// migrations is the ordered migration plan and len(migrations) is the current
// schema version. It is completed in init(): migration 1's append-only trigger
// statements are generated, and every migration after it is appended there.
var migrations = []migration{{
	version: 1,
	name:    "authoritative_foundation",
	statements: []string{
		`CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  checksum TEXT NOT NULL,
  applied_at TEXT NOT NULL
) STRICT`,
		`CREATE TABLE store_meta (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  authority_epoch TEXT NOT NULL,
  head_generation INTEGER NOT NULL DEFAULT 0 CHECK (head_generation >= 0),
  last_event_seq INTEGER NOT NULL DEFAULT 0 CHECK (last_event_seq >= 0),
  signer_generation INTEGER NOT NULL DEFAULT 1 CHECK (signer_generation >= 1),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
) STRICT`,
		`CREATE TABLE legacy_imports (
  scope_key TEXT NOT NULL,
  source_kind TEXT NOT NULL,
  source_fingerprint TEXT NOT NULL,
  status TEXT NOT NULL,
  imported_through TEXT,
  details_json BLOB CHECK (details_json IS NULL OR json_valid(details_json)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (scope_key, source_kind, source_fingerprint)
) STRICT`,
		`CREATE UNIQUE INDEX legacy_import_once ON legacy_imports(scope_key, source_kind)`,
		`CREATE TABLE state_documents (
  scope_key TEXT NOT NULL,
  kind TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK (revision >= 1),
  document_json BLOB NOT NULL CHECK (json_valid(document_json)),
  document_sha256 BLOB NOT NULL CHECK (length(document_sha256) = 32),
  updated_at TEXT NOT NULL,
  PRIMARY KEY (scope_key, kind)
) STRICT`,
		`CREATE TABLE broker_scopes (
  scope_key TEXT PRIMARY KEY,
  endpoint TEXT NOT NULL,
  client_id INTEGER NOT NULL CHECK (client_id >= 0),
  account TEXT NOT NULL,
  mode TEXT NOT NULL,
  binding_sha256 BLOB NOT NULL UNIQUE CHECK (length(binding_sha256) = 32),
  created_at TEXT NOT NULL
) STRICT`,
		`CREATE TABLE event_log (
  event_seq INTEGER PRIMARY KEY AUTOINCREMENT,
  scope_key TEXT NOT NULL,
  event_key TEXT NOT NULL,
  event_type TEXT NOT NULL,
  action_kind TEXT NOT NULL,
  origin TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  occurred_at_ms INTEGER NOT NULL,
  recorded_at TEXT NOT NULL,
  payload_json BLOB NOT NULL CHECK (json_valid(payload_json)),
  payload_sha256 BLOB NOT NULL CHECK (length(payload_sha256) = 32),
  UNIQUE (scope_key, event_key)
) STRICT`,
		`CREATE INDEX event_log_scope_time ON event_log(scope_key, occurred_at_ms, event_seq)`,
		`CREATE TABLE observations (
  observation_id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope_key TEXT NOT NULL,
  source TEXT NOT NULL,
  kind TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  observed_at_ms INTEGER NOT NULL,
  recorded_at TEXT NOT NULL,
  content_type TEXT NOT NULL,
  payload BLOB NOT NULL,
  payload_sha256 BLOB NOT NULL CHECK (length(payload_sha256) = 32),
  metadata_json BLOB CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
  decision_eligible INTEGER NOT NULL CHECK (decision_eligible IN (0, 1))
) STRICT`,
		`CREATE INDEX observations_scope_time ON observations(scope_key, kind, observed_at_ms, observation_id)`,
		`CREATE TABLE consumed_preview_tokens (
  token_digest BLOB PRIMARY KEY CHECK (length(token_digest) = 32),
  scope_key TEXT NOT NULL REFERENCES broker_scopes(scope_key),
  authority_epoch TEXT NOT NULL,
  signer_generation INTEGER NOT NULL CHECK (signer_generation >= 1),
  head_generation INTEGER NOT NULL CHECK (head_generation >= 1),
  consumed_at TEXT NOT NULL
) STRICT`,
		`CREATE TABLE order_id_floors (
  floor_scope TEXT NOT NULL CHECK (floor_scope IN ('global', 'broker')),
  scope_key TEXT NOT NULL,
  floor INTEGER NOT NULL CHECK (floor >= 0),
  updated_at TEXT NOT NULL,
  PRIMARY KEY (floor_scope, scope_key),
  CHECK ((floor_scope = 'global' AND scope_key = '') OR (floor_scope = 'broker' AND scope_key <> ''))
) STRICT`,
		`CREATE TABLE regime_decisions (
  event_seq INTEGER PRIMARY KEY REFERENCES event_log(event_seq),
  scope_key TEXT NOT NULL,
  decision_key TEXT NOT NULL,
  stage TEXT NOT NULL,
  severity TEXT,
  readiness TEXT,
  confidence TEXT,
  verdict TEXT,
  fingerprint TEXT,
  UNIQUE (scope_key, decision_key)
) STRICT`,
		`CREATE TABLE regime_indicators (
  decision_event_seq INTEGER NOT NULL REFERENCES regime_decisions(event_seq),
  indicator TEXT NOT NULL,
  status TEXT,
  band TEXT,
  value REAL,
  depth REAL,
  streak_sessions INTEGER,
  freshness TEXT,
  eligible INTEGER CHECK (eligible IS NULL OR eligible IN (0, 1)),
  latched INTEGER NOT NULL DEFAULT 0 CHECK (latched IN (0, 1)),
  thresholds_label TEXT,
  PRIMARY KEY (decision_event_seq, indicator)
) STRICT`,
		`CREATE TABLE rule_transitions (
  event_seq INTEGER PRIMARY KEY REFERENCES event_log(event_seq),
  scope_key TEXT NOT NULL,
  rule_id TEXT NOT NULL,
  status TEXT NOT NULL,
  previous_status TEXT,
  policy_id TEXT,
  policy_version INTEGER,
  policy_fingerprint TEXT
) STRICT`,
		`CREATE TABLE canary_transitions (
  event_seq INTEGER PRIMARY KEY REFERENCES event_log(event_seq),
  scope_key TEXT NOT NULL,
  action TEXT NOT NULL,
  severity TEXT,
  direction TEXT,
  market_stage TEXT,
  input_health TEXT,
  portfolio_alert_relevant INTEGER CHECK (portfolio_alert_relevant IS NULL OR portfolio_alert_relevant IN (0, 1))
) STRICT`,
		`CREATE TABLE capital_events (
  event_seq INTEGER PRIMARY KEY REFERENCES event_log(event_seq),
  scope_key TEXT NOT NULL,
  kind TEXT NOT NULL,
  amount_base_text TEXT,
  effective_at TEXT,
  report_id TEXT
) STRICT`,
		`CREATE TABLE risk_policy_events (
  event_seq INTEGER PRIMARY KEY REFERENCES event_log(event_seq),
  scope_key TEXT NOT NULL,
  kind TEXT NOT NULL,
  policy_id TEXT,
  policy_version INTEGER,
  policy_fingerprint TEXT
) STRICT`,
		`CREATE TABLE proposal_outcomes (
  event_seq INTEGER PRIMARY KEY REFERENCES event_log(event_seq),
  scope_key TEXT NOT NULL,
  proposal_key TEXT NOT NULL,
  revision TEXT,
  bucket TEXT,
  symbol TEXT,
  sec_type TEXT,
  action TEXT,
  state TEXT NOT NULL
) STRICT`,
		`CREATE TABLE order_events (
  event_seq INTEGER PRIMARY KEY REFERENCES event_log(event_seq),
  scope_key TEXT NOT NULL REFERENCES broker_scopes(scope_key),
  batch_ordinal INTEGER NOT NULL CHECK (batch_ordinal >= 0),
  type TEXT NOT NULL,
  order_ref TEXT,
  preview_token_id TEXT,
  reserved_order_id INTEGER,
  perm_id INTEGER,
  status TEXT,
  token_digest BLOB CHECK (token_digest IS NULL OR length(token_digest) = 32)
) STRICT`,
		`CREATE INDEX order_events_scope_seq ON order_events(scope_key, event_seq)`,
		`CREATE INDEX order_events_ref ON order_events(scope_key, order_ref, event_seq) WHERE order_ref IS NOT NULL`,
		`CREATE INDEX order_events_reserved ON order_events(scope_key, reserved_order_id, event_seq) WHERE reserved_order_id IS NOT NULL`,
		`CREATE INDEX order_events_perm ON order_events(scope_key, perm_id, event_seq) WHERE perm_id IS NOT NULL`,
		`CREATE INDEX order_events_token ON order_events(scope_key, preview_token_id, event_seq) WHERE preview_token_id IS NOT NULL`,
		`CREATE TABLE statement_files (
  scope_key TEXT NOT NULL,
  file_key TEXT NOT NULL,
  size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
  sha256 BLOB NOT NULL CHECK (length(sha256) = 32),
  status TEXT NOT NULL,
  statement_generated_at TEXT,
  ingested_at TEXT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (scope_key, file_key),
  UNIQUE (scope_key, file_key, sha256)
) STRICT`,
		`CREATE TABLE statement_file_versions (
  scope_key TEXT NOT NULL,
  file_key TEXT NOT NULL,
  sha256 BLOB NOT NULL CHECK (length(sha256) = 32),
  size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
  status TEXT NOT NULL,
  statement_generated_at TEXT,
  ingested_at TEXT,
  recorded_at TEXT NOT NULL,
  PRIMARY KEY (scope_key, file_key, sha256)
) STRICT`,
		`CREATE TABLE statement_equity_day_versions (
  equity_version_id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope_key TEXT NOT NULL,
  account_key TEXT NOT NULL,
  day TEXT NOT NULL,
  equity_base_text TEXT NOT NULL,
  statement_file_key TEXT NOT NULL,
  statement_file_sha256 BLOB NOT NULL CHECK (length(statement_file_sha256) = 32),
  generated_at TEXT NOT NULL,
  raw_json BLOB NOT NULL CHECK (json_valid(raw_json)),
  raw_sha256 BLOB NOT NULL CHECK (length(raw_sha256) = 32),
  recorded_at TEXT NOT NULL,
  FOREIGN KEY (scope_key, statement_file_key, statement_file_sha256)
    REFERENCES statement_file_versions(scope_key, file_key, sha256),
  UNIQUE (scope_key, account_key, day, statement_file_key, statement_file_sha256, generated_at, equity_base_text, raw_sha256)
) STRICT`,
		`CREATE TABLE statement_equity_days (
  equity_day_id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope_key TEXT NOT NULL,
  account_key TEXT NOT NULL,
  day TEXT NOT NULL,
  equity_base_text TEXT NOT NULL,
  statement_file_key TEXT NOT NULL,
  statement_file_sha256 BLOB NOT NULL CHECK (length(statement_file_sha256) = 32),
  generated_at TEXT NOT NULL,
  raw_json BLOB NOT NULL CHECK (json_valid(raw_json)),
  updated_at TEXT NOT NULL,
  FOREIGN KEY (scope_key, statement_file_key, statement_file_sha256)
    REFERENCES statement_files(scope_key, file_key, sha256),
  UNIQUE (scope_key, account_key, day)
) STRICT`,
		`CREATE INDEX statement_equity_days_scope_day ON statement_equity_days(scope_key, day, equity_day_id)`,
		`CREATE INDEX statement_equity_versions_scope_day ON statement_equity_day_versions(scope_key, day, equity_version_id)`,
	},
}}

// migrationV1AppendOnlyTables is the append-only set exactly as migration 1
// migrationChecksum hashes them, so every name here is frozen: editing one
// rewrites v1's checksum and every existing authority database would refuse to
// open with "migration checksum drift at version 1". Later renames belong in a
// later migration; appendOnlyTables carries the current names.
var migrationV1AppendOnlyTables = []string{
	"schema_migrations", "broker_scopes", "event_log", "observations",
	"consumed_preview_tokens", "regime_decisions", "regime_indicators",
	"rule_transitions", "canary_transitions", "capital_events",
	"risk_policy_events", "proposal_outcomes", "order_events",
	"statement_file_versions", "statement_equity_day_versions",
}

// appendOnlyTables is the append-only set after the whole migration plan has
// been applied: v1's tables with migration 2's canary→stress rename.
func appendOnlyUpdateTrigger(table string) string {
	return fmt.Sprintf(`CREATE TRIGGER %s_no_update BEFORE UPDATE ON %s BEGIN SELECT RAISE(ABORT, '%s is append-only'); END`, table, table, table)
}

func appendOnlyDeleteTrigger(table string) string {
	return fmt.Sprintf(`CREATE TRIGGER %s_no_delete BEFORE DELETE ON %s BEGIN SELECT RAISE(ABORT, '%s is append-only'); END`, table, table, table)
}

// stressRenameMigration is migration 2: the portfolio-stress sensor's
// RENAME rather than copied, so no evidence row is ever rewritten; only the
// event_log label column is rewritten, in place, for the exact event type
func stressRenameMigration() migration {
	return migration{
		version: 2,
		name:    "stress_sensor_rename",
		statements: []string{
			// Unarm the append-only pair, carry the table and every row to the
			// new name, then re-arm under the new names. Dropping first avoids
			// depending on how SQLite rewrites trigger bodies across a rename.
			`DROP TRIGGER canary_transitions_no_update`,
			`DROP TRIGGER canary_transitions_no_delete`,
			`ALTER TABLE canary_transitions RENAME TO stress_transitions`,
			appendOnlyUpdateTrigger("stress_transitions"),
			appendOnlyDeleteTrigger("stress_transitions"),
			// Relabel the persisted event type. event_log's UPDATE guard has to
			// its DELETE guard is never touched.
			`DROP TRIGGER event_log_no_update`,
			`UPDATE event_log SET event_type = 'stress_decision' WHERE event_type = 'canary_decision'`,
			appendOnlyUpdateTrigger("event_log"),
		},
		destructive: &destructiveApproval{
			reason: "Renaming the portfolio-stress sensor requires three DROP TRIGGER statements. " +
				"Two unarm the canary_transitions append-only pair so the table can be renamed and " +
				"re-armed under its new name; the rows are carried by the rename and never rewritten. " +
				"The third suspends event_log's append-only property for exactly one statement, the " +
				"UPDATE that relabels event_type from canary_decision to stress_decision. That " +
				"temporarily makes an audit log writable, which is why it is approved explicitly and " +
				"held as narrow as possible: only the event_type column changes, only on rows whose " +
				"event_type is exactly canary_decision, no payload or digest is touched, the recorded " +
				"decisions are unchanged, event_log's DELETE guard stays armed throughout, and all " +
				"three triggers are recreated before the migration transaction commits. Any failure " +
				"rolls the whole transaction back to the fully armed pre-migration schema.",
			statements: []string{
				`DROP TRIGGER canary_transitions_no_update`,
				`DROP TRIGGER canary_transitions_no_delete`,
				`DROP TRIGGER event_log_no_update`,
			},
		},
	}
}

// legacyStressMeasurementRename is migration 3: the observations the SQLite
// exact (scope_key, source, kind) triple its importer wrote, so the UPDATE is
// came from are sealed into legacy-sealed/<cutover-id>/ and the derived
func legacyStressMeasurementRename() migration {
	const relabel = `UPDATE observations
   SET scope_key = 'market/legacy/stress-measurements',
       source    = 'legacy.stress_decision_journal',
       kind      = 'stress_market_measurement.v1'
 WHERE scope_key = 'market/legacy/canary-measurements'
   AND source    = 'legacy.canary_decision_journal'
   AND kind      = 'canary_market_measurement.v1'`
	return migration{
		version: 3,
		name:    "legacy_stress_measurement_rename",
		statements: []string{
			`DROP TRIGGER observations_no_update`,
			relabel,
			appendOnlyUpdateTrigger("observations"),
		},
		destructive: &destructiveApproval{
			reason: "Relabelling the imported legacy portfolio-stress measurements requires one " +
				"DROP TRIGGER: observations is append-only, and the three label columns live on " +
				"the existing rows. The exception is held as narrow as the schema allows — a " +
				"single UPDATE, predicated on the exact scope_key/source/kind triple the cutover " +
				"importer wrote, changing only those three columns. Payloads, digests, timestamps, " +
				"decision_eligible, and every row from every other source are untouched; " +
				"observations' DELETE guard is never dropped and the UPDATE guard is recreated " +
				"before the migration transaction commits, so a failure rolls back to the fully " +
				"armed pre-migration schema. Re-importing instead of renaming is not available: " +
				"the source journals are sealed at cutover and the derived history.db is discarded, " +
				"which makes these rows the only remaining queryable copy of that evidence.",
			statements: []string{`DROP TRIGGER observations_no_update`},
		},
	}
}

// contractCacheObservationPrune is migration 4. A defect copied the current
// IBKR contract cache into the retained observation ledger on every refresh.
var contractCachePruneSelector = ObservationDiscardSelector{
	ScopeKey: "market/contracts",
	Source:   "ibkr.tws.contract_details",
	Kind:     "contract_cache.snapshot.v3",
}

func contractCacheObservationPrune() migration {
	selector := contractCachePruneSelector
	dropDeleteGuard := `DROP TRIGGER observations_no_delete`
	discard := observationDiscardDeleteStatement(selector)
	return migration{
		version: 4,
		name:    "contract_cache_observation_prune",
		statements: []string{
			dropDeleteGuard,
			discard,
			appendOnlyDeleteTrigger("observations"),
		},
		destructive: &destructiveApproval{
			reason: "A defect appended the full refetchable IBKR contract cache to the observation ledger on every refresh. " +
				"No reader or decision uses this exact observation kind; the current cache remains in the market/contracts " +
				"state document and can be refetched from IBKR. This exception drops only observations_no_delete, deletes " +
				"only rows matching the exact scope_key/source/kind triple, and recreates the identical delete guard in the " +
				"same transaction. It does not touch event_log, store_meta, current state, near-matching observations, or any " +
				"other retained evidence.",
			statements: []string{dropDeleteGuard, discard},
		},
		maintenance: &migrationMaintenance{
			ObservationDiscard:    &selector,
			CompactCandidate:      true,
			RetireSourceBackup:    true,
			PreserveAuthorityHead: true,
		},
	}
}

const alertEpisodeNonTransitionPredicate = "alert_episode_non_transition.v1"

var alertEpisodeNonTransitionSelector = EventDiscardSelector{
	ScopeKey:  "daemon",
	EventType: "alert_episode_decision",
	Predicate: alertEpisodeNonTransitionPredicate,
}

// alertEpisodeEventPrune is migration 5. The alert registry is authoritative
// snapshot on every 30-second evaluation. Only lifecycle transitions are audit
func alertEpisodeEventPrune() migration {
	selector := alertEpisodeNonTransitionSelector
	dropDeleteGuard := `DROP TRIGGER event_log_no_delete`
	discard := eventDiscardDeleteStatement(selector)
	return migration{
		version: 5,
		name:    "alert_episode_event_prune",
		statements: []string{
			dropDeleteGuard,
			discard,
			appendOnlyDeleteTrigger("event_log"),
		},
		destructive: &destructiveApproval{
			reason: "The alert registry appended its complete redacted decision snapshot on every 30-second evaluation even when no lifecycle transition occurred. " +
				"Those snapshots have no reader and duplicate current registry state and durable commissioning counters in state_documents. This exception drops only event_log_no_delete, deletes only released version-3 or version-4 daemon alert_episode_decision payloads whose every decision is an object with a reviewed non-transition action, and recreates the identical delete guard in the same transaction. " +
				"Opened, reopened, escalated, and recovered lifecycle events remain, as do unknown versions or actions, all other event types and scopes, current registry state, typed projections, and observations.",
			statements: []string{dropDeleteGuard, discard},
		},
		maintenance: &migrationMaintenance{
			EventDiscard:       &selector,
			CompactCandidate:   true,
			RetireSourceBackup: true,
		},
	}
}

const betaOperationalPrunePredicate = "beta_operational_history.v1"

var betaOperationalPruneSelector = OperationalPruneSelector{Predicate: betaOperationalPrunePredicate}

// betaOperationalHistoryPrune is migration 6. Canary's beta writers retained
// raw market refreshes and analytical histories without a bounded operational
// reader. Current state, outage fallbacks, broker/order continuity, token
// tombstones, capital and risk governance, reconciliation, statements,
// receipt-bound earnings identity observations, terminal-evidence changes,
// quarantined gamma recovery rows, operational proposal/opportunity events,
// and the newest two regime events all survive.
//
// The exact statements are frozen by validateMigrationMaintenance. This is a
// one-time semantic epoch reset, not a configurable retention engine: unknown
// observation kinds, unknown event types/actions, and near matches survive.
func betaOperationalHistoryPrune() migration {
	selector := betaOperationalPruneSelector
	statements := betaOperationalPruneStatements()
	destructive := make([]string, 0, len(statements))
	for _, statement := range statements {
		if isDestructiveStatement(statement) {
			destructive = append(destructive, statement)
		}
	}
	return migration{
		version:    6,
		name:       "beta_operational_history_prune",
		statements: statements,
		destructive: &destructiveApproval{
			reason: "The beta authority retained full market refresh payloads and analytical histories that no operational reader uses. " +
				"This reviewed reset preserves every current state document, outage cache, broker route, order event, consumed-token tombstone, order-ID floor, capital and risk-policy event, statement and reconciliation row, exact earnings identity proof, terminal-evidence change, quarantined gamma recovery row, operational proposal/opportunity event, unknown row class, and the newest two regime events required by projection recovery. " +
				"It removes only the frozen observation kinds and analytical event/action classes named by beta_operational_history.v1, deletes their typed child projections before parent events, restores every suspended append-only delete trigger in the same transaction, advances the authority head exactly once, and requires out-of-place compaction plus verified target-backup publication.",
			statements: destructive,
		},
		maintenance: &migrationMaintenance{
			OperationalPrune:   &selector,
			CompactCandidate:   true,
			RetireSourceBackup: true,
		},
	}
}

func init() {
	for _, table := range migrationV1AppendOnlyTables {
		migrations[0].statements = append(migrations[0].statements,
			appendOnlyUpdateTrigger(table),
			appendOnlyDeleteTrigger(table),
		)
	}
	migrations[0].statements = append(migrations[0].statements,
		`CREATE TRIGGER store_meta_epoch_immutable BEFORE UPDATE OF authority_epoch ON store_meta
WHEN NEW.authority_epoch <> OLD.authority_epoch BEGIN SELECT RAISE(ABORT, 'authority epoch is immutable'); END`,
		`CREATE TRIGGER store_meta_monotonic BEFORE UPDATE ON store_meta
WHEN NEW.head_generation < OLD.head_generation OR NEW.last_event_seq < OLD.last_event_seq OR NEW.signer_generation < OLD.signer_generation
BEGIN SELECT RAISE(ABORT, 'authority head cannot decrease'); END`,
		`CREATE TRIGGER store_meta_no_delete BEFORE DELETE ON store_meta BEGIN SELECT RAISE(ABORT, 'store metadata cannot be deleted'); END`,
		`CREATE TRIGGER order_id_floors_no_decrease BEFORE UPDATE OF floor ON order_id_floors
WHEN NEW.floor < OLD.floor BEGIN SELECT RAISE(ABORT, 'order id floor cannot decrease'); END`,
	)
	// v1's trigger statements are generated, so the plan is completed here
	migrations = append(migrations,
		stressRenameMigration(),
		legacyStressMeasurementRename(),
		contractCacheObservationPrune(),
		alertEpisodeEventPrune(),
		betaOperationalHistoryPrune(),
	)
}

// migrationChecksum is the ledger identity of an applied migration: version,
// its prose can be clarified later without making every existing authority
// database fail to open. Maintenance is appended only when present, preserving
// the already-shipped checksums of migrations that predate this field.
func migrationChecksum(m migration) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%s\x00", m.version, m.name)
	for _, stmt := range m.statements {
		h.Write([]byte(stmt))
		h.Write([]byte{0})
	}
	if m.maintenance != nil {
		h.Write([]byte("maintenance.v1"))
		h.Write([]byte{0})
		fmt.Fprintf(h, "compact=%t\x00retire_source_backup=%t\x00preserve_authority_head=%t\x00",
			m.maintenance.CompactCandidate,
			m.maintenance.RetireSourceBackup,
			m.maintenance.PreserveAuthorityHead)
		if m.maintenance.ObservationDiscard == nil {
			h.Write([]byte("observation_discard=nil"))
			h.Write([]byte{0})
		} else {
			h.Write([]byte("observation_discard"))
			h.Write([]byte{0})
			for _, value := range []string{
				m.maintenance.ObservationDiscard.ScopeKey,
				m.maintenance.ObservationDiscard.Source,
				m.maintenance.ObservationDiscard.Kind,
			} {
				h.Write([]byte(value))
				h.Write([]byte{0})
			}
		}
		// EventDiscard was added after migration 4 shipped. Append it only when
		// present so migration 4's ledger checksum remains byte-for-byte frozen.
		if m.maintenance.EventDiscard != nil {
			h.Write([]byte("maintenance.event-discard.v1"))
			h.Write([]byte{0})
			h.Write([]byte("event_discard"))
			h.Write([]byte{0})
			for _, value := range []string{
				m.maintenance.EventDiscard.ScopeKey,
				m.maintenance.EventDiscard.EventType,
				m.maintenance.EventDiscard.Predicate,
			} {
				h.Write([]byte(value))
				h.Write([]byte{0})
			}
		}
		// OperationalPrune was added after migrations 4 and 5 shipped. Append
		// it only when present so both earlier ledger checksums remain frozen.
		if m.maintenance.OperationalPrune != nil {
			h.Write([]byte("maintenance.operational-prune.v1"))
			h.Write([]byte{0})
			h.Write([]byte(m.maintenance.OperationalPrune.Predicate))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func currentMigrationPlan() []migration {
	return cloneMigrationPlan(migrations)
}

func cloneMigrationPlan(plan []migration) []migration {
	cloned := make([]migration, len(plan))
	for i, m := range plan {
		cloned[i] = m
		cloned[i].statements = append([]string(nil), m.statements...)
		if m.destructive != nil {
			approval := *m.destructive
			approval.statements = append([]string(nil), m.destructive.statements...)
			cloned[i].destructive = &approval
		}
		if m.maintenance != nil {
			maintenance := *m.maintenance
			if m.maintenance.ObservationDiscard != nil {
				selector := *m.maintenance.ObservationDiscard
				maintenance.ObservationDiscard = &selector
			}
			if m.maintenance.EventDiscard != nil {
				selector := *m.maintenance.EventDiscard
				maintenance.EventDiscard = &selector
			}
			if m.maintenance.OperationalPrune != nil {
				selector := *m.maintenance.OperationalPrune
				maintenance.OperationalPrune = &selector
			}
			cloned[i].maintenance = &maintenance
		}
	}
	return cloned
}

func validateMigrationPlan(plan []migration) error {
	if len(plan) == 0 {
		return errorsf("empty migration plan")
	}
	for i, m := range plan {
		if m.version != i+1 || strings.TrimSpace(m.name) == "" {
			return fmt.Errorf("invalid migration plan at version %d", i+1)
		}
		if err := validateMigrationStatements(m); err != nil {
			return err
		}
	}
	return nil
}

func validateMigrationStatements(m migration) error {
	approved, err := approvedDestructiveStatements(m)
	if err != nil {
		return err
	}
	if err := validateMigrationMaintenance(m, approved); err != nil {
		return err
	}
	for _, stmt := range m.statements {
		if !isDestructiveStatement(stmt) {
			continue
		}
		if _, ok := approved[stmt]; !ok {
			return fmt.Errorf("migration %d contains destructive statement", m.version)
		}
	}
	return nil
}

func validateMigrationMaintenance(m migration, approved map[string]struct{}) error {
	if m.maintenance == nil {
		return nil
	}
	maintenance := m.maintenance
	typed := 0
	for _, present := range []bool{
		maintenance.ObservationDiscard != nil,
		maintenance.EventDiscard != nil,
		maintenance.OperationalPrune != nil,
	} {
		if present {
			typed++
		}
	}
	if typed != 1 {
		return fmt.Errorf("migration %d maintenance must have exactly one typed discard", m.version)
	}
	if !maintenance.CompactCandidate {
		return fmt.Errorf("migration %d discard does not require candidate compaction", m.version)
	}
	if !maintenance.RetireSourceBackup {
		return fmt.Errorf("migration %d discard does not require eventual source-backup retirement", m.version)
	}

	if maintenance.ObservationDiscard != nil {
		selector := *maintenance.ObservationDiscard
		if strings.TrimSpace(selector.ScopeKey) == "" ||
			strings.TrimSpace(selector.Source) == "" ||
			strings.TrimSpace(selector.Kind) == "" {
			return fmt.Errorf("migration %d maintenance has an incomplete observation discard selector", m.version)
		}
		if !maintenance.PreserveAuthorityHead {
			return fmt.Errorf("migration %d observation discard does not explicitly preserve the authority head", m.version)
		}

		exactDelete := observationDiscardDeleteStatement(selector)
		wantStatements := []string{
			`DROP TRIGGER observations_no_delete`,
			exactDelete,
			appendOnlyDeleteTrigger("observations"),
		}
		return validateExactMaintenanceStatements(m, approved, wantStatements, "observation-discard")
	}

	if maintenance.EventDiscard != nil {
		selector := *maintenance.EventDiscard
		if selector != alertEpisodeNonTransitionSelector {
			return fmt.Errorf("migration %d maintenance has an invalid event discard selector", m.version)
		}
		if maintenance.PreserveAuthorityHead {
			return fmt.Errorf("migration %d event discard cannot preserve the authority head", m.version)
		}
		exactDelete := eventDiscardDeleteStatement(selector)
		wantStatements := []string{
			`DROP TRIGGER event_log_no_delete`,
			exactDelete,
			appendOnlyDeleteTrigger("event_log"),
		}
		return validateExactMaintenanceStatements(m, approved, wantStatements, "event-discard")
	}

	selector := *maintenance.OperationalPrune
	if selector != betaOperationalPruneSelector {
		return fmt.Errorf("migration %d maintenance has an invalid operational prune selector", m.version)
	}
	if maintenance.PreserveAuthorityHead {
		return fmt.Errorf("migration %d operational prune cannot preserve the authority head", m.version)
	}
	wantStatements := betaOperationalPruneStatements()
	if len(m.statements) != len(wantStatements) {
		return fmt.Errorf("migration %d maintenance is not the exact guarded operational-prune transaction", m.version)
	}
	for i, statement := range wantStatements {
		if m.statements[i] != statement {
			return fmt.Errorf("migration %d maintenance statement %d is not the exact guarded operational-prune statement", m.version, i+1)
		}
		if isDestructiveStatement(statement) {
			if _, ok := approved[statement]; !ok {
				return fmt.Errorf("migration %d operational-prune destructive statement lacks exact approval", m.version)
			}
		}
	}
	return nil
}

func validateExactMaintenanceStatements(m migration, approved map[string]struct{}, wantStatements []string, label string) error {
	if len(m.statements) != len(wantStatements) {
		return fmt.Errorf("migration %d maintenance is not the exact guarded %s transaction", m.version, label)
	}
	for i := range wantStatements {
		if m.statements[i] != wantStatements[i] {
			return fmt.Errorf("migration %d maintenance statement %d is not the exact guarded %s statement", m.version, i+1, label)
		}
	}
	for _, statement := range wantStatements[:2] {
		if _, ok := approved[statement]; !ok {
			return fmt.Errorf("migration %d maintenance destructive statement lacks exact approval", m.version)
		}
	}
	return nil
}

func observationDiscardDeleteStatement(selector ObservationDiscardSelector) string {
	quote := func(value string) string {
		return strings.ReplaceAll(value, "'", "''")
	}
	return fmt.Sprintf(`DELETE FROM observations
 WHERE scope_key = '%s'
   AND source    = '%s'
   AND kind      = '%s'`, quote(selector.ScopeKey), quote(selector.Source), quote(selector.Kind))
}

func eventDiscardDeleteStatement(selector EventDiscardSelector) string {
	clause := eventDiscardMatchClause(selector)
	if clause == "" {
		return ""
	}
	return "DELETE FROM event_log\n" + clause
}

func eventDiscardSummaryStatement(selector EventDiscardSelector) string {
	clause := eventDiscardMatchClause(selector)
	if clause == "" {
		return ""
	}
	return "SELECT event_seq,length(payload_json),payload_sha256\nFROM event_log\n" + clause + "\n ORDER BY event_seq"
}

func eventDiscardCountStatement(selector EventDiscardSelector) string {
	clause := eventDiscardMatchClause(selector)
	if clause == "" {
		return ""
	}
	return "SELECT count(*) FROM event_log\n" + clause
}

func eventDiscardMatchClause(selector EventDiscardSelector) string {
	quote := func(value string) string {
		return strings.ReplaceAll(value, "'", "''")
	}
	if selector != alertEpisodeNonTransitionSelector {
		return ""
	}
	return fmt.Sprintf(` WHERE scope_key = '%s'
   AND event_type = '%s'
   AND json_extract(payload_json, '$.version') IN (3,4)
   AND json_type(payload_json, '$.decisions') = 'array'
   AND NOT EXISTS (
         SELECT 1
           FROM json_each(event_log.payload_json, '$.decisions') AS decision
          WHERE CASE
                  WHEN decision.type = 'object'
                  THEN coalesce(json_extract(decision.value, '$.action') IN ('opened','reopened','escalated','recovered'), 0)
                  ELSE 0
                END
       )
   AND NOT EXISTS (
         SELECT 1
           FROM json_each(event_log.payload_json, '$.decisions') AS decision
          WHERE CASE
                  WHEN decision.type <> 'object' THEN 1
                  WHEN json_type(decision.value, '$.action') IS NOT 'text' THEN 1
                  WHEN json_extract(decision.value, '$.action') NOT IN ('opened','reopened','escalated','refreshed_active','recovered','confirmed_recovered','negative_without_episode','held_omitted','held_partial','held_stale','held_unavailable','held_untrusted_evidence') THEN 1
                  ELSE 0
                END
       )`, quote(selector.ScopeKey), quote(selector.EventType))
}

func betaOperationalObservationMatchClause() string {
	return ` WHERE
       (source = 'ibkr.tws.option_chain'
        AND kind = 'gamma_zero.compute.v1'
        AND decision_eligible = 1)
    OR kind IN (
       'contract_cache.snapshot.v3',
       'gamma_open_interest.snapshot.v1',
       'trading_halts.snapshot.v1',
       'breadth_spx.windows.v2',
       'borrow_inventory.snapshot.v1',
       'gamma_expiry_grid.snapshot.v1',
       'regime_streaks.snapshot.v1',
       'regime_official_series.snapshot.v1',
       'regime_hmds.snapshot.v1',
       'regime_measurement.v1',
       'gamma_skew_diagnostic.v1',
       'reg_sho.snapshot.v1',
       'stress_market_measurement.v1',
       'fx_rates.snapshot.v1',
       'earnings_dates.snapshot.v1',
       'borrow_fees.fetch_outcome.v2',
       'earnings_dates.provider_outcome.v2',
       'earnings_dates.provider_outcome.v3',
       'breadth_spx.history.v2',
       'breadth_spx.snapshot.v1',
       'spx_members.snapshot.v1',
       'borrow_fee_tws.fetch_outcome.v1'
    )`
}

func betaOperationalRegimeEventMatchClause() string {
	return ` WHERE scope_key = 'daemon'
   AND event_type = 'regime_decision'
   AND event_seq NOT IN (
       SELECT event_seq
         FROM event_log
        WHERE scope_key = 'daemon'
          AND event_type = 'regime_decision'
        ORDER BY event_seq DESC
        LIMIT 2
   )`
}

func betaOperationalEventMatchClause() string {
	return ` WHERE scope_key = 'daemon'
   AND (
       event_type IN ('rule_transition','stress_decision','alert_episode_decision')
       OR (
           event_type = 'regime_decision'
           AND event_seq NOT IN (
               SELECT event_seq
                 FROM event_log
                WHERE scope_key = 'daemon'
                  AND event_type = 'regime_decision'
                ORDER BY event_seq DESC
                LIMIT 2
           )
       )
       OR (
           event_type = 'trade_proposal_event'
           AND json_extract(payload_json, '$.version') = 1
           AND json_type(payload_json, '$.type') = 'text'
           AND json_extract(payload_json, '$.type') IN (
               'generated','theta-suppressed','shown','blocked','previewed',
               'policy-drift','policy-error'
           )
       )
       OR (
           event_type = 'opportunity_event'
           AND json_extract(payload_json, '$.version') = 1
           AND json_type(payload_json, '$.type') = 'text'
           AND json_extract(payload_json, '$.type') IN (
               'shown','submit-blocked','submit-error','policy-drift','policy-error'
           )
       )
   )`
}

func betaOperationalPruneStatements() []string {
	regimeEvents := betaOperationalRegimeEventMatchClause()
	regimeProjectionEvents := ` WHERE event_seq IN (
       SELECT event_seq FROM event_log` + regimeEvents + `
   )`
	ruleEvents := ` WHERE event_seq IN (
       SELECT event_seq FROM event_log
        WHERE scope_key = 'daemon' AND event_type = 'rule_transition'
   )`
	stressEvents := ` WHERE event_seq IN (
       SELECT event_seq FROM event_log
        WHERE scope_key = 'daemon' AND event_type = 'stress_decision'
   )`
	return []string{
		`DROP TRIGGER observations_no_delete`,
		`DROP TRIGGER regime_indicators_no_delete`,
		`DROP TRIGGER regime_decisions_no_delete`,
		`DROP TRIGGER rule_transitions_no_delete`,
		`DROP TRIGGER stress_transitions_no_delete`,
		`DROP TRIGGER event_log_no_delete`,
		"DELETE FROM observations\n" + betaOperationalObservationMatchClause(),
		`DELETE FROM regime_indicators
 WHERE decision_event_seq IN (
       SELECT event_seq FROM event_log` + regimeEvents + `
   )`,
		"DELETE FROM regime_decisions\n" + regimeProjectionEvents,
		"DELETE FROM rule_transitions\n" + ruleEvents,
		"DELETE FROM stress_transitions\n" + stressEvents,
		"DELETE FROM event_log\n" + betaOperationalEventMatchClause(),
		appendOnlyDeleteTrigger("event_log"),
		appendOnlyDeleteTrigger("stress_transitions"),
		appendOnlyDeleteTrigger("rule_transitions"),
		appendOnlyDeleteTrigger("regime_decisions"),
		appendOnlyDeleteTrigger("regime_indicators"),
		appendOnlyDeleteTrigger("observations"),
	}
}

func isDestructiveStatement(stmt string) bool {
	upper := strings.ToUpper(strings.TrimSpace(stmt))
	return strings.HasPrefix(upper, "DROP ") || strings.HasPrefix(upper, "DELETE ") ||
		strings.HasPrefix(upper, "REPLACE ") || strings.HasPrefix(upper, "VACUUM") ||
		strings.Contains(upper, " DROP COLUMN ")
}

// approvedDestructiveStatements returns the exact statements this migration's
// carry a reason, must name at least one statement, and may only name
// statements that this migration actually runs and that are actually
func approvedDestructiveStatements(m migration) (map[string]struct{}, error) {
	if m.destructive == nil {
		return nil, nil
	}
	if strings.TrimSpace(m.destructive.reason) == "" {
		return nil, fmt.Errorf("migration %d approves destructive statements without a reason", m.version)
	}
	if len(m.destructive.statements) == 0 {
		return nil, fmt.Errorf("migration %d approves no destructive statements", m.version)
	}
	present := make(map[string]struct{}, len(m.statements))
	for _, stmt := range m.statements {
		present[stmt] = struct{}{}
	}
	approved := make(map[string]struct{}, len(m.destructive.statements))
	for _, stmt := range m.destructive.statements {
		if !isDestructiveStatement(stmt) {
			return nil, fmt.Errorf("migration %d approves a statement that is not destructive: %q", m.version, stmt)
		}
		if _, ok := present[stmt]; !ok {
			return nil, fmt.Errorf("migration %d approves a statement it does not run: %q", m.version, stmt)
		}
		approved[stmt] = struct{}{}
	}
	return approved, nil
}

// validateSchemaObjects compares every application-owned table, index, and
// trigger against a database built from the canonical migration plan. SQLite
func validateSchemaObjectsWithPlan(ctx context.Context, db *sql.DB, expectedVersion int, plan []migration) error {
	if err := validateMigrationPlan(plan); err != nil {
		return err
	}
	if expectedVersion < 1 || expectedVersion > len(plan) {
		return errorsf("unsupported schema version")
	}
	expected, err := canonicalSchemaManifestWithPlan(ctx, expectedVersion, plan)
	if err != nil {
		return fmt.Errorf("build canonical schema manifest: %w", err)
	}
	actual, err := readSchemaManifest(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema object manifest: %w", err)
	}
	if len(expected) == len(actual) {
		matches := true
		for i := range expected {
			if expected[i] != actual[i] {
				matches = false
				break
			}
		}
		if matches {
			return nil
		}
	}
	wantFingerprint := schemaManifestFingerprint(expected)
	gotFingerprint := schemaManifestFingerprint(actual)

	want := make(map[string]schemaObject, len(expected))
	for _, object := range expected {
		want[object.typeName+"\x00"+object.name] = object
	}
	got := make(map[string]schemaObject, len(actual))
	for _, object := range actual {
		got[object.typeName+"\x00"+object.name] = object
	}
	for key, expectedObject := range want {
		actualObject, ok := got[key]
		switch {
		case !ok:
			return fmt.Errorf("schema object manifest mismatch: missing %s %q (expected %s, got %s)", expectedObject.typeName, expectedObject.name, wantFingerprint, gotFingerprint)
		case actualObject != expectedObject:
			return fmt.Errorf("schema object manifest mismatch: changed %s %q (expected %s, got %s)", expectedObject.typeName, expectedObject.name, wantFingerprint, gotFingerprint)
		}
	}
	for key, actualObject := range got {
		if _, ok := want[key]; !ok {
			return fmt.Errorf("schema object manifest mismatch: unexpected %s %q (expected %s, got %s)", actualObject.typeName, actualObject.name, wantFingerprint, gotFingerprint)
		}
	}
	return fmt.Errorf("schema object manifest mismatch: expected %s, got %s", wantFingerprint, gotFingerprint)
}

// canonicalSchemaManifests memoizes the manifest each validated plan prefix
// produces. The manifest is a pure function of the statements that prefix
// runs, and the ledger checksums already identify those statements exactly,
// so a hit is as strong a derivation as the rebuild it replaces. Building it
// migrates a throwaway in-memory database, which every inspection, open,
// backup verification, and upgrade step would otherwise pay for again.
var canonicalSchemaManifests sync.Map

func canonicalSchemaManifestWithPlan(ctx context.Context, version int, plan []migration) ([]schemaObject, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return nil, err
	}
	if version < 1 || version > len(plan) {
		return nil, errorsf("unsupported schema version")
	}
	key := canonicalSchemaManifestKey(plan[:version])
	if cached, ok := canonicalSchemaManifests.Load(key); ok {
		return slices.Clone(cached.([]schemaObject)), nil
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	if _, err := migrate(ctx, db, cloneMigrationPlan(plan[:version]), time.Unix(0, 0).UTC()); err != nil {
		return nil, err
	}
	objects, err := readSchemaManifest(ctx, db)
	if err != nil {
		return nil, err
	}
	canonicalSchemaManifests.Store(key, slices.Clone(objects))
	return objects, nil
}

func canonicalSchemaManifestKey(prefix []migration) string {
	var key strings.Builder
	for _, m := range prefix {
		key.WriteString(migrationChecksum(m))
		key.WriteByte('\x00')
	}
	return key.String()
}

func readSchemaManifest(ctx context.Context, db *sql.DB) ([]schemaObject, error) {
	rows, err := db.QueryContext(ctx, `SELECT type,name,tbl_name,sql
FROM sqlite_schema
WHERE type IN ('table','index','trigger')
ORDER BY type,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []schemaObject
	for rows.Next() {
		var object schemaObject
		var ddl sql.NullString
		if err := rows.Scan(&object.typeName, &object.name, &object.table, &ddl); err != nil {
			return nil, err
		}
		if strings.HasPrefix(object.name, "sqlite_") {
			continue
		}
		if !ddl.Valid {
			return nil, fmt.Errorf("application-owned %s %q has no defining SQL", object.typeName, object.name)
		}
		object.sql, err = normalizeSchemaSQL(ddl.String)
		if err != nil {
			return nil, fmt.Errorf("normalize %s %q: %w", object.typeName, object.name, err)
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return objects, nil
}

func schemaManifestFingerprint(objects []schemaObject) string {
	h := sha256.New()
	for _, object := range objects {
		for _, part := range []string{object.typeName, object.name, object.table, object.sql} {
			h.Write([]byte(part))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeSchemaSQL removes formatting and keyword-case differences while
// retaining token, quoted-identifier, and string-literal boundaries. That
func normalizeSchemaSQL(input string) (string, error) {
	var tokens []string
	for i := 0; i < len(input); {
		if strings.ContainsRune(" \t\r\n\f\v", rune(input[i])) {
			i++
			continue
		}
		switch input[i] {
		case '\'', '"', '`':
			quote := input[i]
			start := i
			i++
			closed := false
			for i < len(input) {
				if input[i] != quote {
					i++
					continue
				}
				if i+1 < len(input) && input[i+1] == quote {
					i += 2
					continue
				}
				i++
				closed = true
				break
			}
			if !closed {
				return "", errorsf("unterminated quoted token")
			}
			tokens = append(tokens, input[start:i])
		case '[':
			start := i
			i++
			for i < len(input) && input[i] != ']' {
				i++
			}
			if i == len(input) {
				return "", errorsf("unterminated bracketed identifier")
			}
			i++
			tokens = append(tokens, input[start:i])
		default:
			if strings.ContainsRune("(),;.=<>+-*/%|&~", rune(input[i])) {
				tokens = append(tokens, input[i:i+1])
				i++
				continue
			}
			start := i
			for i < len(input) &&
				!strings.ContainsRune(" \t\r\n\f\v'\"`[](),;.=<>+-*/%|&~", rune(input[i])) {
				i++
			}
			tokens = append(tokens, strings.ToLower(input[start:i]))
		}
	}
	return strings.Join(tokens, "\x1f"), nil
}

// migrate brings db up to the plan's current version and reports how many
// migrations it had to apply. Callers use that count to decide whether
// post-migration work is warranted: on an ordinary open the database is already
func migrate(ctx context.Context, db *sql.DB, plan []migration, now time.Time) (int, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return 0, err
	}
	var userVersion, appID int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&appID); err != nil {
		return 0, fmt.Errorf("read application identity: %w", err)
	}
	current := plan[len(plan)-1].version
	if userVersion > current {
		return 0, fmt.Errorf("future schema version %d exceeds supported %d", userVersion, current)
	}

	var tableCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tableCount); err != nil {
		return 0, fmt.Errorf("inspect schema: %w", err)
	}
	var migrationTable int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&migrationTable); err != nil {
		return 0, fmt.Errorf("inspect migration ledger: %w", err)
	}
	if migrationTable == 0 {
		if userVersion != 0 || tableCount != 0 || appID != 0 {
			return 0, fmt.Errorf("unmanaged or incomplete authority database")
		}
	} else {
		if appID != applicationID {
			return 0, fmt.Errorf("application identity mismatch")
		}
		rows, err := db.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
		if err != nil {
			return 0, fmt.Errorf("read migration ledger: %w", err)
		}
		applied := 0
		for rows.Next() {
			var version int
			var name, checksum string
			if err := rows.Scan(&version, &name, &checksum); err != nil {
				rows.Close()
				return 0, fmt.Errorf("scan migration ledger: %w", err)
			}
			if version != applied+1 || version > current {
				rows.Close()
				return 0, fmt.Errorf("future or non-contiguous migration version %d", version)
			}
			want := plan[version-1]
			if name != want.name || checksum != migrationChecksum(want) {
				rows.Close()
				return 0, fmt.Errorf("migration checksum drift at version %d", version)
			}
			applied = version
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read migration ledger: %w", err)
		}
		rows.Close()
		if applied != userVersion {
			return 0, fmt.Errorf("schema version %d does not match migration ledger %d", userVersion, applied)
		}
	}

	for version := userVersion + 1; version <= current; version++ {
		m := plan[version-1]
		if m.version != version {
			return 0, fmt.Errorf("invalid migration plan at version %d", version)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin migration %d: %w", version, err)
		}
		failed := func() error {
			defer tx.Rollback()
			for _, stmt := range m.statements {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("apply migration %d: %w", version, err)
				}
			}
			stamp := formatTime(now)
			if version == 1 {
				epoch, err := authorityEpoch()
				if err != nil {
					return fmt.Errorf("create authority epoch: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO store_meta
(singleton, authority_epoch, head_generation, last_event_seq, signer_generation, created_at, updated_at)
VALUES (1, ?, 0, 0, 1, ?, ?)`, epoch, stamp, stamp); err != nil {
					return fmt.Errorf("initialize authority metadata: %w", err)
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, migrationChecksum(m), stamp); err != nil {
				return fmt.Errorf("record migration %d: %w", version, err)
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA application_id = %d`, applicationID)); err != nil {
				return fmt.Errorf("stamp application identity: %w", err)
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
				return fmt.Errorf("stamp schema version: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration %d: %w", version, err)
			}
			return nil
		}()
		if failed != nil {
			return 0, failed
		}
	}
	return max(current-userVersion, 0), nil
}

func authorityEpoch() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func errorsf(message string) error { return fmt.Errorf("corestore: %s", message) }
