package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestOpenSQLiteSpecialPathAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "odd #name? 100%", "traces")
	st, err := OpenSQLite(dir)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	ctx := context.Background()
	if err = st.PutKV(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, DBFileName)); err != nil {
		t.Fatalf("database not created at the literal path: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, errStat := os.Stat(dir)
		if errStat != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("folder mode = %v, want 0700", info.Mode().Perm())
		}
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	value, updated, err := reopened.GetKV(ctx, "k")
	if err != nil || string(value) != "v" || time.Since(updated) > time.Minute {
		t.Errorf("GetKV = %q %v %v", value, updated, err)
	}
}

func TestBlobsAreStoredOnceAndCompressed(t *testing.T) {
	st, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	raw := []byte(`{"role":"user","blocks":[{"kind":"user","text":"hello hello hello hello hello hello hello hello"}]}`)
	var added []int64
	err = st.Write(ctx, func(tx Tx) error {
		for i := 0; i < 3; i++ {
			n, errPut := tx.PutBlob("h1", RoleMessage, raw)
			if errPut != nil {
				return errPut
			}
			added = append(added, n)
		}
		return tx.InsertTurnBlobs("t1", []BlobRef{{Ord: 0, Hash: "h1", Role: RoleMessage, IsNew: true}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if added[0] == 0 || added[1] != 0 || added[2] != 0 {
		t.Errorf("PutBlob sizes = %v, want only the first insert to add bytes", added)
	}
	got, err := st.Blob(ctx, "h1")
	if err != nil || string(got) != string(raw) {
		t.Errorf("Blob round trip = %q %v", got, err)
	}
	if missing, _ := st.Blob(ctx, "nope"); missing != nil {
		t.Errorf("missing blob returned data")
	}
}

// legacySessions is the sessions table of the first release: no project_source
// and no path_project column.
const legacySessions = `CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	root_id TEXT NOT NULL,
	parent_id TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	project_name TEXT NOT NULL DEFAULT '',
	project_key TEXT NOT NULL DEFAULT '',
	project_rule TEXT NOT NULL DEFAULT '',
	header_project TEXT NOT NULL DEFAULT '',
	folder TEXT NOT NULL DEFAULT '',
	git_remote TEXT NOT NULL DEFAULT '',
	client_project_hash TEXT NOT NULL DEFAULT '',
	client TEXT NOT NULL DEFAULT '',
	client_version TEXT NOT NULL DEFAULT '',
	key_label TEXT NOT NULL DEFAULT '',
	tags TEXT NOT NULL DEFAULT '[]',
	machine TEXT NOT NULL DEFAULT '',
	client_ip TEXT NOT NULL DEFAULT '',
	device_id TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	synthetic INTEGER NOT NULL DEFAULT 0,
	started_at INTEGER NOT NULL,
	last_at INTEGER NOT NULL
)`

func TestOpenSQLiteMigratesAnOlderDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DBFileName)
	old, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.Exec(legacySessions); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Exec(`INSERT INTO sessions (id, root_id, title, project_name, folder, started_at, last_at) VALUES ('old', 'old', 'Old title', 'app', '~/code/app', 1000, 2000)`); err != nil {
		t.Fatal(err)
	}
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenSQLite(dir)
	if err != nil {
		t.Fatalf("OpenSQLite on an older database: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	sessions, err := st.AllSessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].Title != "Old title" || sessions[0].ProjectSource != "" || sessions[0].PathProject != "" {
		t.Fatalf("sessions after migration = %+v %v", sessions, err)
	}
	sess := sessions[0]
	sess.ProjectSource, sess.PathProject = "folder", "from-path"
	if err = st.Write(ctx, func(tx Tx) error { return tx.UpsertSession(&sess) }); err != nil {
		t.Fatal(err)
	}
	got, err := st.Sessions(ctx, []string{"old"})
	if err != nil || len(got) != 1 || got[0].ProjectSource != "folder" || got[0].PathProject != "from-path" {
		t.Fatalf("session after upsert = %+v %v", got, err)
	}
	// Opening again must not try to add the columns twice.
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenSQLite(dir)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	_ = again.Close()
}
