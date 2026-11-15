// Package checks runs verification queries against a restored cluster.
package checks

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shanurwan/cnpg-restore-check/api/v1alpha1"
)

// Target is how to reach the restored cluster.
type Target struct {
	Host     string
	Port     int
	User     string
	Password string
	SSLMode  string
}

// Result is the outcome of all checks in a run.
type Result struct {
	Checks []v1alpha1.CheckResult
	// DataAge is set when a freshness check ran and returned a timestamp.
	DataAge *time.Duration
}

// Passed reports whether every check passed.
func (r Result) Passed() bool {
	for _, c := range r.Checks {
		if !c.Passed {
			return false
		}
	}
	return true
}

// Connector opens a connection to a database. Swappable in tests.
type Connector func(ctx context.Context, t Target, database string) (Conn, error)

// Conn is the subset of *pgx.Conn the checks use.
type Conn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) error
	Close(ctx context.Context) error
}

type pgxConn struct{ *pgx.Conn }

func (c pgxConn) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := c.Conn.Exec(ctx, sql, args...)
	return err
}

// PGXConnector connects with pgx.
func PGXConnector(ctx context.Context, t Target, database string) (Conn, error) {
	ssl := t.SSLMode
	if ssl == "" {
		ssl = "require"
	}
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s&connect_timeout=10",
		url.QueryEscape(t.User), url.QueryEscape(t.Password), t.Host, t.Port, url.PathEscape(database), ssl)
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return pgxConn{c}, nil
}

// Run runs the configured checks. A connectivity check always runs first.
// runStart is the time the run started; freshness age is measured against it.
func Run(ctx context.Context, connect Connector, t Target, spec v1alpha1.ChecksSpec, runStart time.Time) Result {
	var res Result
	add := func(name string, err error) bool {
		cr := v1alpha1.CheckResult{Name: name, Passed: err == nil}
		if err != nil {
			cr.Message = truncate(err.Error(), 512)
		}
		res.Checks = append(res.Checks, cr)
		return err == nil
	}

	conn, err := connectWithRetry(ctx, connect, t, ConnectRetryFor)
	if !add("connect", err) {
		return res
	}
	databases, err := listDatabases(ctx, conn)
	_ = conn.Close(ctx)
	if !add("list-databases", err) {
		return res
	}

	if spec.Amcheck != nil {
		for _, db := range databases {
			add("amcheck/"+db, amcheck(ctx, connect, t, db, spec.Amcheck.Heap))
		}
	}

	for _, s := range spec.SQL {
		add("sql/"+s.Name, sqlAssert(ctx, connect, t, dbOr(s.Database), s.Query))
	}

	if f := spec.Freshness; f != nil {
		ts, err := scalarTime(ctx, connect, t, dbOr(f.Database), f.Query)
		if err == nil {
			age := runStart.Sub(ts)
			if age < 0 {
				age = 0
			}
			res.DataAge = &age
		}
		add("freshness", err)
	}
	return res
}

// ConnectRetryFor is how long the first connection is retried. The cluster's
// Ready condition can turn true before its -rw service routes to the primary.
var ConnectRetryFor = 3 * time.Minute

func connectWithRetry(ctx context.Context, connect Connector, t Target, d time.Duration) (Conn, error) {
	deadline := time.Now().Add(d)
	wait := time.Second
	for {
		conn, err := connect(ctx, t, "postgres")
		if err == nil {
			return conn, nil
		}
		if time.Now().Add(wait).After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
		if wait < 10*time.Second {
			wait *= 2
		}
	}
}

func dbOr(db string) string {
	if db == "" {
		return "postgres"
	}
	return db
}

func listDatabases(ctx context.Context, c Conn) ([]string, error) {
	rows, err := c.Query(ctx, "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dbs []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		dbs = append(dbs, n)
	}
	return dbs, rows.Err()
}

// btreeQuery checks every valid, permanent B-tree index.
const btreeQuery = `SELECT count(*) FROM (
  SELECT bt_index_check(index => c.oid, heapallindexed => $1)
  FROM pg_index i
  JOIN pg_class c ON c.oid = i.indexrelid
  JOIN pg_am am ON am.oid = c.relam
  WHERE am.amname = 'btree' AND c.relpersistence = 'p' AND i.indisready AND i.indisvalid
) s`

// heapQuery returns the number of corruption reports from verify_heapam over
// every permanent table and materialized view.
const heapQuery = `SELECT count(*) FROM pg_class c,
  LATERAL verify_heapam(c.oid, on_error_stop => false)
  WHERE c.relkind IN ('r', 'm') AND c.relpersistence = 'p'`

func amcheck(ctx context.Context, connect Connector, t Target, db string, heap bool) error {
	c, err := connect(ctx, t, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()
	if err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS amcheck"); err != nil {
		return fmt.Errorf("create extension amcheck: %w", err)
	}
	var indexes int64
	if err := c.QueryRow(ctx, btreeQuery, heap).Scan(&indexes); err != nil {
		return fmt.Errorf("bt_index_check: %w", err)
	}
	if heap {
		var problems int64
		if err := c.QueryRow(ctx, heapQuery).Scan(&problems); err != nil {
			return fmt.Errorf("verify_heapam: %w", err)
		}
		if problems > 0 {
			return fmt.Errorf("verify_heapam reported %d problem(s)", problems)
		}
	}
	return nil
}

func sqlAssert(ctx context.Context, connect Connector, t Target, db, query string) error {
	c, err := connect(ctx, t, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()
	rows, err := c.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	var vals []bool
	for rows.Next() {
		var v *bool
		if err := rows.Scan(&v); err != nil {
			return fmt.Errorf("query must return one boolean column: %w", err)
		}
		if v == nil {
			return fmt.Errorf("query returned NULL")
		}
		vals = append(vals, *v)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(vals) != 1 {
		return fmt.Errorf("query must return exactly one row, got %d", len(vals))
	}
	if !vals[0] {
		return fmt.Errorf("assertion returned false")
	}
	return nil
}

func scalarTime(ctx context.Context, connect Connector, t Target, db, query string) (time.Time, error) {
	c, err := connect(ctx, t, db)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = c.Close(ctx) }()
	var ts *time.Time
	if err := c.QueryRow(ctx, query).Scan(&ts); err != nil {
		return time.Time{}, fmt.Errorf("query must return one timestamptz: %w", err)
	}
	if ts == nil {
		return time.Time{}, fmt.Errorf("query returned NULL (no restored data?)")
	}
	return *ts, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
