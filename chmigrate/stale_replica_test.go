package chmigrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// The verbatim shape ClickHouse 25.x produces for a stale Keeper registration
// (observed live: doujins stack, hentai0-migrate, 2026-06-10).
const liveStaleReplicaMsg = "code: 253, message: There was an error on [localhost:9000]: Code: 253. " +
	"DB::Exception: Replica /clickhouse/tables/analytics/session_links/replicas/replica1 already exists. " +
	"(REPLICA_ALREADY_EXISTS) (version 25.8.16.34 (official build))"

func TestStaleReplicaInfo(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantZkPath  string
		wantReplica string
		wantOK      bool
	}{
		{
			name:        "live error string",
			err:         errors.New(liveStaleReplicaMsg),
			wantZkPath:  "/clickhouse/tables/analytics/session_links",
			wantReplica: "replica1",
			wantOK:      true,
		},
		{
			name: "clickhouse exception with code 253",
			err: &clickhouse.Exception{
				Code:    253,
				Message: "Replica /clickhouse/tables/hub/signal_state/replicas/replica1 already exists. (REPLICA_ALREADY_EXISTS)",
			},
			wantZkPath:  "/clickhouse/tables/hub/signal_state",
			wantReplica: "replica1",
			wantOK:      true,
		},
		{
			name: "wrapped exception",
			err: fmt.Errorf("migration 001_schema.up.sql: %w", &clickhouse.Exception{
				Code:    253,
				Message: "Replica /x/y/replicas/r2 already exists. (REPLICA_ALREADY_EXISTS)",
			}),
			wantZkPath:  "/x/y",
			wantReplica: "r2",
			wantOK:      true,
		},
		{
			name:   "different exception code is not matched",
			err:    &clickhouse.Exception{Code: 57, Message: "Table analytics.session_links already exists"},
			wantOK: false,
		},
		{
			name:   "unrelated error",
			err:    errors.New("dial tcp: connection refused"),
			wantOK: false,
		},
		{
			name:   "nil error",
			err:    nil,
			wantOK: false,
		},
		{
			name:   "253 without parseable path",
			err:    &clickhouse.Exception{Code: 253, Message: "something unexpected"},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zkPath, replica, ok := staleReplicaInfo(tt.err)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (err=%v)", ok, tt.wantOK, tt.err)
			}
			if !ok {
				return
			}
			if zkPath != tt.wantZkPath || replica != tt.wantReplica {
				t.Errorf("got (%q, %q), want (%q, %q)", zkPath, replica, tt.wantZkPath, tt.wantReplica)
			}
		})
	}
}

// fakeRows implements driver.Rows over a fixed set of (database, table) rows.
type fakeRows struct {
	driver.Rows
	rows [][2]string
	idx  int
}

func (r *fakeRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	if len(dest) != 2 {
		return fmt.Errorf("expected 2 scan targets, got %d", len(dest))
	}
	*(dest[0].(*string)) = row[0]
	*(dest[1].(*string)) = row[1]
	return nil
}

func (r *fakeRows) Close() error { return nil }
func (r *fakeRows) Err() error   { return nil }

// fakeConn implements driver.Conn. The first CREATE fails with the live 253
// error; everything else succeeds. system.replicas queries return liveRows.
type fakeConn struct {
	driver.Conn
	execs       []string
	queries     []string
	failCreates int // how many CREATE execs should fail with 253 before succeeding
	liveRows    [][2]string
}

func (f *fakeConn) Exec(_ context.Context, query string, _ ...any) error {
	f.execs = append(f.execs, query)
	if strings.HasPrefix(query, "CREATE TABLE") && f.failCreates > 0 {
		f.failCreates--
		return errors.New(liveStaleReplicaMsg)
	}
	return nil
}

func (f *fakeConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	f.queries = append(f.queries, query)
	return &fakeRows{rows: f.liveRows}, nil
}

func TestExecStatementsRecoversStaleReplica(t *testing.T) {
	conn := &fakeConn{failCreates: 1}
	c := &ClickHouse{conn: conn}

	stmt := "CREATE TABLE IF NOT EXISTS session_links (id UInt64) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{database}/{table}', '{replica}') ORDER BY id"
	if err := c.execStatements(context.Background(), "001_schema.up.sql", stmt); err != nil {
		t.Fatalf("execStatements returned error, want recovery: %v", err)
	}

	// Expected exec sequence: CREATE (fails 253) -> SYSTEM DROP REPLICA -> CREATE (succeeds)
	if len(conn.execs) != 3 {
		t.Fatalf("got %d execs, want 3: %q", len(conn.execs), conn.execs)
	}
	wantDrop := "SYSTEM DROP REPLICA 'replica1' FROM ZKPATH '/clickhouse/tables/analytics/session_links'"
	if conn.execs[1] != wantDrop {
		t.Errorf("drop statement = %q, want %q", conn.execs[1], wantDrop)
	}
	if conn.execs[0] != stmt || conn.execs[2] != stmt {
		t.Errorf("expected the CREATE to be retried verbatim; execs = %q", conn.execs)
	}
	// The live-table guard must have been consulted.
	if len(conn.queries) != 1 || !strings.Contains(conn.queries[0], "system.replicas") {
		t.Errorf("expected one system.replicas guard query, got %q", conn.queries)
	}
}

func TestExecStatementsBacksOffWhenReplicaHasLiveOwner(t *testing.T) {
	// A live owner means another writer just created the (shared) table — the
	// statement must be retried after backoff (IF NOT EXISTS then no-ops), and
	// SYSTEM DROP REPLICA must never be issued.
	conn := &fakeConn{
		failCreates: 1,
		liveRows:    [][2]string{{"analytics", "session_links"}},
	}
	c := &ClickHouse{conn: conn}

	stmt := "CREATE TABLE IF NOT EXISTS session_links (id UInt64) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{database}/{table}', '{replica}') ORDER BY id"
	if err := c.execStatements(context.Background(), "001_schema.up.sql", stmt); err != nil {
		t.Fatalf("expected backoff+retry to succeed once the winner's table is visible, got: %v", err)
	}
	// CREATE (253) -> backoff -> CREATE (succeeds). No drop.
	if len(conn.execs) != 2 {
		t.Errorf("got %d execs, want 2: %q", len(conn.execs), conn.execs)
	}
	for _, e := range conn.execs {
		if strings.HasPrefix(e, "SYSTEM DROP REPLICA") {
			t.Errorf("SYSTEM DROP REPLICA was issued despite a live owner: %q", conn.execs)
		}
	}
}

func TestExecStatementsStaleReplicaRecoveryIsOneShot(t *testing.T) {
	// If the CREATE keeps failing with 253 even after the drop, we must not
	// loop — the second 253 surfaces as a permanent error.
	conn := &fakeConn{failCreates: 2}
	c := &ClickHouse{conn: conn}

	stmt := "CREATE TABLE IF NOT EXISTS session_links (id UInt64) ENGINE = ReplicatedMergeTree('/x/{table}', '{replica}') ORDER BY id"
	err := c.execStatements(context.Background(), "001_schema.up.sql", stmt)
	if err == nil {
		t.Fatal("expected the second 253 to surface as an error, got nil")
	}
	if !strings.Contains(err.Error(), "REPLICA_ALREADY_EXISTS") {
		t.Errorf("expected the original 253 error to surface, got: %v", err)
	}
	// CREATE, DROP REPLICA, CREATE (fails again) — and no further attempts.
	if len(conn.execs) != 3 {
		t.Errorf("got %d execs, want 3 (no retry loop): %q", len(conn.execs), conn.execs)
	}
}
