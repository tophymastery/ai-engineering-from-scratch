package idempotency

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

// backend is one store implementation the concurrency suite runs against. The
// SAME criteria must hold on the real DB (Postgres), the pure-Go DB (SQLite),
// and the in-memory simulation (MemStore) — proving the semantics are the
// store's, not one engine's.
type backend struct {
	name string
	kind string // "sql" | "mem"
	// fresh returns a new empty store + an effect sink counting committed effects.
	fresh func(t *testing.T) (Store, *effectSink)
}

// effectSink counts business effects. For SQL backends the effect is a durable
// INSERT into an `effects` table (proving the write commits atomically with the
// key); the atomic counter is a cross-check that also covers MemStore.
type effectSink struct {
	counter int64
	db      *sql.DB
	dialect Dialect
	sqlMode bool
}

func (e *effectSink) business(ctx context.Context, tx Execer) (int, []byte, error) {
	atomic.AddInt64(&e.counter, 1)
	if e.sqlMode {
		q := "INSERT INTO effects (effect_key) VALUES (" + e.dialect.Placeholder(1) + ")"
		if _, err := tx.Exec(ctx, q, "k"); err != nil {
			return 0, nil, err
		}
	}
	return 201, []byte(`{"created":true}`), nil
}

func (e *effectSink) count(t *testing.T) int {
	c := int(atomic.LoadInt64(&e.counter))
	if e.sqlMode {
		var n int
		if err := e.db.QueryRow("SELECT COUNT(*) FROM effects").Scan(&n); err != nil {
			t.Fatalf("count effects: %v", err)
		}
		if n != c {
			t.Fatalf("effect counter (%d) disagrees with durable effects table (%d)", c, n)
		}
	}
	return c
}

// allBackends returns every available backend. Postgres is included only if an
// ephemeral server can be started (skipped, not failed, otherwise).
func allBackends(t *testing.T) []backend {
	bs := []backend{
		{
			name: "mem", kind: "mem",
			fresh: func(t *testing.T) (Store, *effectSink) {
				return NewMemStore(), &effectSink{}
			},
		},
		{
			name: "sqlite", kind: "sql",
			fresh: func(t *testing.T) (Store, *effectSink) {
				db := openSQLite(t)
				d := SQLiteDialect{}
				migrateEffects(t, db, d)
				return NewSQLStore(db, d), &effectSink{db: db, dialect: d, sqlMode: true}
			},
		},
	}
	if pg := tryStartPostgres(t); pg != nil {
		bs = append(bs, backend{
			name: "postgres", kind: "sql",
			fresh: func(t *testing.T) (Store, *effectSink) {
				db := pg.freshDB(t)
				d := PostgresDialect{}
				migrateEffects(t, db, d)
				return NewSQLStore(db, d), &effectSink{db: db, dialect: d, sqlMode: true}
			},
		})
	}
	return bs
}

func openSQLite(t *testing.T) *sql.DB {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "idem.db") +
		"?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(12)
	if err := Migrate(context.Background(), db, SQLiteDialect{}); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func migrateEffects(t *testing.T, db *sql.DB, d Dialect) {
	typ := "TEXT"
	_, err := db.Exec("CREATE TABLE IF NOT EXISTS effects (id SERIAL PRIMARY KEY, effect_key " + typ + ")")
	if err != nil {
		// SQLite has no SERIAL; retry with INTEGER PRIMARY KEY AUTOINCREMENT.
		if d.Name() == "sqlite" {
			if _, err2 := db.Exec("CREATE TABLE IF NOT EXISTS effects (id INTEGER PRIMARY KEY AUTOINCREMENT, effect_key TEXT)"); err2 != nil {
				t.Fatalf("create effects (sqlite): %v", err2)
			}
			return
		}
		t.Fatalf("create effects: %v", err)
	}
}

// ---- ephemeral Postgres ----

type pgServer struct {
	bin  string
	dir  string
	sock string
	port int
	base *sql.DB
	n    int32
}

