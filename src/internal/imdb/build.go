package imdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite" // pure-Go SQLite driver "sqlite"
)

// schemaVersion changes whenever the database layout changes; a database
// with another version is rebuilt.
const schemaVersion = "1"

// Dataset files. basics, ratings and episode are required; the rest can be
// skipped (Options.Skip) to save disk and import time.
const (
	FileBasics     = "title.basics"
	FileRatings    = "title.ratings"
	FileEpisode    = "title.episode"
	FileCrew       = "title.crew"
	FileAkas       = "title.akas"
	FilePrincipals = "title.principals"
	FileNames      = "name.basics"
)

// AllFiles in import order.
var AllFiles = []string{FileRatings, FileCrew, FileBasics, FileEpisode, FileAkas, FilePrincipals, FileNames}

var requiredFiles = map[string]bool{FileBasics: true, FileRatings: true, FileEpisode: true}

var headers = map[string][]string{
	FileBasics:     {"tconst", "titleType", "primaryTitle", "originalTitle", "isAdult", "startYear", "endYear", "runtimeMinutes", "genres"},
	FileRatings:    {"tconst", "averageRating", "numVotes"},
	FileEpisode:    {"tconst", "parentTconst", "seasonNumber", "episodeNumber"},
	FileCrew:       {"tconst", "directors", "writers"},
	FileAkas:       {"titleId", "ordering", "title", "region", "language", "types", "attributes", "isOriginalTitle"},
	FilePrincipals: {"tconst", "ordering", "nconst", "category", "job", "characters"},
	FileNames:      {"nconst", "primaryName", "birthYear", "deathYear", "primaryProfession", "knownForTitles"},
}

const schema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE title (
  tid INTEGER PRIMARY KEY, type TEXT NOT NULL, primary_title TEXT NOT NULL, original_title TEXT,
  adult INTEGER NOT NULL, start_year INTEGER, end_year INTEGER, runtime INTEGER, genres TEXT,
  rating REAL, votes INTEGER, directors TEXT, writers TEXT);
CREATE TABLE episode (tid INTEGER PRIMARY KEY, parent INTEGER NOT NULL, season INTEGER, episode INTEGER);
CREATE TABLE aka (
  id INTEGER PRIMARY KEY, tid INTEGER NOT NULL, title TEXT NOT NULL, norm TEXT NOT NULL,
  kind TEXT NOT NULL, region TEXT, language TEXT, cjk INTEGER NOT NULL);
CREATE TABLE principal (
  tid INTEGER NOT NULL, ord INTEGER NOT NULL, nid INTEGER NOT NULL, category TEXT NOT NULL,
  job TEXT, characters TEXT, PRIMARY KEY (tid, ord)) WITHOUT ROWID;
CREATE TABLE name (
  nid INTEGER PRIMARY KEY, name TEXT NOT NULL, norm TEXT NOT NULL, birth INTEGER, death INTEGER,
  professions TEXT, known_for TEXT);
