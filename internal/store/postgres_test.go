package store

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Postgres tests run only when FORGELINE_TEST_DATABASE_URL points at a database they may
// create schemas in. Each test gets its own schema, dropped afterwards.
func TestPostgres(t *testing.T) {
	base := os.Getenv("FORGELINE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set FORGELINE_TEST_DATABASE_URL to run the Postgres store tests")
	}
	runSuite(t, func(t *testing.T) Store {
		ctx := context.Background()
		schema := "t_" + newID()
		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
			admin.Close(ctx)
		})

		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		s, err := OpenPostgres(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	})
}

func TestPostgresMigrationsAreIdempotent(t *testing.T) {
	base := os.Getenv("FORGELINE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set FORGELINE_TEST_DATABASE_URL to run the Postgres store tests")
	}
	ctx := context.Background()
	schema := "t_" + newID()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	admin.Exec(ctx, "CREATE SCHEMA "+schema)
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")

	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	for i := 0; i < 2; i++ { // the second open must find everything already applied
		s, err := OpenPostgres(ctx, u.String())
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		s.Close()
	}
	var n int
	admin.QueryRow(ctx, "SELECT count(*) FROM "+schema+".schema_migrations").Scan(&n)
	files, _ := migrationFiles.ReadDir("migrations")
	if n != len(files) {
		t.Fatalf("schema_migrations has %d rows, want %d (one per migration file)", n, len(files))
	}
}
