package corestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrDamaged marks an authority file whose pages SQLite or the content hashes
// reject, as opposed to a busy, locked, rolled-back or future one.
var ErrDamaged = errors.New("corestore: authority file is damaged")

// SalvageResult describes an automatic rebuild of a damaged authority.
type SalvageResult struct {
	// Preserved is the directory holding the damaged files exactly as found.
	Preserved string
	// Dropped counts the state documents left behind, by kind.
	Dropped map[string]int
}

// Salvage rebuilds the damaged authority at path row by row and replaces it
// only when nothing but rebuildable state documents was lost: every other row
// of every table must copy and pass its content hash, the rebuilt file must
// pass the startup integrity check, and its write head must not be older than
// minimum, so order-safety state never rolls back. Anything else refuses and
// leaves path untouched; the damaged files are always kept under preserveDir
// first. The caller holds the persistence lock and has no connection open on
// path, since copying it beside an open connection would drop SQLite's locks.
func Salvage(ctx context.Context, path string, minimum AuthorityHead, preserveDir string, rebuildable func(kind string) bool) (SalvageResult, error) {
	result := SalvageResult{Dropped: map[string]int{}}
	preserved, err := preserveDamaged(path, preserveDir)
	if err != nil {
		return result, fmt.Errorf("keep the damaged authority: %w", err)
	}
	result.Preserved = preserved

	target := path + ".salvage"
	removeSalvage := func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(target + suffix)
		}
	}
	removeSalvage()
	dropped, err := rebuildInto(ctx, filepath.Join(preserved, filepath.Base(path)), target, minimum, rebuildable)
	if err != nil {
		removeSalvage()
		return result, err
	}
	result.Dropped = dropped
	if err := enforcePrivateModes(target); err != nil {
		removeSalvage()
		return result, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			removeSalvage()
			return result, fmt.Errorf("remove the damaged authority's %s: %w", suffix, err)
		}
	}
	if err := os.Rename(target, path); err != nil {
		removeSalvage()
		return result, fmt.Errorf("replace the damaged authority: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return result, fmt.Errorf("sync the authority directory: %w", err)
	}
	return result, nil
}

// preserveDamaged copies path and its sidecars, before any SQLite connection
// opens them, into a new private directory under dir.
func preserveDamaged(path, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	out, err := os.MkdirTemp(dir, "damaged-"+time.Now().UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		return "", err
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := copyPrivate(path+suffix, filepath.Join(out, filepath.Base(path)+suffix)); err != nil && !(suffix != "" && errors.Is(err, os.ErrNotExist)) {
			return out, err
		}
	}
	return out, syncDir(out)
}

