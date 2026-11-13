package hue

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSharedSQLiteHelper is the child side of TestSharedSQLiteConcurrentOpen:
// it waits for the barrier file, then opens the store and stages one row.
func TestSharedSQLiteHelper(t *testing.T) {
	db := os.Getenv("HUE_WAL_TEST_DB")
	if db == "" {
		t.Skip("child process of TestSharedSQLiteConcurrentOpen only")
	}
	barrier := os.Getenv("HUE_WAL_TEST_BARRIER")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(barrier); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("barrier never opened")
		}
		time.Sleep(time.Millisecond)
	}
	h, err := openSharedSQLite(db, txSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if _, err := h.Exec("INSERT INTO staged (tx, kind, target, body, created) VALUES ('t', 'light', ?, '{}', 0)",
		os.Getenv("HUE_WAL_TEST_ID")); err != nil {
		t.Fatal(err)
	}
}

// TestSharedSQLiteConcurrentOpen opens one fresh database from 8 processes at
// once (the race that failed CI on Windows with "disk I/O error (1546)").
func TestSharedSQLiteConcurrentOpen(t *testing.T) {
	if os.Getenv("HUE_WAL_TEST_DB") != "" {
		t.Skip("parent only")
	}
	const n = 8
	dir := t.TempDir()
	db := filepath.Join(dir, "shared.db")
	barrier := filepath.Join(dir, "go")
	cmds := make([]*exec.Cmd, n)
	outs := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSharedSQLiteHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "HUE_WAL_TEST_DB="+db, "HUE_WAL_TEST_BARRIER="+barrier, fmt.Sprintf("HUE_WAL_TEST_ID=%d", i))
		cmds[i] = cmd
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = cmds[i].CombinedOutput()
		}(i)
	}
	time.Sleep(500 * time.Millisecond) // let every child start and spin on the barrier
	if err := os.WriteFile(barrier, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Errorf("process %d: %v\n%s", i, errs[i], outs[i])
		}
	}
	h, err := openSharedSQLite(db, txSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	var mode string
	if err := h.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	var tables, indexes, rows int
	if err := h.QueryRow("SELECT count(*) FILTER (WHERE type = 'table' AND name = 'staged'), "+
		"count(*) FILTER (WHERE type = 'index' AND name = 'staged_tx') FROM sqlite_master").Scan(&tables, &indexes); err != nil {
		t.Fatal(err)
	}
	if tables != 1 || indexes != 1 {
		t.Fatalf("schema objects: %d staged tables, %d staged_tx indexes; want 1 each", tables, indexes)
	}
	if err := h.QueryRow("SELECT count(DISTINCT target) FROM staged").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != n {
		t.Fatalf("%d processes staged a row; want %d", rows, n)
	}
}
