package store

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool returns a pool on a fresh, migrated database that is dropped when
// the test ends. It needs DATABASE_URL (scripts/with-testdb.sh sets one up);
// without it the test is skipped, unless QBUZZER_REQUIRE_DB is set, as in CI,
// where a missing database must fail rather than silently skip.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		if os.Getenv("QBUZZER_REQUIRE_DB") != "" {
			t.Fatal("QBUZZER_REQUIRE_DB is set but DATABASE_URL is empty")
		}
		t.Skip("DATABASE_URL not set; run via scripts/with-testdb.sh")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	name := "qbz_test_" + strings.ToLower(rand.Text()[:10])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
