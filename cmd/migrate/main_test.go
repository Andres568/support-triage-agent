package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The binary must ship every migration the goose CLI applies in dev.
func TestEmbeddedMigrationsMatchDir(t *testing.T) {
	onDisk, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil || len(onDisk) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(onDisk))
	}
	// sql.Open does not connect; the provider only needs the handle.
	conn, err := sql.Open("pgx", "postgres://unused")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	p, err := newProvider(conn)
	if err != nil {
		t.Fatal(err)
	}
	sources := p.ListSources()
	if len(sources) != len(onDisk) {
		t.Fatalf("embedded %d migrations, db/migrations has %d", len(sources), len(onDisk))
	}
	for i, s := range sources {
		if filepath.Base(s.Path) != filepath.Base(onDisk[i]) {
			t.Errorf("migration %d: embedded %s, on disk %s", i, s.Path, onDisk[i])
		}
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	if err := run("down", nil, "postgres://x", nil); err == nil {
		t.Error("down: want error (prod job only goes up)")
	}
	if err := run("up", nil, "", nil); err == nil {
		t.Error("empty DATABASE_URL: want error")
	}
	if err := run("purge", []string{"-days=0"}, "postgres://x", nil); err == nil {
		t.Error("purge -days=0: want error (would delete every transcript)")
	}
	if err := run("purge", []string{"-bogus"}, "postgres://x", nil); err == nil {
		t.Error("unknown flag: want error")
	}
}
