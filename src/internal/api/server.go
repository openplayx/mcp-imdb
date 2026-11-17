// Package api serves the HTTP API and the MCP endpoint.
//
//	GET  /api/healthz                      liveness + whether data is loaded (no auth)
//	GET  /api/v1/status                    dataset versions, counts, updater state
//	POST /api/v1/refresh                   check IMDb for new files now
//	GET  /api/v1/titles/search             ?q=&year=&type=&limit=&adult=
//	GET  /api/v1/titles/{id}               details (?max_akas=)
//	GET  /api/v1/titles/{id}/episodes      ?season=&start=&limit=
//	GET  /api/v1/people/search             ?q=&limit=
//	GET  /api/v1/people/{id}               details + credits (?limit=)
//	POST /api/mcp                          MCP Streamable HTTP (JSON responses only)
//
// When tokens are configured every route except healthz needs
// "Authorization: Bearer <token>".
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/openplayx/mcp-imdb/src/internal/imdb"
	"github.com/openplayx/mcp-imdb/src/internal/mcp"
)

const maxMCPBody = 64 << 10

type Server struct {
	store   *imdb.Store
	updater *imdb.Updater // nil: no refresh / updater state
	mcp     *mcp.Server
	tokens  [][32]byte
	log     *slog.Logger
	version string
	mux     *http.ServeMux
}

// Status is the body of /api/v1/status and the dataset_status tool.
type Status struct {
	Ready   bool                `json:"ready"`
	Version string              `json:"version"`
	Data    *imdb.Info          `json:"data"`
	Files   []string            `json:"files"`
	Updater *imdb.UpdaterStatus `json:"updater,omitempty"`
	Source  string              `json:"source"`
}

// StatusFunc builds the status report.
func StatusFunc(store *imdb.Store, u *imdb.Updater, version string) func() any {
	return func() any {
		st := Status{Version: version, Data: store.Info(), Source: "https://developer.imdb.com/non-commercial-datasets/"}
		st.Ready = st.Data != nil
		if u != nil {
			us := u.Status()
			st.Updater, st.Files = &us, u.Files()
		}
		return st
	}
}

