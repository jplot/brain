package db

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/jplot/brain/internal/paths"
)

const IndexVersion = 6

type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Open passes the per-connection pragmas in the DSN so every pooled
// connection gets them; journal_mode=WAL is persisted in the file.
func Open(p paths.Paths, busy time.Duration) (*sql.DB, error) {
	if err := p.EnsureHome(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p.DB(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()

	query := url.Values{"_pragma": {
		"journal_mode(WAL)",
		"busy_timeout(" + strconv.FormatInt(busy.Milliseconds(), 10) + ")",
		"foreign_keys(ON)",
		"secure_delete(ON)",
	}}
	dsn := (&url.URL{Scheme: "file", Path: p.DB(), RawQuery: query.Encode()}).String()
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(stateSchema + indexSchema); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func RebuildIndex(d *sql.DB) error { return rebuild(d, indexSchema) }

func rebuild(d *sql.DB, schema string) error {
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	var drops strings.Builder
	for _, table := range indexTables {
		drops.WriteString("DROP TABLE IF EXISTS " + table + ";")
	}
	if _, err := conn.ExecContext(ctx, drops.String()+schema); err != nil {
		conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}
