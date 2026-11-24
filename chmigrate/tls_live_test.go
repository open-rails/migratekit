package chmigrate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/migratekit"
)

// The server listens only on its secure native port, with a certificate for
// localhost and 127.0.0.1 signed by the CA in MIGRATEKIT_TEST_CLICKHOUSE_CA_FILE.
func TestLiveTLS(t *testing.T) {
	addr, dsn := os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_TLS_ADDR"), os.Getenv("MIGRATEKIT_TEST_DATABASE_URL")
	caFile := os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_CA_FILE")
	if addr == "" || dsn == "" || caFile == "" {
		t.Skip("live TLS ClickHouse/Postgres endpoints not configured")
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatalf("%s holds no PEM certificate", caFile)
	}
	trusted := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	user, pass := os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_USER"), os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_PASS")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	pg, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	admin, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, TLS: trusted, Auth: clickhouse.Auth{Username: user, Password: pass}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	db := fmt.Sprintf("mk_tls_%d", time.Now().UnixNano())
	if err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Exec(context.Background(), "DROP DATABASE "+db+" SYNC") })
	var apps []string
	t.Cleanup(func() {
		_, _ = pg.ExecContext(context.Background(), "DELETE FROM public.migrations WHERE app = ANY($1)", apps)
	})
	migrator := func(app string, cfg *tls.Config) *ClickHouse {
		apps = append(apps, app)
		m := New(&Config{App: app, Database: db, ClientAddr: addr, Username: user, Password: pass, TLS: cfg, PostgresDB: pg})
		t.Cleanup(func() { _ = m.Close() })
		return m
	}
	migrations := []migratekit.Migration{{Name: "0_tls.up.sql", Content: "CREATE TABLE proof (id UInt64) ENGINE=Memory; INSERT INTO proof VALUES (1)"}}

	refused := map[string]struct {
		cfg  *tls.Config
		want string
	}{
		"plain":            {nil, ""},
		"unknown CA":       {&tls.Config{MinVersion: tls.VersionTLS12}, "x509"},
		"wrong ServerName": {&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "elsewhere.invalid"}, "x509"},
	}
	for name, c := range refused {
		err := migrator(db+"_"+strings.ReplaceAll(name, " ", "_"), c.cfg).ApplyMigrations(ctx, migrations)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: want a %q dial failure, got %v", name, c.want, err)
		}
	}
	var exists uint8
	if err := admin.QueryRow(ctx, "EXISTS TABLE "+db+".proof").Scan(&exists); err != nil || exists != 0 {
		t.Fatalf("a refused dial ran DDL: exists=%d err=%v", exists, err)
	}

	m := migrator(db, trusted)
	for range 2 {
		if err := m.ApplyMigrations(ctx, migrations); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.ValidateAllApplied(ctx, migrations); err != nil {
		t.Fatal(err)
	}
	var n uint64
	if err := admin.QueryRow(ctx, "SELECT count() FROM "+db+".proof").Scan(&n); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}
