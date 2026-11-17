package imdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is IMDb's non-commercial dataset location.
const DefaultBaseURL = "https://datasets.imdbws.com/"

type UpdaterOptions struct {
	BaseURL  string        // default DefaultBaseURL
	Interval time.Duration // between checks; default 6h
	// Settle postpones a rebuild while a file changed less than this long
	// ago, since IMDb publishes the files one by one. Default 20m.
	Settle        time.Duration
	RetryAfter    time.Duration // after a failure; default 30m
	Skip          []string      // optional dataset files not to import
	KeepDownloads bool          // keep the .tsv.gz files after a build
	HTTP          *http.Client
	Log           *slog.Logger
}

// Updater keeps the store in sync with IMDb: it checks the dataset files'
// ETags periodically and, when any changed (or no database exists yet),
// downloads them, builds a new database and swaps it in.
type Updater struct {
	store *Store
	opt   UpdaterOptions
	files []string
	kick  chan struct{}

	mu     sync.Mutex
	status UpdaterStatus
}

// UpdaterStatus is reported by /api/v1/status and the dataset_status tool.
type UpdaterStatus struct {
	State       string     `json:"state"` // idle, checking, downloading, importing
	Detail      string     `json:"detail,omitempty"`
	LastCheck   *time.Time `json:"last_check,omitempty"`
	LastChange  *time.Time `json:"last_update,omitempty"` // last successful rebuild
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	NextCheck   *time.Time `json:"next_check,omitempty"`
	Pending     bool       `json:"update_available,omitempty"`
}

func NewUpdater(store *Store, opt UpdaterOptions) (*Updater, error) {
	if opt.BaseURL == "" {
		opt.BaseURL = DefaultBaseURL
	}
	if !strings.HasSuffix(opt.BaseURL, "/") {
		opt.BaseURL += "/"
	}
	if opt.Interval <= 0 {
		opt.Interval = 6 * time.Hour
	}
	if opt.Settle < 0 {
		opt.Settle = 0
	} else if opt.Settle == 0 {
		opt.Settle = 20 * time.Minute
	}
	if opt.RetryAfter <= 0 {
		opt.RetryAfter = 30 * time.Minute
	}
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{Timeout: 2 * time.Hour}
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	u := &Updater{store: store, opt: opt, kick: make(chan struct{}, 1), status: UpdaterStatus{State: "idle"}}
	skip := map[string]bool{}
	for _, name := range opt.Skip {
		f := skipAliases[strings.TrimSpace(name)]
		if f == "" && slices.Contains(AllFiles, name) {
			f = name
		}
		switch {
		case name == "":
		case f == "":
			return nil, fmt.Errorf("unknown dataset %q in skip list (optional: akas, principals, crew, names)", name)
		case requiredFiles[f]:
			return nil, fmt.Errorf("dataset %s is required and cannot be skipped", f)
		default:
			skip[f] = true
		}
	}
	for _, f := range AllFiles {
		if !skip[f] {
			u.files = append(u.files, f)
		}
	}
	return u, nil
}

var skipAliases = map[string]string{"akas": FileAkas, "principals": FilePrincipals, "crew": FileCrew, "names": FileNames}

// Files are the dataset files this updater imports.
func (u *Updater) Files() []string { return slices.Clone(u.files) }

func (u *Updater) Status() UpdaterStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

func (u *Updater) set(fn func(s *UpdaterStatus)) {
	u.mu.Lock()
	fn(&u.status)
	u.mu.Unlock()
}

// Trigger asks for a check now; false if one is already queued or running.
func (u *Updater) Trigger() bool {
	select {
	case u.kick <- struct{}{}:
		return true
	default:
		return false
	}
}

// Run checks immediately, then on every interval (with ±5% jitter), until
// ctx is done.
func (u *Updater) Run(ctx context.Context) {
	wait := time.Duration(0)
	for {
		next := time.Now().Add(wait)
		u.set(func(s *UpdaterStatus) { s.NextCheck = &next })
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-u.kick:
		}
		u.set(func(s *UpdaterStatus) { s.NextCheck = nil })
		wait = u.cycle(ctx)
	}
}

// cycle runs one check and returns how long to wait before the next.
func (u *Updater) cycle(ctx context.Context) time.Duration {
	retry, err := u.Check(ctx)
	if ctx.Err() != nil {
		return 0
	}
	if err != nil {
		now := time.Now()
		u.opt.Log.Error("dataset update failed", "err", err)
		u.set(func(s *UpdaterStatus) { s.LastError, s.LastErrorAt = err.Error(), &now })
		return u.opt.RetryAfter
	}
	if retry > 0 {
		return retry
	}
	jitter := time.Duration((rand.Float64()*0.1 - 0.05) * float64(u.opt.Interval))
	return u.opt.Interval + jitter
}