// tryStartPostgres brings up an ephemeral PostgreSQL over a unix socket, run as
// the `postgres` OS user (postgres refuses to run as root). Returns nil (and
// logs a skip reason) if the toolchain/user is unavailable — the suite then
// runs on SQLite + MemStore, per the documented DB-adaptation fallback.
func tryStartPostgres(t *testing.T) *pgServer {
	if os.Getenv("IDEMPOTENCY_SKIP_PG") != "" {
		t.Log("postgres backend skipped (IDEMPOTENCY_SKIP_PG set)")
		return nil
	}
	bin := "/usr/lib/postgresql/16/bin"
	if _, err := os.Stat(filepath.Join(bin, "initdb")); err != nil {
		if p, err := exec.LookPath("initdb"); err == nil {
			bin = filepath.Dir(p)
		} else {
			t.Log("postgres backend skipped: no initdb binary")
			return nil
		}
	}
	dir, err := os.MkdirTemp("", "idem-pg-")
	if err != nil {
		t.Log("postgres backend skipped: mktemp:", err)
		return nil
	}
	data := filepath.Join(dir, "data")
	sock := filepath.Join(dir, "sock")
	_ = os.MkdirAll(data, 0o777)
	_ = os.MkdirAll(sock, 0o777)
	runAs := pgRunAsUser()
	if runAs != "" {
		if out, err := exec.Command("chown", "-R", runAs+":"+runAs, dir).CombinedOutput(); err != nil {
			t.Logf("postgres backend skipped: chown: %v: %s", err, out)
			_ = os.RemoveAll(dir)
			return nil
		}
	}
	port := 5400 + (os.Getpid() % 150)
	if out, err := pgCmd(runAs, filepath.Join(bin, "initdb"),
		"-D", data, "-U", "postgres", "--auth=trust", "--no-sync", "-E", "UTF8").CombinedOutput(); err != nil {
		t.Logf("postgres backend skipped: initdb: %v: %s", err, out)
		_ = os.RemoveAll(dir)
		return nil
	}
	start := pgCmd(runAs, filepath.Join(bin, "pg_ctl"),
		"-D", data, "-o", fmt.Sprintf("-p %d -k %s -h ''", port, sock), "-l", filepath.Join(dir, "log"), "-w", "start")
	if out, err := start.CombinedOutput(); err != nil {
		t.Logf("postgres backend skipped: pg_ctl start: %v: %s", err, out)
		_ = os.RemoveAll(dir)
		return nil
	}
	s := &pgServer{bin: bin, dir: dir, sock: sock, port: port}
	t.Cleanup(func() { s.stop() })

	dsn := fmt.Sprintf("host=%s port=%d user=postgres dbname=postgres sslmode=disable", sock, port)
	base, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Logf("postgres backend skipped: open: %v", err)
		s.stop()
		return nil
	}
	base.SetMaxOpenConns(4)
	// Wait for readiness.
	ok := false
	for i := 0; i < 40; i++ {
		if err := base.Ping(); err == nil {
			ok = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ok {
		t.Log("postgres backend skipped: never became ready")
		_ = base.Close()
		s.stop()
		return nil
	}
	s.base = base
	t.Logf("postgres backend ACTIVE (ephemeral, socket=%s port=%d)", sock, port)
	return s
}

// freshDB creates a new database on the ephemeral server for one test and
// returns a pool bound to it (isolates tables between subtests).
func (s *pgServer) freshDB(t *testing.T) *sql.DB {
	n := atomic.AddInt32(&s.n, 1)
	name := fmt.Sprintf("idem_%d", n)
	if _, err := s.base.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	dsn := fmt.Sprintf("host=%s port=%d user=postgres dbname=%s sslmode=disable", s.sock, s.port, name)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	db.SetMaxOpenConns(30)
	if err := Migrate(context.Background(), db, PostgresDialect{}); err != nil {
		t.Fatalf("migrate pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (s *pgServer) stop() {
	if s.base != nil {
		_ = s.base.Close()
		s.base = nil
	}
	runAs := pgRunAsUser()
	_ = pgCmd(runAs, filepath.Join(s.bin, "pg_ctl"), "-D", filepath.Join(s.dir, "data"), "-m", "immediate", "-w", "stop").Run()
	_ = os.RemoveAll(s.dir)
}

// pgRunAsUser returns the OS user to run postgres as. If we are root, use the
// `postgres` account (postgres refuses to run as root); otherwise run as self.
func pgRunAsUser() string {
	if os.Geteuid() == 0 {
		return "postgres"
	}
	return ""
}

func pgCmd(runAs, name string, args ...string) *exec.Cmd {
	if runAs != "" {
		return exec.Command("sudo", append([]string{"-n", "-u", runAs, name}, args...)...)
	}
	return exec.Command(name, args...)
}

