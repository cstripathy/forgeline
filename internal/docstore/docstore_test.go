package docstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

type thing struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func runSuite(t *testing.T, open func(t *testing.T) Store) {
	ctx := context.Background()

	t.Run("create get put delete", func(t *testing.T) {
		s := open(t)
		if err := s.Create(ctx, "k", "a", thing{"alpha", 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.Create(ctx, "k", "a", thing{"again", 2}); !errors.Is(err, ErrExists) {
			t.Fatalf("second Create = %v, want ErrExists", err)
		}
		var got thing
		if err := s.Get(ctx, "k", "a", &got); err != nil || got != (thing{"alpha", 1}) {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		if err := s.Put(ctx, "k", "a", thing{"alpha", 9}); err != nil {
			t.Fatal(err)
		}
		s.Get(ctx, "k", "a", &got)
		if got.N != 9 {
			t.Fatalf("after Put, N = %d, want 9", got.N)
		}
		if err := s.Get(ctx, "k", "missing", &got); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get missing = %v", err)
		}
		if err := s.Delete(ctx, "k", "a"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "k", "a"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second Delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("kinds are separate", func(t *testing.T) {
		s := open(t)
		s.Put(ctx, "users", "x", thing{"u", 1})
		s.Put(ctx, "pipes", "x", thing{"p", 2})
		var u, p thing
		s.Get(ctx, "users", "x", &u)
		s.Get(ctx, "pipes", "x", &p)
		if u.Name != "u" || p.Name != "p" {
			t.Fatalf("users/x = %+v, pipes/x = %+v", u, p)
		}
		if n, _ := s.Count(ctx, "users"); n != 1 {
			t.Fatalf("Count = %d, want 1", n)
		}
	})

	t.Run("list is ordered, limited and can be reversed", func(t *testing.T) {
		s := open(t)
		for _, id := range []string{"b", "c", "a", "d"} {
			s.Put(ctx, "k", id, thing{Name: id})
		}
		ids := func(o ListOptions) []string {
			docs, err := s.List(ctx, "k", o)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, d := range docs {
				out = append(out, d.ID)
			}
			return out
		}
		if got := ids(ListOptions{}); !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
			t.Errorf("asc = %v", got)
		}
		if got := ids(ListOptions{Desc: true, Limit: 2}); !reflect.DeepEqual(got, []string{"d", "c"}) {
			t.Errorf("desc limit 2 = %v", got)
		}
		if got := ids(ListOptions{Limit: 1}); !reflect.DeepEqual(got, []string{"a"}) {
			t.Errorf("limit 1 = %v", got)
		}
		docs, _ := s.List(ctx, "k", ListOptions{})
		things, err := Decode[thing](docs)
		if err != nil || len(things) != 4 || things[0].Name != "a" {
			t.Errorf("Decode = %+v, %v", things, err)
		}
	})

	t.Run("concurrent creates of one id yield exactly one winner", func(t *testing.T) {
		s := open(t)
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if s.Create(ctx, "k", "same", thing{N: i}) == nil {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("%d creates succeeded, want 1", wins)
		}
	})
}

func TestMemory(t *testing.T) {
	runSuite(t, func(*testing.T) Store { return NewMemory() })
}

func TestFileSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.json")
	a, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Put(context.Background(), "users", "u1", thing{"ada", 1})
	a.Put(context.Background(), "users", "u2", thing{"bob", 2})
	a.Delete(context.Background(), "users", "u2")

	b, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got thing
	if err := b.Get(context.Background(), "users", "u1", &got); err != nil || got.Name != "ada" {
		t.Fatalf("after reopen: %+v, %v", got, err)
	}
	if err := b.Get(context.Background(), "users", "u2", &got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted doc came back: %v", err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestFileRollsBackWhenSaveFails(t *testing.T) {
	dir := t.TempDir()
	m, _ := OpenFile(filepath.Join(dir, "console.json"))
	ctx := context.Background()
	m.Put(ctx, "k", "a", thing{"keep", 1})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	if err := m.Put(ctx, "k", "b", thing{"lost", 2}); err == nil {
		t.Fatal("want an error when the file cannot be saved")
	}
	var got thing
	if err := m.Get(ctx, "k", "b", &got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("memory kept a write the file does not have: %+v %v", got, err)
	}
	if err := m.Put(ctx, "k", "a", thing{"changed", 3}); err == nil {
		t.Fatal("want an error")
	}
	m.Get(ctx, "k", "a", &got)
	if got.Name != "keep" {
		t.Fatalf("failed overwrite was not rolled back: %+v", got)
	}
}

func TestFileRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.json")
	os.WriteFile(path, []byte("not json"), 0o600)
	if _, err := OpenFile(path); err == nil {
		t.Fatal("want an error for a corrupt store file")
	}
}

func TestPostgres(t *testing.T) {
	base := os.Getenv("FORGELINE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set FORGELINE_TEST_DATABASE_URL to run the Postgres store tests")
	}
	runSuite(t, func(t *testing.T) Store {
		ctx := context.Background()
		schema := "d_" + randomSuffix()
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
		u, _ := url.Parse(base)
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		p, err := OpenPostgres(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	})
}

func randomSuffix() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
