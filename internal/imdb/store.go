package imdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store serves queries from the current database file and swaps in a newly
// built one atomically: queries hold a read lock, the swap a write lock.
type Store struct {
	dir  string
	mu   sync.RWMutex
	db   *sql.DB
	info *Info
}

const dbName = "imdb.db"

// OpenStore opens dir/imdb.db if it exists and is complete; otherwise the
// store starts empty (queries fail with not_ready until the first build).
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	path := filepath.Join(dir, dbName)
	if _, err := os.Stat(path); err == nil {
		db, info, err := openDB(path)
		if err != nil {
			return s, fmt.Errorf("ignoring %s: %w", path, err)
		}
		s.db, s.info = db, info
	}
	return s, nil
}

func openDB(path string) (*sql.DB, *Info, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=cache_size(-65536)&_pragma=mmap_size(1073741824)")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	var raw string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'info'`).Scan(&raw); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("incomplete database: %w", err)
	}
	var info Info
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		db.Close()
		return nil, nil, err
	}
	if info.Schema != schemaVersion {
		db.Close()
		return nil, nil, fmt.Errorf("schema %q, want %q", info.Schema, schemaVersion)
	}
	return db, &info, nil
}

// Info of the current database, nil before the first build.
func (s *Store) Info() *Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db, s.info = nil, nil
	return err
}

// install replaces the current database with the file at path.
func (s *Store) install(path string) error {
	final := filepath.Join(s.dir, dbName)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		s.db.Close()
		s.db, s.info = nil, nil
	}
	if err := os.Rename(path, final); err != nil {
		return err
	}
	db, info, err := openDB(final)
	if err != nil {
		return err
	}
	s.db, s.info = db, info
	return nil
}

// read runs fn with the current database under the read lock.
func (s *Store) read(ctx context.Context, fn func(db *sql.DB) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return errNotReady
	}
	err := fn(s.db)
	var e *Error
	if err != nil && !errors.As(err, &e) && ctx.Err() == nil {
		return fmt.Errorf("query: %w", err)
	}
	return err
}