// Check compares the remote files with the current database and rebuilds
// when they differ. A positive duration asks to check again that soon
// (files still settling).
func (u *Updater) Check(ctx context.Context) (time.Duration, error) {
	defer u.set(func(s *UpdaterStatus) { s.State, s.Detail = "idle", "" })
	u.set(func(s *UpdaterStatus) { s.State, s.Detail = "checking", "" })
	remote := map[string]Version{}
	for _, f := range u.files {
		v, err := u.head(ctx, f)
		if err != nil {
			return 0, err
		}
		remote[f] = v
	}
	now := time.Now()
	u.set(func(s *UpdaterStatus) { s.LastCheck = &now })

	info := u.store.Info()
	if info != nil && u.upToDate(info, remote) {
		u.set(func(s *UpdaterStatus) { s.Pending = false })
		u.opt.Log.Info("dataset up to date", "built_at", info.BuiltAt)
		return 0, nil
	}
	u.set(func(s *UpdaterStatus) { s.Pending = true })
	if info != nil { // keep serving the old data while IMDb is mid-publish
		for f, v := range remote {
			if age := time.Since(v.LastModified); age < u.opt.Settle {
				u.opt.Log.Info("dataset changing, waiting for it to settle", "file", f, "modified", v.LastModified)
				return u.opt.Settle - age + time.Minute, nil
			}
		}
	}
	u.opt.Log.Info("dataset changed, updating", "first_build", info == nil)
	if err := u.update(ctx, remote); err != nil {
		return 0, err
	}
	done := time.Now()
	u.set(func(s *UpdaterStatus) { s.Pending, s.LastChange, s.LastError, s.LastErrorAt = false, &done, "", nil })
	return 0, nil
}

func (u *Updater) upToDate(info *Info, remote map[string]Version) bool {
	if len(info.Versions) != len(remote) {
		return false // skip list changed
	}
	for f, v := range remote {
		have, ok := info.Versions[f]
		if !ok || !have.same(v) {
			return false
		}
	}
	return true
}

func (u *Updater) url(file string) string { return u.opt.BaseURL + file + ".tsv.gz" }

func versionOf(h http.Header, size int64) Version {
	v := Version{ETag: h.Get("ETag"), Size: size}
	if t, err := http.ParseTime(h.Get("Last-Modified")); err == nil {
		v.LastModified = t.UTC()
	}
	return v
}

func (u *Updater) head(ctx context.Context, file string) (Version, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.url(file), nil)
	if err != nil {
		return Version{}, err
	}
	resp, err := u.opt.HTTP.Do(req)
	if err != nil {
		return Version{}, fmt.Errorf("checking %s: %w", file, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Version{}, fmt.Errorf("checking %s: HTTP %d", file, resp.StatusCode)
	}
	return versionOf(resp.Header, resp.ContentLength), nil
}

// update downloads changed files (reusing finished downloads with the same
// version, e.g. after a failed import), builds a database and installs it.
func (u *Updater) update(ctx context.Context, remote map[string]Version) error {
	dl := filepath.Join(u.store.dir, "download")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		return err
	}
	paths := map[string]string{}
	versions := map[string]Version{}
	for i, f := range u.files {
		path := filepath.Join(dl, f+".tsv.gz")
		if v, ok := readVersion(path); ok && v.same(remote[f]) {
			paths[f], versions[f] = path, v
			continue
		}
		u.set(func(s *UpdaterStatus) {
			s.State, s.Detail = "downloading", fmt.Sprintf("%s (%d/%d)", f, i+1, len(u.files))
		})
		v, err := u.download(ctx, f, path)
		if err != nil {
			return err
		}
		paths[f], versions[f] = path, v
	}
	u.set(func(s *UpdaterStatus) { s.State, s.Detail = "importing", "" })
	out := filepath.Join(u.store.dir, dbName+".new")
	start := time.Now()
	info, err := build(ctx, out, paths, versions, u.opt.Log)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if err := u.store.install(out); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	u.opt.Log.Info("dataset updated", "seconds", int(time.Since(start).Seconds()), "counts", info.Counts)
	if !u.opt.KeepDownloads {
		for _, p := range paths {
			os.Remove(p)
			os.Remove(p + ".json")
		}
	}
	return nil
}

func readVersion(path string) (Version, bool) {
	raw, err := os.ReadFile(path + ".json")
	if err != nil {
		return Version{}, false
	}
	var v Version
	if json.Unmarshal(raw, &v) != nil {
		return Version{}, false
	}
	if st, err := os.Stat(path); err != nil || (v.Size > 0 && st.Size() != v.Size) {
		return Version{}, false
	}
	return v, true
}

func (u *Updater) download(ctx context.Context, file, path string) (Version, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url(file), nil)
	if err != nil {
		return Version{}, err
	}
	resp, err := u.opt.HTTP.Do(req)
	if err != nil {
		return Version{}, fmt.Errorf("downloading %s: %w", file, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Version{}, fmt.Errorf("downloading %s: HTTP %d", file, resp.StatusCode)
	}
	v := versionOf(resp.Header, resp.ContentLength)
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return Version{}, err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && v.Size > 0 && n != v.Size {
		err = fmt.Errorf("got %d of %d bytes", n, v.Size)
	}
	if err != nil {
		os.Remove(tmp)
		return Version{}, fmt.Errorf("downloading %s: %w", file, err)
	}
	v.Size = n
	if err := os.Rename(tmp, path); err != nil {
		return Version{}, err
	}
	raw, _ := json.Marshal(v)
	if err := os.WriteFile(path+".json", raw, 0o644); err != nil {
		return Version{}, err
	}
	u.opt.Log.Info("downloaded", "file", file, "bytes", n, "seconds", int(time.Since(start).Seconds()))
	return v, nil
}
