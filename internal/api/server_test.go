package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openplayx/mcp-imdb/internal/imdb"
)

func newServer(t *testing.T, tokens ...string) *httptest.Server {
	t.Helper()
	store, err := imdb.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	u, err := imdb.NewUpdater(store, imdb.UpdaterOptions{BaseURL: "http://127.0.0.1:1/"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(store, u, tokens, slog.New(slog.NewTextHandler(io.Discard, nil)), "test"))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, token, body string, hdr ...string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func TestAuthAndNotReady(t *testing.T) {
	srv := newServer(t, "s3cret")
	if code, v := do(t, "GET", srv.URL+"/api/healthz", "", ""); code != 200 || v["ready"] != false {
		t.Fatalf("healthz %d %v", code, v)
	}
	if code, _ := do(t, "GET", srv.URL+"/api/v1/titles/search?q=x", "", ""); code != 401 {
		t.Fatalf("want 401, got %d", code)
	}
	if code, _ := do(t, "GET", srv.URL+"/api/v1/titles/search?q=x", "wrong", ""); code != 401 {
		t.Fatalf("want 401, got %d", code)
	}
	code, v := do(t, "GET", srv.URL+"/api/v1/titles/search?q=x", "s3cret", "")
	if code != 503 || v["error"].(map[string]any)["code"] != "not_ready" {
		t.Fatalf("want not_ready, got %d %v", code, v)
	}
	if code, v := do(t, "GET", srv.URL+"/api/v1/titles/search?q=x&limit=abc", "s3cret", ""); code != 400 {
		t.Fatalf("bad limit: %d %v", code, v)
	}
	code, v = do(t, "GET", srv.URL+"/api/v1/status", "s3cret", "")
	if code != 200 || v["ready"] != false || len(v["files"].([]any)) != 7 {
		t.Fatalf("status %d %v", code, v)
	}
	if code, v := do(t, "POST", srv.URL+"/api/v1/refresh", "s3cret", ""); code != 202 || v["queued"] != true {
		t.Fatalf("refresh %d %v", code, v)
	}
}

func TestMCP(t *testing.T) {
	srv := newServer(t)
	url := srv.URL + "/api/mcp"
	code, v := do(t, "POST", url, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if code != 200 || v["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize %d %v", code, v)
	}
	if code, _ := do(t, "POST", url, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Fatalf("notification: %d", code)
	}
	_, v = do(t, "POST", url, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	names := []string{}
	for _, tl := range v["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "search_titles,get_title,get_episodes,search_people,get_person,dataset_status" {
		t.Fatalf("tools %v", names)
	}
	_, v = do(t, "POST", url, "", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_titles","arguments":{"query":"x"}}}`)
	res := v["result"].(map[string]any)
	if res["isError"] != true || res["structuredContent"].(map[string]any)["error"].(map[string]any)["code"] != "not_ready" {
		t.Fatalf("call %v", v)
	}
	_, v = do(t, "POST", url, "", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_titles","arguments":{"qury":"x"}}}`)
	if v["result"].(map[string]any)["isError"] != true {
		t.Fatalf("unknown argument accepted: %v", v)
	}
	_, v = do(t, "POST", url, "", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"dataset_status"}}`)
	if v["result"].(map[string]any)["isError"] != false {
		t.Fatalf("status tool %v", v)
	}
	if code, _ := do(t, "POST", url, "", `{}`, "Origin", "http://evil.example"); code != 403 {
		t.Fatalf("foreign origin: %d", code)
	}
	if code, _ := do(t, "GET", url, "", ""); code != 405 {
		t.Fatalf("GET: %d", code)
	}
}