`

const indexes = `
CREATE INDEX aka_norm ON aka (norm);
CREATE INDEX aka_tid ON aka (tid);
CREATE INDEX aka_cjk ON aka (norm) WHERE cjk = 1;
CREATE INDEX episode_parent ON episode (parent, season, episode);
CREATE INDEX principal_nid ON principal (nid);
CREATE INDEX name_norm ON name (norm);
CREATE VIRTUAL TABLE aka_fts USING fts5 (norm, content='aka', content_rowid='id', tokenize='unicode61', detail='column');
INSERT INTO aka_fts (aka_fts) VALUES ('rebuild');
CREATE VIRTUAL TABLE name_fts USING fts5 (norm, content='name', content_rowid='nid', tokenize='unicode61', detail='column');
INSERT INTO name_fts (name_fts) VALUES ('rebuild');
`

// Version identifies one published copy of a dataset file.
type Version struct {
	ETag         string    `json:"etag"`
	LastModified time.Time `json:"last_modified"`
	Size         int64     `json:"size"`
}

func (v Version) same(o Version) bool {
	if v.ETag != "" || o.ETag != "" {
		return v.ETag == o.ETag
	}
	return v.LastModified.Equal(o.LastModified) && v.Size == o.Size
}

// Info describes a built database (stored in its meta table).
type Info struct {
	Schema   string             `json:"schema"`
	BuiltAt  time.Time          `json:"built_at"`
	Seconds  float64            `json:"build_seconds"`
	Versions map[string]Version `json:"versions"`
	Counts   map[string]int64   `json:"counts"`
}

// build imports the downloaded files (paths by dataset name) into a new
// SQLite database at out. Files missing from paths are skipped.
func build(ctx context.Context, out string, paths map[string]string, versions map[string]Version, log *slog.Logger) (*Info, error) {
	start := time.Now()
	for f := range requiredFiles {
		if paths[f] == "" {
			return nil, fmt.Errorf("required dataset %s missing", f)
		}
	}
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		_ = os.Remove(out + suffix)
	}
	db, err := sql.Open("sqlite", "file:"+out+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)"+
		"&_pragma=locking_mode(EXCLUSIVE)&_pragma=cache_size(-524288)&_pragma=temp_store(FILE)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ok := false
	defer func() {
		db.Close()
		if !ok {
			_ = os.Remove(out)
		}
	}()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	b := &builder{ctx: ctx, db: db, paths: paths, log: log, counts: map[string]int64{}}
	steps := []struct {
		file string
		run  func() error
	}{
		{FileRatings, b.ratings}, {FileBasics, b.basics}, {FileCrew, b.crew}, {FileEpisode, b.episodes},
		{FileAkas, b.akas}, {"", b.titleNames}, {FilePrincipals, b.principals}, {FileNames, b.names},
	}
	for _, s := range steps {
		if s.file != "" && paths[s.file] == "" {
			continue
		}
		t := time.Now()
		if err := s.run(); err != nil {
			return nil, err
		}
		if s.file != "" {
			log.Info("imported", "file", s.file, "seconds", int(time.Since(t).Seconds()))
		}
	}
	t := time.Now()
	if _, err := db.ExecContext(ctx, indexes); err != nil {
		return nil, fmt.Errorf("indexes: %w", err)
	}
	log.Info("indexed", "seconds", int(time.Since(t).Seconds()))
	info := &Info{Schema: schemaVersion, BuiltAt: time.Now().UTC(), Versions: versions, Counts: b.counts}
	info.Seconds = time.Since(start).Seconds()
	raw, _ := json.Marshal(info)
	if _, err := db.ExecContext(ctx, `INSERT INTO meta VALUES ('info', ?); ANALYZE;`, string(raw)); err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	ok = true
	return info, nil
}

type rating struct {
	value float64
	votes int64
}

type builder struct {
	ctx        context.Context
	db         *sql.DB
	paths      map[string]string
	log        *slog.Logger
	counts     map[string]int64
	ratingMap  map[int64]rating
	searchable bitset // non-episode titles: indexed for search, get akas, crew and principals
	named      bitset // titles that already have aka rows
}

func (b *builder) ratings() error {
	b.ratingMap = make(map[int64]rating, 1<<21)
	return b.scan(FileRatings, "", 0, func(_ *inserter, _ *sql.Tx, f [][]byte) error {
		tid, ok := id("tt", f[0])
		v, err1 := strconv.ParseFloat(string(f[1]), 64)
		n, err2 := strconv.ParseInt(string(f[2]), 10, 64)
		if ok && err1 == nil && err2 == nil {
			b.ratingMap[tid] = rating{v, n}
		}
		return nil
	})
}

// crew sets directors and writers of searchable titles (episodes' crew is
// not kept: it is most of the file and rarely useful).
func (b *builder) crew() error {
	var stmt *sql.Stmt
	return b.scan(FileCrew, "", 0, func(_ *inserter, tx *sql.Tx, f [][]byte) error {
		tid, ok := id("tt", f[0])
		if !ok || !b.searchable.has(tid) {
			return nil
		}
		d, w := idList("nm", f[1]), idList("nm", f[2])
		if d == nil && w == nil {
			return nil
		}
		if stmt == nil {
			var err error
			if stmt, err = tx.PrepareContext(b.ctx, `UPDATE title SET directors = ?, writers = ? WHERE tid = ?`); err != nil {
				return err
			}
		}
		_, err := stmt.ExecContext(b.ctx, d, w, tid)
		return err
	})
}

func (b *builder) basics() error {
	defer func() { b.ratingMap = nil }()
	return b.scan(FileBasics, "title", 13, func(ins *inserter, _ *sql.Tx, f [][]byte) error {
		tid, ok := id("tt", f[0])
		if !ok {
			return nil
		}
		typ := string(f[1])
		adult := 0
		if string(f[4]) == "1" {
			adult = 1
		}
		if typ != "tvEpisode" {
			b.searchable.set(tid)
		}
		var rv, rn any
		if r, ok := b.ratingMap[tid]; ok {
			rv, rn = r.value, r.votes
		}
		orig := str(f[3])
		if orig == string(f[2]) {
			orig = nil
		}
		var genres any
		if !isNull(f[8]) {
			genres = string(f[8])
		}
		b.counts["titles"]++
		return ins.add(tid, typ, string(f[2]), orig, adult, num(f[5]), num(f[6]), num(f[7]), genres, rv, rn, nil, nil)
	})
}

func (b *builder) episodes() error {
	return b.scan(FileEpisode, "episode", 4, func(ins *inserter, _ *sql.Tx, f [][]byte) error {
		tid, ok1 := id("tt", f[0])
		parent, ok2 := id("tt", f[1])
		if !ok1 || !ok2 {
			return nil
		}
		b.counts["episodes"]++
		return ins.add(tid, parent, num(f[2]), num(f[3]))
	})
}

func isCJK(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return true
		}
	}
	return false
}

func (b *builder) addAka(ins *inserter, seen map[string]bool, tid int64, title, kind string, region, lang any) error {
	n := Normalize(title)
	if n == "" || seen[n] {
		return nil
	}
	seen[n] = true
	cjk := 0
	if isCJK(n) {
		cjk = 1
	}
	b.counts["akas"]++
	return ins.add(nil, tid, title, n, kind, region, lang, cjk)
}

// akas adds alternative titles of searchable titles, deduplicated per title
// by normalized form, after the title's own primary and original titles.
func (b *builder) akas() error {
	var titleStmt *sql.Stmt
	cur := int64(-1)
	seen := map[string]bool{}
	return b.scan(FileAkas, "aka", 8, func(ins *inserter, _ *sql.Tx, f [][]byte) error {
		tid, ok := id("tt", f[0])
		if !ok || !b.searchable.has(tid) || isNull(f[2]) {
			return nil
		}
		if tid != cur {
			cur = tid
			clear(seen)
			if titleStmt == nil {
				var err error
				if titleStmt, err = ins.tx.PrepareContext(b.ctx, `SELECT primary_title, original_title FROM title WHERE tid = ?`); err != nil {
					return err
				}
			}
			if !b.named.has(tid) {
				b.named.set(tid)
				if err := b.ownTitles(ins, titleStmt, seen, tid); err != nil {
					return err
				}
			} else { // rows of this title were not contiguous: rebuild the seen set
				if err := b.seenFor(ins, seen, tid); err != nil {
					return err
				}
			}
		}
		kind := "aka"
		if string(f[7]) == "1" {
			kind = "original"
		}
		return b.addAka(ins, seen, tid, string(f[2]), kind, str(f[3]), str(f[4]))
	})
}

func (b *builder) ownTitles(ins *inserter, stmt *sql.Stmt, seen map[string]bool, tid int64) error {
	var primary string
	var orig sql.NullString
	if err := stmt.QueryRowContext(b.ctx, tid).Scan(&primary, &orig); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if err := b.addAka(ins, seen, tid, primary, "primary", nil, nil); err != nil {
		return err
	}
	if orig.Valid {
		return b.addAka(ins, seen, tid, orig.String, "original", nil, nil)
	}
	return nil
}

func (b *builder) seenFor(ins *inserter, seen map[string]bool, tid int64) error {
	if err := ins.flush(); err != nil {
		return err
	}
	rows, err := ins.tx.QueryContext(b.ctx, `SELECT norm FROM aka WHERE tid = ?`, tid)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		seen[n] = true
	}
	return rows.Err()
}

// titleNames gives searchable titles without akas rows their own names.
func (b *builder) titleNames() error {
	tx, err := b.db.BeginTx(b.ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(b.ctx, `SELECT tid, primary_title, original_title FROM title WHERE type <> 'tvEpisode'`)
	if err != nil {
		return err
	}
	type own struct {
		tid     int64
		primary string
		orig    sql.NullString
	}
	var todo []own
	for rows.Next() {
		var o own
		if err := rows.Scan(&o.tid, &o.primary, &o.orig); err != nil {
			rows.Close()
			return err
		}
		if !b.named.has(o.tid) {
			todo = append(todo, o)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	ins := newInserter(b.ctx, tx, "aka", 8)
	seen := map[string]bool{}
	for _, o := range todo {
		clear(seen)
		b.named.set(o.tid)
		if err := b.addAka(ins, seen, o.tid, o.primary, "primary", nil, nil); err != nil {
			return err
		}
		if o.orig.Valid {
			if err := b.addAka(ins, seen, o.tid, o.orig.String, "original", nil, nil); err != nil {
				return err
			}
		}
	}
	if err := ins.flush(); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *builder) principals() error {
	return b.scan(FilePrincipals, "principal", 6, func(ins *inserter, _ *sql.Tx, f [][]byte) error {
		tid, ok1 := id("tt", f[0])
		nid, ok2 := id("nm", f[2])
		if !ok1 || !ok2 || !b.searchable.has(tid) {
			return nil
		}
		b.counts["principals"]++
		return ins.add(tid, num(f[1]), nid, string(f[3]), str(f[4]), str(f[5]))
	})
}

func (b *builder) names() error {
	return b.scan(FileNames, "name", 7, func(ins *inserter, _ *sql.Tx, f [][]byte) error {
		nid, ok := id("nm", f[0])
		if !ok || isNull(f[1]) {
			return nil
		}
		b.counts["names"]++
		var prof any
		if !isNull(f[4]) {
			prof = string(f[4])
		}
		return ins.add(nid, string(f[1]), Normalize(string(f[1])), num(f[2]), num(f[3]), prof, idList("tt", f[5]))
	})
}

// scan streams a file's rows to fn inside one transaction, with a
// multi-row inserter for table (none when table is "") flushed before commit.
func (b *builder) scan(file, table string, cols int, fn func(ins *inserter, tx *sql.Tx, f [][]byte) error) error {
	r, err := openTSV(b.paths[file], file, headers[file])
	if err != nil {
		return err
	}
	defer r.Close()
	tx, err := b.db.BeginTx(b.ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ins *inserter
	if table != "" {
		ins = newInserter(b.ctx, tx, table, cols)
	}
	width := len(headers[file])
	for n := 0; ; n++ {
		if n&0xffff == 0 {
			if err := b.ctx.Err(); err != nil {
				return err
			}
		}
		f, err := r.row(width)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := fn(ins, tx, f); err != nil {
			return fmt.Errorf("%s line %d: %w", file, r.line, err)
		}
	}
	if ins != nil {
		if err := ins.flush(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// inserter batches rows into multi-row INSERTs, which is much faster than
// one statement per row with the pure-Go driver.
type inserter struct {
	ctx   context.Context
	tx    *sql.Tx
	table string
	cols  int
	rows  int // rows per full batch
	full  *sql.Stmt
	args  []any
	err   error
}

func newInserter(ctx context.Context, tx *sql.Tx, table string, cols int) *inserter {
	return &inserter{ctx: ctx, tx: tx, table: table, cols: cols, rows: 4000 / cols}
}

func (in *inserter) query(n int) string {
	row := "(" + strings.Repeat("?,", in.cols-1) + "?)"
	return "INSERT INTO " + in.table + " VALUES " + strings.Repeat(row+",", n-1) + row
}

func (in *inserter) add(vals ...any) error {
	if in.err != nil {
		return in.err
	}
	in.args = append(in.args, vals...)
	if len(in.args) < in.rows*in.cols {
		return nil
	}
	if in.full == nil {
		if in.full, in.err = in.tx.PrepareContext(in.ctx, in.query(in.rows)); in.err != nil {
			return in.err
		}
	}
	_, in.err = in.full.ExecContext(in.ctx, in.args...)
	in.args = in.args[:0]
	return in.err
}

func (in *inserter) flush() error {
	if in.err != nil || len(in.args) == 0 {
		return in.err
	}
	_, in.err = in.tx.ExecContext(in.ctx, in.query(len(in.args)/in.cols), in.args...)
	in.args = in.args[:0]
	return in.err
}
