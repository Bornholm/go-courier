package whatsapp

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// A pool handed over by the application holds the session: whatsmeow
// creates its tables there, and the pool stays open for its owner.
func TestOpenStore_UsesTheCallerPool(t *testing.T) {
	ctx := context.Background()

	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "app.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := NewProvider(WithDB(db, "sqlite3"))

	if _, err := p.openStore(ctx); err != nil {
		t.Fatalf("openStore: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'whatsmeow_device'`).Scan(&n); err != nil {
		t.Fatalf("pool closed or unusable after openStore: %v", err)
	}
	if n != 1 {
		t.Error("whatsmeow tables not created in the caller's pool")
	}
}

// sqlstore.NewWithDB panics on an unknown dialect: the provider must turn
// that into an error instead of crashing the application.
func TestOpenStore_RejectsAnUnknownDialect(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := NewProvider(WithDB(db, "mysql"))

	if _, err := p.openStore(context.Background()); err == nil {
		t.Fatal("an unknown dialect must be refused")
	}
}
