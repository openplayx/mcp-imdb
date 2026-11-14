package imdb

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fixture datasets in IMDb's format.
var fixtures = map[string]string{
	FileBasics: `tconst	titleType	primaryTitle	originalTitle	isAdult	startYear	endYear	runtimeMinutes	genres
tt0111161	movie	The Shawshank Redemption	The Shawshank Redemption	0	1994	\N	142	Drama
tt0106332	movie	Farewell My Concubine	Ba wang bie ji	0	1993	\N	171	Drama,Music,Romance
tt0903747	tvSeries	Breaking Bad	Breaking Bad	0	2008	2013	45	Crime,Drama,Thriller
tt0959621	tvEpisode	Pilot	Pilot	0	2008	\N	58	Crime,Drama,Thriller
tt1054724	tvEpisode	Cat's in the Bag...	Cat's in the Bag...	0	2008	\N	48	Crime,Drama,Thriller
tt1232244	tvEpisode	Seven Thirty-Seven	Seven Thirty-Seven	0	2009	\N	47	Crime,Drama,Thriller
tt0000001	movie	Hamlet	Hamlet	0	1948	\N	154	Drama
tt0000002	movie	Hamlet	Hamlet	0	1996	\N	242	Drama
tt0000003	movie	Amélie	Le fabuleux destin d'Amélie Poulain	0	2001	\N	122	Comedy,Romance
tt0000004	movie	Secret Thing	Secret Thing	1	2001	\N	90	Adult
tt0000005	short	Shawshank Short	\N	0	2010	\N	5	\N
`,
	FileRatings: `tconst	averageRating	numVotes
tt0111161	9.3	3000000
tt0106332	8.1	90000
tt0903747	9.5	2200000
tt0959621	9.0	40000
tt0000001	7.7	20000
tt0000002	7.7	40000
tt0000003	8.3	800000
`,
	FileEpisode: `tconst	parentTconst	seasonNumber	episodeNumber
tt0959621	tt0903747	1	1
tt1054724	tt0903747	1	2
tt1232244	tt0903747	2	1
`,
	FileCrew: `tconst	directors	writers
tt0111161	nm0001104	nm0000175,nm0001104
tt0106332	nm0154164	\N
tt0959621	nm0319213	nm0319213
`,
	FileAkas: `titleId	ordering	title	region	language	types	attributes	isOriginalTitle
tt0111161	1	The Shawshank Redemption	\N	\N	original	\N	1
tt0111161	2	肖申克的救赎	CN	zh	imdbDisplay	\N	0
tt0111161	3	刺激1995	TW	\N	imdbDisplay	\N	0
tt0111161	4	The Shawshank Redemption	US	\N	imdbDisplay	\N	0
tt0106332	1	霸王别姬	CN	\N	imdbDisplay	\N	0
tt0106332	2	Ba wang bie ji	\N	\N	original	\N	1
tt0903747	1	绝命毒师	CN	\N	\N	\N	0
tt0959621	1	试播集	CN	\N	\N	\N	0
`,
	FilePrincipals: `tconst	ordering	nconst	category	job	characters
tt0111161	1	nm0000209	actor	\N	["Andy Dufresne"]
tt0111161	2	nm0000151	actor	\N	["Ellis Boyd 'Red' Redding"]
tt0111161	3	nm0001104	director	\N	\N
tt0903747	1	nm0186505	actor	\N	["Walter White"]
tt0959621	1	nm0186505	actor	\N	["Walter White"]
`,
	FileNames: `nconst	primaryName	birthYear	deathYear	primaryProfession	knownForTitles
nm0000209	Tim Robbins	1958	\N	actor,director,producer	tt0111161
nm0000151	Morgan Freeman	1937	\N	actor,producer	tt0111161
nm0001104	Frank Darabont	1959	\N	director,writer,producer	tt0111161
nm0000175	Stephen King	1947	\N	writer	tt0111161
nm0186505	Bryan Cranston	1956	\N	actor,producer	tt0903747
nm0154164	Chen Kaige	1952	\N	director	tt0106332
nm9999999	Tim Robbinson	1970	\N	actor	\N
`,
}

type fakeIMDb struct {
	mu       sync.Mutex
	files    map[string][]byte
	etags    map[string]string
	modified time.Time
	gets     int
}

func newFake(t *testing.T) (*fakeIMDb, *httptest.Server) {
	f := &fakeIMDb{files: map[string][]byte{}, etags: map[string]string{}, modified: time.Now().Add(-2 * time.Hour)}
	for name, body := range fixtures {
		f.put(name, body)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeIMDb) put(name, body string) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	io.WriteString(zw, body)
	zw.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = buf.Bytes()
	f.etags[name] = `"` + name + time.Now().Format(time.RFC3339Nano) + `"`
}

func (f *fakeIMDb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".tsv.gz")
	body, ok := f.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		f.gets++
	}
	w.Header().Set("ETag", f.etags[name])
	w.Header().Set("Last-Modified", f.modified.UTC().Format(http.TimeFormat))
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