func copyPrivate(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// rebuildInto copies the preserved damaged file into a fresh database at
// target and checks it, returning the rebuildable documents it left behind.
func rebuildInto(ctx context.Context, damaged, target string, minimum AuthorityHead, rebuildable func(string) bool) (map[string]int, error) {
	db, err := sql.Open("sqlite", sqliteDSN(target, 5*time.Second, false))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `ATTACH DATABASE ? AS old`, "file:"+damaged+"?mode=ro"); err != nil {
		return nil, fmt.Errorf("open the damaged authority: %w", err)
	}
	objects, err := damagedSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	for _, pragma := range []string{"user_version", "application_id"} {
		var v int64
		if err := db.QueryRowContext(ctx, `PRAGMA old.`+pragma).Scan(&v); err != nil {
			return nil, fmt.Errorf("read the damaged %s: %w", pragma, err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA main.%s=%d`, pragma, v)); err != nil {
			return nil, err
		}
	}
	// Tables first and empty of triggers, so copying a row fires nothing.
	for _, o := range objects {
		if o.kind == "table" {
			if _, err := db.ExecContext(ctx, o.sql); err != nil {
				return nil, fmt.Errorf("create %s: %w", o.name, err)
			}
		}
	}
	for _, o := range objects {
		if o.kind != "table" {
			continue
		}
		if err := salvageTable(ctx, db, o); err != nil {
			return nil, err
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO main.sqlite_sequence SELECT * FROM old.sqlite_sequence`); err != nil && !strings.Contains(err.Error(), "no such table") {
		return nil, fmt.Errorf("copy sqlite_sequence: %w", err)
	}
	dropped, err := dropDamagedStateDocuments(ctx, db, rebuildable)
	if err != nil {
		return nil, err
	}
	for _, o := range objects {
		if o.kind != "table" {
			if _, err := db.ExecContext(ctx, o.sql); err != nil {
				return nil, fmt.Errorf("create %s: %w", o.name, err)
			}
		}
	}
	if _, err := db.ExecContext(ctx, `DETACH DATABASE old`); err != nil {
		return nil, err
	}
	report, err := checkIntegrityDB(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("check the rebuilt authority: %w", err)
	}
	if !report.OK() {
		return nil, fmt.Errorf("check the rebuilt authority: %w", integrityFailure(report))
	}
	head, err := readAuthorityHead(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("read the rebuilt authority head: %w", err)
	}
	if err := requireMinimumHead(head, minimum); err != nil {
		return nil, fmt.Errorf("the rebuilt authority: %w", err)
	}
	// Fold the WAL into the file, so the rename moves every committed page.
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA main.journal_mode=DELETE`).Scan(&mode); err != nil || mode != "delete" {
		return nil, fmt.Errorf("fold the rebuilt authority's WAL: mode=%q err=%v", mode, err)
	}
	return dropped, nil
}

type salvageObject struct{ kind, name, table, sql string }

func damagedSchema(ctx context.Context, db *sql.DB) ([]salvageObject, error) {
	rows, err := db.QueryContext(ctx, `SELECT type, name, tbl_name, sql FROM old.sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("read the damaged schema: %w", err)
	}
	defer rows.Close()
	var out []salvageObject
	for rows.Next() {
		var o salvageObject
		if err := rows.Scan(&o.kind, &o.name, &o.table, &o.sql); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the damaged schema: %w", err)
	}
	return out, nil
}

// salvageTable copies one table whole, or, when a page refuses, row by row
// over rowid ranges halved down to the unreadable rows. Only state_documents
// may lose rows here, and only rows whose kind still reads; they are left
// for dropDamagedStateDocuments to judge.
func salvageTable(ctx context.Context, db *sql.DB, o salvageObject) error {
	quoted := `"` + strings.ReplaceAll(o.name, `"`, `""`) + `"`
	// A state document read through a cross-linked page can come back as
	// garbage that fails the table's JSON or digest checks instead of as a
	// corrupt page; for that table both mean damaged content.
	damagedRead := func(err error) bool {
		code, _ := criticalSQLiteCode(err)
		return code == "corrupt" || o.name == "state_documents" && isConstraint(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO main.`+quoted+` SELECT * FROM old.`+quoted); err == nil {
		return nil
	} else if !damagedRead(err) {
		return fmt.Errorf("copy %s: %w", o.name, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM main.`+quoted); err != nil {
		return err
	}
	if strings.Contains(strings.ToUpper(o.sql), "WITHOUT ROWID") {
		return fmt.Errorf("table %s is damaged and has no rowid to salvage by", o.name)
	}
	var last sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max(rowid) FROM old.`+quoted).Scan(&last); err != nil {
		return fmt.Errorf("find the rows of damaged %s: %w", o.name, err)
	}
	var copyRange func(lo, hi int64) error
	copyRange = func(lo, hi int64) error {
		_, err := db.ExecContext(ctx, `INSERT INTO main.`+quoted+` SELECT * FROM old.`+quoted+` WHERE rowid BETWEEN ? AND ?`, lo, hi)
		if err == nil {
			return nil
		}
		if !damagedRead(err) {
			return fmt.Errorf("copy %s: %w", o.name, err)
		}
		if lo < hi {
			mid := lo + (hi-lo)/2
			if _, err := db.ExecContext(ctx, `DELETE FROM main.`+quoted+` WHERE rowid BETWEEN ? AND ?`, lo, hi); err != nil {
				return err
			}
			if err := copyRange(lo, mid); err != nil {
				return err
			}
			return copyRange(mid+1, hi)
		}
		if o.name != "state_documents" {
			return fmt.Errorf("row %d of %s cannot be read", lo, o.name)
		}
		var kind string
		if err := db.QueryRowContext(ctx, `SELECT kind FROM old.state_documents WHERE rowid=?`, lo).Scan(&kind); err != nil {
			return fmt.Errorf("state document row %d cannot be identified: %w", lo, err)
		}
		// The row's keys read but its content does not: keep a placeholder
		// whose hash cannot match, so dropDamagedStateDocuments judges the kind.
		_, err = db.ExecContext(ctx, `INSERT INTO main.state_documents(rowid, scope_key, kind, revision, document_json, document_sha256, updated_at)
SELECT rowid, scope_key, kind, max(revision, 1), CAST('{}' AS BLOB), zeroblob(32), '' FROM old.state_documents WHERE rowid=?`, lo)
		if err != nil {
			return fmt.Errorf("state document row %d (%s) cannot be kept for judgement: %w", lo, kind, err)
		}
		return nil
	}
	if !last.Valid {
		return nil
	}
	const chunk = 512
	for lo := int64(1); lo <= last.Int64; lo += chunk {
		if err := copyRange(lo, min(lo+chunk-1, last.Int64)); err != nil {
			return err
		}
	}
	return nil
}

// dropDamagedStateDocuments removes rebuilt state documents whose content no
// longer matches its hash, and refuses if any of them is not rebuildable.
// Every other hashed table must match in full.
func dropDamagedStateDocuments(ctx context.Context, db *sql.DB, rebuildable func(string) bool) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT rowid, kind, document_json, document_sha256 FROM main.state_documents`)
	if err != nil {
		return nil, err
	}
	var bad []int64
	dropped := map[string]int{}
	var refused []string
	for rows.Next() {
		var rowid int64
		var kind string
		var payload, stored []byte
		if err := rows.Scan(&rowid, &kind, &payload, &stored); err != nil {
			rows.Close()
			return nil, err
		}
		digest := sha256.Sum256(payload)
		if len(stored) == sha256.Size && string(stored) == string(digest[:]) {
			continue
		}
		if !rebuildable(kind) {
			refused = append(refused, kind)
			continue
		}
		bad = append(bad, rowid)
		dropped[kind]++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(refused) > 0 {
		slices.Sort(refused)
		return nil, fmt.Errorf("damaged state documents the daemon cannot rebuild: %s", strings.Join(slices.Compact(refused), ", "))
	}
	for _, rowid := range bad {
		if _, err := db.ExecContext(ctx, `DELETE FROM main.state_documents WHERE rowid=?`, rowid); err != nil {
			return nil, err
		}
	}
	if err := checkApplicationHashes(ctx, db); err != nil {
		return nil, fmt.Errorf("the rebuilt authority: %w", err)
	}
	return maps.Clone(dropped), nil
}
