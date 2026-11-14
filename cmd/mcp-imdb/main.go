// Command mcp-imdb serves IMDb's non-commercial datasets over HTTP and MCP
// from a local SQLite copy that it keeps up to date.
//
//	mcp-imdb [-listen :8810]   HTTP API + POST /api/mcp, with the background updater
//	mcp-imdb stdio             MCP over stdin/stdout (for desktop MCP clients)
//	mcp-imdb update            check once, download and import if changed, exit
//	mcp-imdb version
//
// Environment:
//
//	LISTEN               listen address (default :8810; -listen wins)
//	API_TOKENS           comma-separated bearer tokens; empty = no auth
//	DATA_DIR             database and download directory (default ./data)
//	IMDB_UPDATE          "false" disables the background updater (default true)
//	IMDB_CHECK_INTERVAL  time between update checks (default 6h)
//	IMDB_SETTLE          wait while files changed less than this ago (default 20m)
//	IMDB_SKIP            optional datasets not to import: akas,principals,crew,names
//	IMDB_KEEP_DOWNLOADS  "true" keeps the .tsv.gz files after import
//	IMDB_BASE_URL        dataset location (default https://datasets.imdbws.com/)
//	LOG_LEVEL            debug | info | warn | error (default info)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/openplayx/mcp-imdb/internal/api"
	"github.com/openplayx/mcp-imdb/internal/imdb"
	"github.com/openplayx/mcp-imdb/internal/mcp"
)

var version = "dev"

func main() {
	cmd := ""
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		cmd = os.Args[1]
	}
	if cmd == "version" {
		fmt.Println(version)
		return
	}
	listen := flag.String("listen", env("LISTEN", ":8810"), "listen address")
	if cmd == "" {
		flag.Parse()
	}
	log := newLogger()
	var err error
	switch cmd {
	case "":
		err = serve(*listen, log)
	case "stdio":
		err = stdio(log)
	case "update":
		err = updateOnce(log)
	default:
		err = fmt.Errorf("unknown command %q (want stdio, update or version)", cmd)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	b, err := strconv.ParseBool(env(k, strconv.FormatBool(def)))
	if err != nil {
		return def
	}
	return b
}

func envDuration(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}

// newLogger writes JSON to stderr; stdout stays clean for the stdio transport.
func newLogger() *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// open returns the store and, unless disabled, its updater.
func open(log *slog.Logger, forceUpdater bool) (*imdb.Store, *imdb.Updater, error) {
	store, err := imdb.OpenStore(env("DATA_DIR", "data"))
	if store == nil {
		return nil, nil, err
	}
	if err != nil {
		log.Warn("existing database not usable; it will be rebuilt", "err", err)
	}
	if !forceUpdater && !envBool("IMDB_UPDATE", true) {
		return store, nil, nil
	}
	interval, err := envDuration("IMDB_CHECK_INTERVAL", 6*time.Hour)
	if err != nil {
		return nil, nil, err
	}
	settle, err := envDuration("IMDB_SETTLE", 20*time.Minute)
	if err != nil {
		return nil, nil, err
	}
	if settle == 0 {
		settle = -1 // explicit 0 disables settling
	}
	var skip []string
	if s := os.Getenv("IMDB_SKIP"); s != "" {
		skip = strings.Split(s, ",")
	}
	u, err := imdb.NewUpdater(store, imdb.UpdaterOptions{
		BaseURL: os.Getenv("IMDB_BASE_URL"), Interval: interval, Settle: settle, Skip: skip,
		KeepDownloads: envBool("IMDB_KEEP_DOWNLOADS", false), Log: log,
	})
	return store, u, err
}

func serve(listen string, log *slog.Logger) error {
	store, updater, err := open(log, false)
	if err != nil {
		return err
	}
	defer store.Close()
	var tokens []string
	if t := os.Getenv("API_TOKENS"); t != "" {
		tokens = strings.Split(t, ",")
	}
	if len(tokens) == 0 {
		log.Warn("API_TOKENS is empty: API and MCP endpoints accept unauthenticated requests")
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           api.New(store, updater, tokens, log, version),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if updater != nil {
		go updater.Run(ctx)
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", listen, "version", version, "auth", len(tokens) > 0, "ready", store.Info() != nil)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func updateOnce(log *slog.Logger) error {
	store, updater, err := open(log, true)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	retry, err := updater.Check(ctx)
	if err == nil && retry > 0 {
		log.Info("files still changing upstream; run again later", "after", retry.Round(time.Minute).String())
	}
	return err
}

// stdio runs the MCP stdio transport: newline-delimited JSON-RPC messages
// on stdin, replies on stdout. The updater runs in the background too.
func stdio(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, updater, err := open(log, false)
	if err != nil {
		return err
	}
	defer store.Close()
	if updater != nil {
		go updater.Run(ctx)
	}
	srv := mcp.New(store, api.StatusFunc(store, updater, version), log, version)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<10), 1<<20)
	out := json.NewEncoder(os.Stdout)
	out.SetEscapeHTML(false)
	for in.Scan() {
		line := in.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		reply, _ := srv.Handle(ctx, line)
		if reply == nil {
			continue
		}
		if err := out.Encode(reply); err != nil {
			return err
		}
	}
	return in.Err()
}
