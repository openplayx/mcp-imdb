// Package mcp implements the Model Context Protocol tool server over the
// local IMDb database. It is transport-agnostic: Handle takes one JSON-RPC
// 2.0 message and returns the reply; internal/api serves it over Streamable
// HTTP (POST /api/mcp) and cmd/mcp-imdb over stdio.
//
// Supported subset: single messages (no batches), no server-initiated
// messages, stateless (no Mcp-Session-Id); initialize, ping, tools/list,
// tools/call and notifications. Business failures are tool results with
// isError=true carrying the same error object as the HTTP API.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/openplayx/mcp-imdb/internal/imdb"
)

// Versions are the protocol versions this server speaks, newest first.
var Versions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
)

const instructions = "Read-only lookups in a local copy of IMDb's official datasets (refreshed when IMDb " +
	"publishes new files, normally daily): titles (movies, series, shorts, specials…), ratings, " +
	"episodes, alternative titles in many regions (including Chinese titles), principal cast and crew, " +
	"and people. Use search_titles to turn a title (optionally with year and type) into an IMDb id, " +
	"then get_title / get_episodes; search_people and get_person for people. Ratings are out of 10 " +
	"and null for titles with no votes. dataset_status shows when the data was last refreshed."

// Status reports the dataset state for the dataset_status tool.
type Status func() any

// Server answers MCP messages using the store.
type Server struct {
	store   *imdb.Store
	status  Status
	log     *slog.Logger
	version string
}

func New(store *imdb.Store, status Status, log *slog.Logger, version string) *Server {
	return &Server{store: store, status: status, log: log, version: version}
}

type tool struct {
	Name, Title, Description string
	Schema                   map[string]any
	Run                      func(ctx context.Context, s *Server, args json.RawMessage) (any, error)
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func integer(desc string, def, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "default": def, "minimum": min, "maximum": max}
}

var (
	titleID  = map[string]any{"type": "string", "pattern": "^tt[0-9]{7,10}$", "description": "IMDb title id, e.g. \"tt0111161\"."}
	personID = map[string]any{"type": "string", "pattern": "^nm[0-9]{7,10}$", "description": "IMDb person id, e.g. \"nm0000209\"."}
)

// decode is strict: unknown fields are an error, so typos surface.
func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		msg := "trailing data"
		if err != nil {
			msg = err.Error()
		}
		return &imdb.Error{Status: http.StatusBadRequest, Code: imdb.CodeInvalidRequest, Message: "invalid arguments: " + msg}
	}
	return nil
}

var typeEnum = append([]string{"any", "movie", "series", "short"}, imdb.TitleTypes...)

var tools = []tool{
	{
		Name: "search_titles", Title: "Search IMDb titles",
		Description: "Finds titles by name (primary, original or any regional alternative title, e.g. Chinese) and " +
			"returns IMDb ids with type, year, runtime, genres and rating, best match first. Give the year when " +
			"known (e.g. from a file name) to rank the right release first; type narrows to movie, series, short " +
			"or an IMDb title type. A tt id as query returns that title. Episodes are not searchable: find the " +
			"series, then use get_episodes.",
		Schema: object(map[string]any{
			"query":         map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
			"year":          map[string]any{"type": "integer", "minimum": 1870, "maximum": 2100, "description": "Release year (series: any year it ran); ranks matching titles first."},
			"type":          map[string]any{"type": "string", "enum": typeEnum, "default": "any", "description": "movie = movie/tvMovie/video, series = tvSeries/tvMiniSeries, short = short/tvShort."},
			"limit":         integer("Number of results.", 10, 1, 50),
			"include_adult": map[string]any{"type": "boolean", "default": false},
		}, "query"),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Query        string `json:"query"`
				Year         int    `json:"year"`
				Type         string `json:"type"`
				Limit        int    `json:"limit"`
				IncludeAdult bool   `json:"include_adult"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			return s.store.SearchTitles(ctx, imdb.SearchParams{Query: a.Query, Year: a.Year, Type: a.Type, Limit: a.Limit, IncludeAdult: a.IncludeAdult})
		},
	},
	{
		Name: "get_title", Title: "Get IMDb title",
		Description: "Details of a title: type, titles, years, runtime, genres, rating and votes, directors, writers, " +
			"principal cast and crew with characters, alternative titles by region and language; for a series its " +
			"seasons and episode counts, for an episode its series, season and episode number.",
		Schema: object(map[string]any{
			"id":       titleID,
			"max_akas": integer("Maximum alternative titles to return.", 100, 1, 500),
		}, "id"),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				ID      string `json:"id"`
				MaxAkas int    `json:"max_akas"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			return s.store.GetTitle(ctx, a.ID, a.MaxAkas)
		},
	},
	{
		Name: "get_episodes", Title: "List IMDb episodes",
		Description: "Episodes of a series in season and episode order, each with its own IMDb id, title, year, " +
			"runtime and rating. Filter by season; page with start and limit.",
		Schema: object(map[string]any{
			"id":     titleID,
			"season": map[string]any{"type": "integer", "minimum": 0, "maximum": 10000, "description": "Season number; omit for all seasons."},
			"start":  integer("Offset.", 0, 0, 100000),
			"limit":  integer("Number of episodes.", 200, 1, 1000),
		}, "id"),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				ID     string `json:"id"`
				Season *int   `json:"season"`
				Start  int    `json:"start"`
				Limit  int    `json:"limit"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			return s.store.Episodes(ctx, a.ID, a.Season, a.Start, a.Limit)
		},
	},
	{
		Name: "search_people", Title: "Search IMDb people",
		Description: "Finds people (actors, directors, writers, crew) by name and returns IMDb ids, birth and death " +
			"years, professions and the titles they are known for, most prominent first.",
		Schema: object(map[string]any{
			"query": map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
			"limit": integer("Number of results.", 10, 1, 50),
		}, "query"),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			return s.store.SearchPeople(ctx, a.Query, a.Limit)
		},
	},
	{
		Name: "get_person", Title: "Get IMDb person",
		Description: "A person's details and principal credits (titles where they are top-billed cast or key crew), " +
			"most-voted titles first.",
		Schema: object(map[string]any{
			"id":    personID,
			"limit": integer("Maximum credits.", 50, 1, 500),
		}, "id"),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				ID    string `json:"id"`
				Limit int    `json:"limit"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			return s.store.GetPerson(ctx, a.ID, a.Limit)
		},
	},
	{
		Name: "dataset_status", Title: "IMDb dataset status",
		Description: "When the local IMDb data was built, from which file versions, row counts, and the state of " +
			"the update checker (last check, next check, last error).",
		Schema: object(map[string]any{}),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			if err := decode(raw, &struct{}{}); err != nil {
				return nil, err
			}
			return s.status(), nil
		},
	},
}

