package sqlite

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// TestMigratesPass2LegacyDB opens a checked-in v0.1.0 database (built
// against the Pass 2 schema before the migration framework existed)
// and asserts:
//   - Open succeeds without data loss
//   - schema_version ends at the latest registered migration
//   - Pre-existing rows and their tags survive
//   - The embedding columns exist and default to NULL / 0
func TestMigratesPass2LegacyDB(t *testing.T) {
	src := filepath.Join("testdata", "v1_pass2.db")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("legacy fixture missing: %v", err)
	}

	dir := t.TempDir()
	dst := filepath.Join(dir, "v1.db")
	copyFile(t, src, dst)

	m, err := Open(dst)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	defer m.Close()

	// Schema version should reach the latest registered migration.
	sdb := m.(*store).db
	var v int
	if err := sdb.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	want := migrations[len(migrations)-1].version
	if v != want {
		t.Errorf("schema_version = %d; want %d", v, want)
	}

	// Embedding columns exist and default NULL / 0 BEFORE any Recall
	// touches the row (Recall bumps access_count).
	var embNull int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM memories WHERE embedding IS NULL`).Scan(&embNull); err != nil {
		t.Fatal(err)
	}
	if embNull != 1 {
		t.Errorf("expected 1 row with NULL embedding; got %d", embNull)
	}
	var ac int
	if err := sdb.QueryRow(`SELECT access_count FROM memories WHERE id='01V1'`).Scan(&ac); err != nil {
		t.Fatal(err)
	}
	if ac != 0 {
		t.Errorf("access_count default = %d; want 0 (pre-recall)", ac)
	}

	// Old row preserved through Recall.
	recs, err := m.Recall(context.Background(), "hello", hippo.MemoryQuery{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Content != "hello from v1" {
		t.Fatalf("expected hello row; got %+v", recs)
	}
	if len(recs[0].Tags) != 1 || recs[0].Tags[0] != "seeded" {
		t.Errorf("tags lost: %v", recs[0].Tags)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "fresh.db")
	m, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Close()

	// Reopen - migrate() should notice everything's already applied.
	m2, err := Open(dst)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer m2.Close()

	sdb := m2.(*store).db
	var rows int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(migrations) {
		t.Errorf("schema_version rows = %d; want %d (one per migration)", rows, len(migrations))
	}
}

func TestReconcileV1LegacyDetectsLegacy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bare.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// Create the memories table by hand - no schema_version - then
	// close, reopen through Open() to trigger migration.
	if _, err := db.Exec(`CREATE TABLE memories (id TEXT PRIMARY KEY, kind TEXT, timestamp INTEGER, content TEXT, importance REAL, metadata TEXT, created_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories(id,kind,timestamp,content,importance,metadata,created_at)
	    VALUES ('legacy','episodic',?,'bare legacy row',0.5,'{}',?)`, time.Now().UnixNano(), time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	m, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// Use recency (empty query) so the assertion doesn't depend on
	// FTS tokenisation of a hyphenated fixture.
	recs, err := m.Recall(context.Background(), "", hippo.MemoryQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Content != "bare legacy row" {
		t.Errorf("legacy row lost after reconcile: %+v", recs)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}