func setup(t *testing.T) (*Store, *Updater, *fakeIMDb) {
	t.Helper()
	fake, srv := newFake(t)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	u, err := NewUpdater(store, UpdaterOptions{BaseURL: srv.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return store, u, fake
}

func TestNotReady(t *testing.T) {
	store, _, _ := setup(t)
	_, err := store.SearchTitles(context.Background(), SearchParams{Query: "x"})
	if e, ok := err.(*Error); !ok || e.Code != CodeNotReady {
		t.Fatalf("want not_ready, got %v", err)
	}
}

func TestBuildAndQuery(t *testing.T) {
	store, u, _ := setup(t)
	ctx := context.Background()
	if _, err := u.Check(ctx); err != nil {
		t.Fatal(err)
	}
	info := store.Info()
	if info == nil || info.Counts["titles"] != 11 || info.Counts["episodes"] != 3 || info.Counts["names"] != 7 {
		t.Fatalf("info %+v", info)
	}

	search := func(p SearchParams) []SearchHit {
		t.Helper()
		r, err := store.SearchTitles(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return r.Items
	}
	if h := search(SearchParams{Query: "shawshank redemption"}); len(h) == 0 || h[0].ID != "tt0111161" || !h[0].Matched.Exact {
		t.Fatalf("shawshank: %+v", h)
	}
	if h := search(SearchParams{Query: "shawshank"}); len(h) != 2 || h[0].ID != "tt0111161" || h[1].ID != "tt0000005" {
		t.Fatalf("shawshank word match: %+v", h)
	}
	if h := search(SearchParams{Query: "shawshank", Type: "short"}); len(h) != 1 || h[0].ID != "tt0000005" {
		t.Fatalf("type filter: %+v", h)
	}
	if h := search(SearchParams{Query: "肖申克的救赎"}); len(h) == 0 || h[0].ID != "tt0111161" || h[0].Matched.Region != "CN" {
		t.Fatalf("chinese: %+v", h)
	}
	if h := search(SearchParams{Query: "肖申克"}); len(h) == 0 || h[0].ID != "tt0111161" {
		t.Fatalf("chinese substring: %+v", h)
	}
	if h := search(SearchParams{Query: "Hamlet", Year: 1948}); len(h) != 2 || h[0].ID != "tt0000001" {
		t.Fatalf("year ranking: %+v", h)
	}
	if h := search(SearchParams{Query: "Hamlet"}); h[0].ID != "tt0000002" {
		t.Fatalf("votes ranking: %+v", h)
	}
	if h := search(SearchParams{Query: "amelie"}); len(h) == 0 || h[0].ID != "tt0000003" {
		t.Fatalf("diacritics: %+v", h)
	}
	if h := search(SearchParams{Query: "fabuleux destin"}); len(h) == 0 || h[0].ID != "tt0000003" {
		t.Fatalf("original title words: %+v", h)
	}
	if h := search(SearchParams{Query: "secret thing"}); len(h) != 0 {
		t.Fatalf("adult hidden: %+v", h)
	}
	if h := search(SearchParams{Query: "secret thing", IncludeAdult: true}); len(h) != 1 {
		t.Fatalf("adult shown: %+v", h)
	}
	if h := search(SearchParams{Query: "pilot"}); len(h) != 0 {
		t.Fatalf("episodes must not be searchable: %+v", h)
	}
	if h := search(SearchParams{Query: "tt0959621"}); len(h) != 1 || h[0].Type != "tvEpisode" {
		t.Fatalf("id lookup: %+v", h)
	}
	if _, err := store.SearchTitles(ctx, SearchParams{Query: "x", Type: "nope"}); err == nil {
		t.Fatal("want invalid type")
	}

	d, err := store.GetTitle(ctx, "tt0111161", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Rating == nil || d.Rating.Value != 9.3 || *d.Year != 1994 || *d.RuntimeMinutes != 142 || d.OriginalTitle != "" {
		t.Fatalf("title %+v", d.Title)
	}
	if len(d.Directors) != 1 || d.Directors[0].Name != "Frank Darabont" || len(d.Writers) != 2 || d.Writers[0].Name != "Stephen King" {
		t.Fatalf("crew %+v %+v", d.Directors, d.Writers)
	}
	if len(d.Principals) != 3 || d.Principals[0].Name != "Tim Robbins" || d.Principals[0].Characters[0] != "Andy Dufresne" {
		t.Fatalf("principals %+v", d.Principals)
	}
	if len(d.Akas) != 3 || d.Akas[0].Kind != "primary" { // primary + 2 Chinese; duplicates folded
		t.Fatalf("akas %+v", d.Akas)
	}
	if d, err := store.GetTitle(ctx, "tt0106332", 0); err != nil || d.OriginalTitle != "Ba wang bie ji" || d.Akas[1].Kind != "original" {
		t.Fatalf("original title: %+v %v", d, err)
	}

	series, err := store.GetTitle(ctx, "tt0903747", 0)
	if err != nil || series.TotalEpisodes != 3 || len(series.Seasons) != 2 || series.Seasons[0].Episodes != 2 {
		t.Fatalf("series %+v %v", series, err)
	}
	ep, err := store.GetTitle(ctx, "tt1054724", 0)
	if err != nil || ep.EpisodeOf == nil || ep.EpisodeOf.Series.ID != "tt0903747" || *ep.EpisodeOf.Episode != 2 {
		t.Fatalf("episode %+v %v", ep, err)
	}
	if _, err := store.GetTitle(ctx, "tt9999999", 0); err == nil || err.(*Error).Code != CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}

	eps, err := store.Episodes(ctx, "tt0903747", nil, 0, 0)
	if err != nil || eps.Total != 3 || eps.Items[0].ID != "tt0959621" || eps.Items[2].ID != "tt1232244" {
		t.Fatalf("episodes %+v %v", eps, err)
	}
	two := 2
	if eps, err := store.Episodes(ctx, "tt0903747", &two, 0, 0); err != nil || eps.Total != 1 {
		t.Fatalf("season 2 %+v %v", eps, err)
	}

	people, err := store.SearchPeople(ctx, "tim robbins", 0)
	if err != nil || len(people.Items) != 2 || people.Items[0].ID != "nm0000209" || !people.Items[0].Exact ||
		len(people.Items[0].KnownFor) != 1 {
		t.Fatalf("people %+v %v", people, err)
	}
	p, err := store.GetPerson(ctx, "nm0186505", 0)
	if err != nil || p.Name != "Bryan Cranston" || len(p.Credits) != 1 || p.Credits[0].ID != "tt0903747" {
		t.Fatalf("person %+v %v", p, err) // episode principals are not imported
	}
}

func TestUpdateCycle(t *testing.T) {
	store, u, fake := setup(t)
	ctx := context.Background()
	if _, err := u.Check(ctx); err != nil {
		t.Fatal(err)
	}
	built := store.Info().BuiltAt
	gets := fake.gets

	// Unchanged: no download, no rebuild.
	if _, err := u.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.gets != gets || !store.Info().BuiltAt.Equal(built) {
		t.Fatal("rebuilt although nothing changed")
	}

	// A new file that is still fresh upstream: wait for it to settle.
	fake.put(FileRatings, strings.Replace(fixtures[FileRatings], "tt0111161\t9.3", "tt0111161\t9.4", 1))
	fake.modified = time.Now()
	retry, err := u.Check(ctx)
	if err != nil || retry <= 0 || !u.Status().Pending {
		t.Fatalf("want settle retry, got %v %v", retry, err)
	}
	if d, _ := store.GetTitle(ctx, "tt0111161", 0); d.Rating.Value != 9.3 {
		t.Fatal("old data should still be served while settling")
	}

	// Settled: download and swap in the new database.
	fake.modified = time.Now().Add(-time.Hour)
	if retry, err := u.Check(ctx); err != nil || retry != 0 {
		t.Fatal(retry, err)
	}
	if d, _ := store.GetTitle(ctx, "tt0111161", 0); d.Rating.Value != 9.4 {
		t.Fatalf("new rating not served: %+v", d.Rating)
	}
	if st := u.Status(); st.Pending || st.LastChange == nil || st.State != "idle" {
		t.Fatalf("status %+v", st)
	}

	// Reopening the directory finds the installed database.
	s2, err := OpenStore(store.dir)
	if err != nil || s2.Info() == nil {
		t.Fatal(err)
	}
	s2.Close()
}

func TestFormatChange(t *testing.T) {
	store, u, fake := setup(t)
	fake.put(FileRatings, "tconst\trating\tvotes\ntt0111161\t9.3\t1\n")
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected columns") {
		t.Fatalf("want format error, got %v", err)
	}
	if store.Info() != nil {
		t.Fatal("failed build must not be installed")
	}
}

func TestSkip(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := NewUpdater(store, UpdaterOptions{Skip: []string{"ratings"}}); err == nil {
		t.Fatal("required file skipped")
	}
	if _, err := NewUpdater(store, UpdaterOptions{Skip: []string{"bogus"}}); err == nil {
		t.Fatal("unknown file accepted")
	}
	u, err := NewUpdater(store, UpdaterOptions{Skip: []string{"principals", "names"}})
	if err != nil || len(u.Files()) != 5 {
		t.Fatal(u.Files(), err)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Amélie": "amelie",
		"  The Lord of the Rings: The Fellowship!! ": "the lord of the rings the fellowship",
		"Fast & Furious":         "fast and furious",
		"霸王别姬":                   "霸王别姬",
		"ＡＢＣ　１２３":                "abc 123",
		"Léon: The Professional": "leon the professional",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if n, ok := ParseID("tt", "tt0111161"); !ok || n != 111161 || FormatID("tt", n) != "tt0111161" {
		t.Fatal("id round trip")
	}
	if FormatID("tt", 12345678) != "tt12345678" {
		t.Fatal("8-digit id")
	}
}
