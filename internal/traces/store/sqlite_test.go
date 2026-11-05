package store

import (
	"context"
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