// New builds the handler. Empty tokens disable authentication.
func New(store *imdb.Store, updater *imdb.Updater, tokens []string, log *slog.Logger, version string) *Server {
	s := &Server{store: store, updater: updater, log: log, version: version, mux: http.NewServeMux()}
	s.mcp = mcp.New(store, StatusFunc(store, updater, version), log, version)
	for _, t := range tokens {
		if t = strings.TrimSpace(t); t != "" {
			s.tokens = append(s.tokens, sha256.Sum256([]byte(t)))
		}
	}
	s.mux.HandleFunc("GET /api/healthz", s.healthz)
	s.mux.HandleFunc("GET /api/v1/status", s.auth(s.status))
	s.mux.HandleFunc("POST /api/v1/refresh", s.auth(s.refresh))
	s.mux.HandleFunc("GET /api/v1/titles/search", s.auth(s.searchTitles))
	s.mux.HandleFunc("GET /api/v1/titles/{id}", s.auth(s.title))
	s.mux.HandleFunc("GET /api/v1/titles/{id}/episodes", s.auth(s.episodes))
	s.mux.HandleFunc("GET /api/v1/people/search", s.auth(s.searchPeople))
	s.mux.HandleFunc("GET /api/v1/people/{id}", s.auth(s.person))
	s.mux.HandleFunc("POST /api/mcp", s.auth(s.mcpHTTP))
	s.mux.HandleFunc("GET /api/mcp", func(w http.ResponseWriter, r *http.Request) {
		// No server-initiated stream (spec: 405 when SSE is not offered).
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &imdb.Error{Status: http.StatusNotFound, Code: imdb.CodeNotFound, Message: "no such route"})
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(sw, r)
	// Paths only: queries may carry search terms.
	s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.tokens) > 0 {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			sum := sha256.Sum256([]byte(token))
			if !ok || token == "" || !slices.ContainsFunc(s.tokens, func(t [32]byte) bool {
				return subtle.ConstantTimeCompare(t[:], sum[:]) == 1
			}) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="mcp-imdb"`)
				writeError(w, &imdb.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "missing or invalid bearer token"})
				return
			}
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var e *imdb.Error
	if !errors.As(err, &e) {
		e = &imdb.Error{Status: http.StatusInternalServerError, Code: "internal_error", Message: "internal error", Retryable: true}
	}
	if e.Retryable && e.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "300")
	}
	writeJSON(w, e.Status, map[string]any{"error": e})
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		var e *imdb.Error
		if !errors.As(err, &e) {
			s.log.Error("request failed", "path", r.URL.Path, "err", err)
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// intParam reads an optional integer query parameter.
func intParam(r *http.Request, name string, def int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, &imdb.Error{Status: http.StatusBadRequest, Code: imdb.CodeInvalidRequest, Message: "invalid " + name,
			Detail: map[string]any{"field": name}}
	}
	return n, nil
}

// ints reads several integer parameters, stopping at the first error.
func ints(w http.ResponseWriter, r *http.Request, names []string, defs []int, out ...*int) bool {
	for i, n := range names {
		v, err := intParam(r, n, defs[i])
		if err != nil {
			writeError(w, err)
			return false
		}
		*out[i] = v
	}
	return true
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	info := s.store.Info()
	body := map[string]any{"status": "ok", "version": s.version, "ready": info != nil}
	if info != nil {
		body["built_at"] = info.BuiltAt
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, StatusFunc(s.store, s.updater, s.version)())
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	if s.updater == nil {
		writeError(w, &imdb.Error{Status: http.StatusConflict, Code: imdb.CodeBusy, Message: "updater disabled"})
		return
	}
	queued := s.updater.Trigger()
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued, "updater": s.updater.Status()})
}

func (s *Server) searchTitles(w http.ResponseWriter, r *http.Request) {
	var year, limit int
	if !ints(w, r, []string{"year", "limit"}, []int{0, 10}, &year, &limit) {
		return
	}
	q := r.URL.Query()
	adult, _ := strconv.ParseBool(q.Get("adult"))
	v, err := s.store.SearchTitles(r.Context(), imdb.SearchParams{
		Query: strings.TrimSpace(q.Get("q")), Year: year, Type: q.Get("type"), Limit: limit, IncludeAdult: adult})
	s.reply(w, r, v, err)
}

func (s *Server) title(w http.ResponseWriter, r *http.Request) {
	var maxAkas int
	if !ints(w, r, []string{"max_akas"}, []int{100}, &maxAkas) {
		return
	}
	v, err := s.store.GetTitle(r.Context(), r.PathValue("id"), maxAkas)
	s.reply(w, r, v, err)
}

func (s *Server) episodes(w http.ResponseWriter, r *http.Request) {
	var start, limit int
	if !ints(w, r, []string{"start", "limit"}, []int{0, 200}, &start, &limit) {
		return
	}
	var season *int
	if r.URL.Query().Get("season") != "" {
		var n int
		if !ints(w, r, []string{"season"}, []int{0}, &n) {
			return
		}
		season = &n
	}
	v, err := s.store.Episodes(r.Context(), r.PathValue("id"), season, start, limit)
	s.reply(w, r, v, err)
}

func (s *Server) searchPeople(w http.ResponseWriter, r *http.Request) {
	var limit int
	if !ints(w, r, []string{"limit"}, []int{10}, &limit) {
		return
	}
	v, err := s.store.SearchPeople(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), limit)
	s.reply(w, r, v, err)
}

func (s *Server) person(w http.ResponseWriter, r *http.Request) {
	var limit int
	if !ints(w, r, []string{"limit"}, []int{50}, &limit) {
		return
	}
	v, err := s.store.GetPerson(r.Context(), r.PathValue("id"), limit)
	s.reply(w, r, v, err)
}

// originAllowed is the DNS-rebinding guard for /api/mcp: browsers may only
// call from our own origin; native clients send no Origin.
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func (s *Server) mcpHTTP(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r) {
		writeError(w, &imdb.Error{Status: http.StatusForbidden, Code: "origin_not_allowed", Message: "foreign Origin"})
		return
	}
	if a := r.Header.Get("Accept"); a != "" && !strings.Contains(a, "application/json") && !strings.Contains(a, "*/*") {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(mcp.Versions, v) {
		writeJSON(w, http.StatusBadRequest, mcp.Reply(nil, nil, mcp.RPCError(mcp.InvalidRequest, "Unsupported protocol version.")))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, mcp.Reply(nil, nil, mcp.RPCError(mcp.InvalidRequest, "Message too large.")))
		return
	}
	reply, status := s.mcp.Handle(r.Context(), raw)
	if reply == nil {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, reply)
}
