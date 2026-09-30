package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Conversions between Go values and the column types of 03 §3.3: times are INTEGER unix milliseconds (UTC), and an
// optional time, ID, text or hash is NULL exactly when its Go value is the zero value.

// fromMS converts a DB time to a UTC time.Time.
func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// fromNullMS converts a nullable DB time; NULL is the zero time.
func fromNullMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromMS(v.Int64)
}

// msOrNull converts an optional time for the DB: the zero time is NULL.
func msOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return unixMS(t)
}

// normMS rounds t to what the DB keeps (UTC milliseconds), so that a struct a Create method filled in compares equal
// to the row read back. The zero time stays zero.
func normMS(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return fromMS(unixMS(t))
}

// strOrNull converts an optional text or ID for the DB: "" is NULL.
func strOrNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// bytesOrNull converts an optional hash for the DB: an empty slice is NULL.
func bytesOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// boolInt converts a bool for an INTEGER 0/1 column.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// requireTime fails when a NOT NULL time that has no sensible default is missing.
func requireTime(method, field string, t time.Time) error {
	if t.IsZero() {
		return fmt.Errorf("store: %s: %s is required", method, field)
	}
	return nil
}

// requireBytes fails when a NOT NULL hash is missing.
func requireBytes(method, field string, b []byte) error {
	if len(b) == 0 {
		return fmt.Errorf("store: %s: %s is required", method, field)
	}
	return nil
}

// orNow returns t, or the store's clock when t is zero, rounded to milliseconds.
func (q *Q) orNow(t time.Time) time.Time {
	if t.IsZero() {
		t = q.now()
	}
	return normMS(t)
}

// uniqueViolation returns the "table.column" that a UNIQUE or PRIMARY KEY violation names ("users.username_key"),
// or "" when err is something else. For a constraint over several columns it returns the first one.
func uniqueViolation(err error) string {
	var se *sqlite.Error
	if !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_CONSTRAINT {
		return ""
	}
	const marker = "UNIQUE constraint failed: "
	msg := se.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	if j := strings.IndexAny(rest, " ,"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// conflictOr turns a UNIQUE violation into *ConflictError naming the column, and wraps any other error with what.
// A primary key collision that survived withNewID's retry is not a user-visible conflict: it stays a plain error.
func conflictOr(what string, err error) error {
	if tc := uniqueViolation(err); tc != "" {
		if _, col, _ := strings.Cut(tc, "."); col != "id" {
			return &ConflictError{Column: col}
		}
	}
	return fmt.Errorf("store: %s: %w", what, err)
}

// withNewID runs insert with a new ID. When the ID collides with an existing row of table (a UNIQUE violation of
// table.id), it retries once with another ID (03 §3.1). It returns the ID that was inserted.
func (q *Q) withNewID(table string, insert func(id string) error) (string, error) {
	for attempt := 0; ; attempt++ {
		id := q.newID()
		err := insert(id)
		if err == nil {
			return id, nil
		}
		if attempt == 0 && uniqueViolation(err) == table+".id" {
			continue
		}
		return "", err
	}
}

// affected returns the number of rows a statement changed.
func affected(res sql.Result) (int64, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: rows affected: %w", err)
	}
	return n, nil
}

// execCount runs a write statement and returns the number of rows it changed.
func (q *Q) execCount(what, query string, args ...any) (int, error) {
	res, err := q.exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("store: %s: %w", what, err)
	}
	n, err := affected(res)
	return int(n), err
}

// execOne runs a write statement that must change exactly one row; no row is ErrNotFound.
func (q *Q) execOne(what, query string, args ...any) error {
	res, err := q.exec(query, args...)
	if err != nil {
		return conflictOr(what, err)
	}
	n, err := affected(res)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanner is *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// queryAll runs a read query and calls scan for each row.
func (q *Q) queryAll(what, query string, args []any, scan func(scanner) error) error {
	rows, err := q.tx.QueryContext(q.ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	return eachRow(what, rows, scan)
}

// writeReturning runs a write statement with a RETURNING clause and calls scan for each returned row. It fails
// inside Read, like exec.
func (q *Q) writeReturning(what, query string, args []any, scan func(scanner) error) error {
	if !q.writable {
		return errReadOnlyTx
	}
	rows, err := q.tx.QueryContext(q.ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	return eachRow(what, rows, scan)
}

func eachRow(what string, rows *sql.Rows, scan func(scanner) error) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("store: %s: %w", what, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	return nil
}

// deleteReturningIDs runs a DELETE … RETURNING id and returns the IDs, sorted.
func deleteReturningIDs[T ~string](q *Q, what, query string, args ...any) ([]T, error) {
	var ids []T
	err := q.writeReturning(what, query, args, func(s scanner) error {
		var id string
		if err := s.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, T(id))
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(ids)
	return ids, nil
}

// queryOne runs a one-row read query; no row is ErrNotFound.
func (q *Q) queryOne(what, query string, args []any, dest ...any) error {
	err := q.tx.QueryRowContext(q.ctx, query, args...).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	return nil
}

// placeholders returns "?, ?, …" for n arguments.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// prefixCols prefixes every column of a comma-separated list with alias ("u.id, u.username, …").
func prefixCols(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