func describe(t tool) map[string]any {
	return map[string]any{
		"name": t.Name, "title": t.Title, "description": t.Description, "inputSchema": t.Schema,
		"annotations": map[string]any{
			"title": t.Title, "readOnlyHint": true, "destructiveHint": false,
			"idempotentHint": true, "openWorldHint": false,
		},
	}
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Reply builds a JSON-RPC response. A nil id becomes JSON null.
func Reply(id json.RawMessage, result any, rpcErr map[string]any) map[string]any {
	body := map[string]any{"jsonrpc": "2.0"}
	if len(id) == 0 {
		body["id"] = nil
	} else {
		body["id"] = id
	}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	return body
}

func RPCError(code int, msg string) map[string]any {
	return map[string]any{"code": code, "message": msg}
}

func toolResult(payload any, isError bool) map[string]any {
	text, _ := json.Marshal(payload)
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": payload,
		"isError":           isError,
	}
}

func (s *Server) toolError(name string, err error) map[string]any {
	var e *imdb.Error
	if !errors.As(err, &e) {
		s.log.Error("tool failed", "tool", name, "err", err)
		e = &imdb.Error{Status: http.StatusInternalServerError, Code: "internal_error", Message: "internal error", Retryable: true}
	}
	return toolResult(map[string]any{"error": e}, true)
}

// Handle answers one JSON-RPC message. It returns nil for notifications and
// client responses, which get no reply. status is the HTTP status to use.
func (s *Server) Handle(ctx context.Context, raw []byte) (reply map[string]any, status int) {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return Reply(nil, nil, RPCError(InvalidRequest, "Expected one JSON-RPC 2.0 message.")), http.StatusBadRequest
	}
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return Reply(nil, nil, RPCError(ParseError, "Parse error.")), http.StatusBadRequest
	}
	if msg.JSONRPC != "2.0" {
		return Reply(msg.ID, nil, RPCError(InvalidRequest, "Expected one JSON-RPC 2.0 message.")), http.StatusBadRequest
	}
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		return nil, http.StatusAccepted
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		version := Versions[0]
		if slices.Contains(Versions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return Reply(msg.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "mcp-imdb", "title": "IMDb datasets", "version": s.version},
			"instructions":    instructions,
		}, nil), http.StatusOK
	case "ping":
		return Reply(msg.ID, map[string]any{}, nil), http.StatusOK
	case "tools/list":
		list := make([]any, len(tools))
		for i, t := range tools {
			list[i] = describe(t)
		}
		return Reply(msg.ID, map[string]any{"tools": list}, nil), http.StatusOK
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Name == "" {
			return Reply(msg.ID, nil, RPCError(InvalidParams, "params.name is required.")), http.StatusOK
		}
		i := slices.IndexFunc(tools, func(t tool) bool { return t.Name == p.Name })
		if i < 0 {
			return Reply(msg.ID, nil, RPCError(InvalidParams, "Unknown tool.")), http.StatusOK
		}
		v, err := tools[i].Run(ctx, s, p.Arguments)
		if err != nil {
			return Reply(msg.ID, s.toolError(p.Name, err), nil), http.StatusOK
		}
		return Reply(msg.ID, toolResult(v, false), nil), http.StatusOK
	default:
		return Reply(msg.ID, nil, RPCError(MethodNotFound, "Method not found.")), http.StatusOK
	}
}
