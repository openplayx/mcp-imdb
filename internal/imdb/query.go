package imdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Rating is IMDb's weighted average (out of 10) and its vote count.
type Rating struct {
	Value float64 `json:"value"`
	Votes int64   `json:"votes"`
}

// Title is the summary shared by search hits, episode lists and credits.
type Title struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	Title          string   `json:"title"`
	OriginalTitle  string   `json:"original_title,omitempty"`
	Year           *int     `json:"year"`
	EndYear        *int     `json:"end_year,omitempty"`
	RuntimeMinutes *int     `json:"runtime_minutes"`
	Genres         []string `json:"genres"`
	Rating         *Rating  `json:"rating"`
	Adult          bool     `json:"adult,omitempty"`
	URL            string   `json:"url"`
}

const titleCols = "t.tid, t.type, t.primary_title, t.original_title, t.adult, t.start_year, t.end_year, t.runtime, t.genres, t.rating, t.votes"

type scanner interface{ Scan(dest ...any) error }

func scanTitle(r scanner, extra ...any) (Title, int64, error) {
	var (
		tid                     int64
		t                       Title
		orig, genres            sql.NullString
		adult                   int
		year, end, runtime, vts sql.NullInt64
		rating                  sql.NullFloat64
	)
	dest := append([]any{&tid, &t.Type, &t.Title, &orig, &adult, &year, &end, &runtime, &genres, &rating, &vts}, extra...)
	if err := r.Scan(dest...); err != nil {
		return t, 0, err
	}
	t.ID = FormatID("tt", tid)
	t.OriginalTitle = orig.String
	t.Adult = adult == 1
	t.Year, t.EndYear, t.RuntimeMinutes = intPtr(year), intPtr(end), intPtr(runtime)
	t.Genres = []string{}
	if genres.Valid {
		t.Genres = strings.Split(genres.String, ",")
	}
	if rating.Valid {
		t.Rating = &Rating{Value: rating.Float64, Votes: vts.Int64}
	}
	t.URL = "https://www.imdb.com/title/" + t.ID + "/"
	return t, tid, nil
}

func intPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// ---- search ----

// TypeGroups are the accepted values of SearchParams.Type besides the raw
// IMDb title types.
var TypeGroups = map[string][]string{
	"movie":  {"movie", "tvMovie", "video"},
	"series": {"tvSeries", "tvMiniSeries"},
	"short":  {"short", "tvShort"},
}

// TitleTypes are IMDb's own title types (episodes are not searchable).
var TitleTypes = []string{"movie", "tvMovie", "video", "short", "tvShort", "tvSeries", "tvMiniSeries", "tvSpecial", "tvPilot", "videoGame"}

type SearchParams struct {
	Query        string
	Year         int    // 0 = any; otherwise favours titles from that year
	Type         string // "" / "any", a TypeGroups key or an IMDb title type
	Limit        int
	IncludeAdult bool
}

type Match struct {
	Title  string `json:"title"`
	Kind   string `json:"kind"` // primary, original or aka
	Region string `json:"region,omitempty"`
	Exact  bool   `json:"exact"`
}

type SearchHit struct {
	Title
	Matched Match   `json:"matched"`
	Score   float64 `json:"score"`
}

type SearchResult struct {
	Query string      `json:"query"`
	Year  int         `json:"year,omitempty"`
	Type  string      `json:"type,omitempty"`
	Items []SearchHit `json:"items"`
}

type candidate struct {
	quality float64
	match   Match
}

func (s *Store) SearchTitles(ctx context.Context, p SearchParams) (*SearchResult, error) {
	q := Normalize(p.Query)
	if q == "" {
		return nil, invalid("query", "query must contain letters or digits")
	}
	if utf8.RuneCountInString(p.Query) > 200 {
		return nil, invalid("query", "query is longer than 200 characters")
	}
	if p.Limit == 0 {
		p.Limit = 10
	}
	if p.Limit < 1 || p.Limit > 50 {
		return nil, invalid("limit", "limit must be 1..50")
	}
	if p.Year != 0 && (p.Year < 1870 || p.Year > 2100) {
		return nil, invalid("year", "year must be 1870..2100")
	}
	var types []string
	switch t := p.Type; {
	case t == "" || t == "any":
	case TypeGroups[t] != nil:
		types = TypeGroups[t]
	case slices.Contains(TitleTypes, t):
		types = []string{t}
	default:
		e := invalid("type", "unknown type %q", t)
		e.Detail["allowed"] = append([]string{"any", "movie", "series", "short"}, TitleTypes...)
		return nil, e
	}
	res := &SearchResult{Query: p.Query, Year: p.Year, Type: p.Type, Items: []SearchHit{}}
	err := s.read(ctx, func(db *sql.DB) error {
		if tid, ok := ParseID("tt", p.Query); ok {
			row := db.QueryRowContext(ctx, `SELECT `+titleCols+` FROM title t WHERE t.tid = ?`, tid)
			t, _, err := scanTitle(row)
			if err == nil {
				res.Items = append(res.Items, SearchHit{Title: t, Matched: Match{Title: t.Title, Kind: "id", Exact: true}, Score: 1000})
			}
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		cands := map[int64]*candidate{}
		forms := articleForms(q)
		add := func(tid int64, title, norm, kind string, region sql.NullString) {
			exact := slices.Contains(forms, norm)
			quality := 1.0
			if !exact {
				cover := float64(utf8.RuneCountInString(q)) / float64(max(1, utf8.RuneCountInString(norm)))
				quality = 0.3 + 0.6*math.Min(cover, 1)
			}
			if kind != "aka" {
				quality += 0.05
			}
			if c := cands[tid]; c == nil || quality > c.quality {
				cands[tid] = &candidate{quality, Match{Title: title, Kind: kind, Region: region.String, Exact: exact}}
			}
		}
		collect := func(query string, args ...any) error {
			rows, err := db.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var tid int64
				var title, norm, kind string
				var region sql.NullString
				if err := rows.Scan(&tid, &title, &norm, &kind, &region); err != nil {
					return err
				}
				add(tid, title, norm, kind, region)
			}
			return rows.Err()
		}
		if err := collect(`SELECT tid, title, norm, kind, region FROM aka WHERE norm IN (`+placeholders(len(forms))+`) LIMIT 3000`,
			stringArgs(forms)...); err != nil {
			return err
		}
		if match := ftsQuery(q); match != "" {
			// A query the FTS parser rejects just contributes no word matches.
			_ = collect(`SELECT a.tid, a.title, a.norm, a.kind, a.region
				FROM (SELECT rowid, rank FROM aka_fts WHERE aka_fts MATCH ? ORDER BY rank LIMIT 500) f
				JOIN aka a ON a.id = f.rowid`, match)
		}
		if isCJK(q) && len(cands) < 200 { // CJK titles are one token: also match substrings
			if err := collect(`SELECT tid, title, norm, kind, region FROM aka INDEXED BY aka_cjk
				WHERE cjk = 1 AND norm LIKE ? ESCAPE '\' LIMIT 500`, "%"+likeEscape(q)+"%"); err != nil {
				return err
			}
		}
		ids := make([]int64, 0, len(cands))
		for tid := range cands {
			ids = append(ids, tid)
		}
		return titlesByID(ctx, db, ids, func(t Title, tid int64, votes int64) {
			if t.Adult && !p.IncludeAdult || len(types) > 0 && !slices.Contains(types, t.Type) {
				return
			}
			c := cands[tid]
			score := 100*c.quality + 12*math.Log10(float64(votes)+1) + yearBonus(p.Year, t)
			res.Items = append(res.Items, SearchHit{Title: t, Matched: c.match, Score: math.Round(score*10) / 10})
		})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(res.Items, func(i, j int) bool {
		a, b := res.Items[i], res.Items[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.ID < b.ID
	})
	if len(res.Items) > p.Limit {
		res.Items = res.Items[:p.Limit]
	}
	return res, nil
}

func yearBonus(want int, t Title) float64 {
	if want == 0 || t.Year == nil {
		return 0
	}
	start, end := *t.Year, *t.Year
	if t.EndYear != nil {
		end = *t.EndYear
	}
	switch {
	case want == start:
		return 40
	case want > start && want <= end:
		return 20
	case want == start-1 || want == start+1:
		return 15
	default:
		return -40
	}
}

// articleForms are the normalized forms that count as an exact match for q:
// q itself plus q with a leading English article added or removed, so
// "shawshank redemption" matches "The Shawshank Redemption".
func articleForms(q string) []string {
	forms := []string{q}
	for _, a := range []string{"the ", "a ", "an "} {
		if rest, ok := strings.CutPrefix(q, a); ok && rest != "" {
			return append(forms, rest)
		}
	}
	for _, a := range []string{"the ", "a ", "an "} {
		forms = append(forms, a+q)
	}
	return forms
}

func stringArgs(ss []string) []any {
	args := make([]any, len(ss))
	for i, v := range ss {
		args[i] = v
	}
	return args
}

// ftsQuery turns a normalized query into an FTS5 AND of quoted tokens; the
// last token also matches as a prefix when it has at least 3 characters.
func ftsQuery(q string) string {
	words := strings.Fields(q)
	for i, w := range words {
		words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
		if i == len(words)-1 && utf8.RuneCountInString(w) >= 3 {
			words[i] += "*"
		}
	}
	return strings.Join(words, " ")
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func int64Args(ids []int64) []any {
	args := make([]any, len(ids))
	for i, v := range ids {
		args[i] = v
	}
	return args
}

// titlesByID loads title rows in chunks; fn gets each title, its tid and votes.
func titlesByID(ctx context.Context, db *sql.DB, ids []int64, fn func(Title, int64, int64)) error {
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		rows, err := db.QueryContext(ctx, `SELECT `+titleCols+` FROM title t WHERE t.tid IN (`+placeholders(len(chunk))+`)`, int64Args(chunk)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			t, tid, err := scanTitle(rows)
			if err != nil {
				rows.Close()
				return err
			}
			var votes int64
			if t.Rating != nil {
				votes = t.Rating.Votes
			}
			fn(t, tid, votes)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// ---- title details ----

type PersonRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Credit struct {
	PersonRef
	Category   string   `json:"category"`
	Job        string   `json:"job,omitempty"`
	Characters []string `json:"characters,omitempty"`
}

type Aka struct {
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	Region   string `json:"region,omitempty"`
	Language string `json:"language,omitempty"`
}

type Season struct {
	Season    *int `json:"season"` // null: episodes without a season number
	Episodes  int  `json:"episodes"`
	FirstYear *int `json:"first_year"`
	LastYear  *int `json:"last_year"`
}

type EpisodeOf struct {
	Series  Title `json:"series"`
	Season  *int  `json:"season"`
	Episode *int  `json:"episode"`
}

type TitleDetail struct {
	Title
	Directors     []PersonRef `json:"directors"`
	Writers       []PersonRef `json:"writers"`
	Principals    []Credit    `json:"principals"`
	Akas          []Aka       `json:"akas"`
	EpisodeOf     *EpisodeOf  `json:"episode_of,omitempty"`
	Seasons       []Season    `json:"seasons,omitempty"`
	TotalEpisodes int         `json:"total_episodes,omitempty"`
}

func parseTitleID(s string) (int64, error) {
	tid, ok := ParseID("tt", s)
	if !ok {
		return 0, invalid("id", "id must look like tt0111161")
	}
	return tid, nil
}

func splitIDs(s sql.NullString) []int64 {
	if !s.Valid || s.String == "" {
		return nil
	}
	var out []int64
	for _, p := range strings.Split(s.String, ",") {
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// namesByID resolves person ids; unknown ids map to "".
func namesByID(ctx context.Context, db *sql.DB, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		rows, err := db.QueryContext(ctx, `SELECT nid, name FROM name WHERE nid IN (`+placeholders(len(chunk))+`)`, int64Args(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var nid int64
			var name string
			if err := rows.Scan(&nid, &name); err != nil {
				rows.Close()
				return nil, err
			}
			out[nid] = name
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func refs(ids []int64, names map[int64]string) []PersonRef {
	out := make([]PersonRef, 0, len(ids))
	for _, n := range ids {
		out = append(out, PersonRef{ID: FormatID("nm", n), Name: names[n]})
	}
	return out
}

func (s *Store) GetTitle(ctx context.Context, idStr string, maxAkas int) (*TitleDetail, error) {
	tid, err := parseTitleID(idStr)
	if err != nil {
		return nil, err
	}
	if maxAkas <= 0 {
		maxAkas = 100
	}
	d := &TitleDetail{Directors: []PersonRef{}, Writers: []PersonRef{}, Principals: []Credit{}, Akas: []Aka{}}
	err = s.read(ctx, func(db *sql.DB) error {
		var dirs, writers sql.NullString
		t, _, err := scanTitle(db.QueryRowContext(ctx, `SELECT `+titleCols+`, t.directors, t.writers FROM title t WHERE t.tid = ?`, tid), &dirs, &writers)
		if err == sql.ErrNoRows {
			return notFound("no such title in the IMDb dataset")
		}
		if err != nil {
			return err
		}
		d.Title = t
		type pr struct {
			nid             int64
			cat             string
			job, characters sql.NullString
		}
		var prs []pr
		rows, err := db.QueryContext(ctx, `SELECT nid, category, job, characters FROM principal WHERE tid = ? ORDER BY ord`, tid)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p pr
			if err := rows.Scan(&p.nid, &p.cat, &p.job, &p.characters); err != nil {
				rows.Close()
				return err
			}
			prs = append(prs, p)
		}
		rows.Close()
		dirIDs, writerIDs := splitIDs(dirs), splitIDs(writers)
		all := slices.Concat(dirIDs, writerIDs)
		for _, p := range prs {
			all = append(all, p.nid)
		}
		names, err := namesByID(ctx, db, all)
		if err != nil {
			return err
		}
		d.Directors, d.Writers = refs(dirIDs, names), refs(writerIDs, names)
		for _, p := range prs {
			c := Credit{PersonRef: PersonRef{ID: FormatID("nm", p.nid), Name: names[p.nid]}, Category: p.cat, Job: p.job.String}
			if p.characters.Valid {
				_ = json.Unmarshal([]byte(p.characters.String), &c.Characters)
			}
			d.Principals = append(d.Principals, c)
		}
		rows, err = db.QueryContext(ctx, `SELECT title, kind, region, language FROM aka WHERE tid = ?
			ORDER BY CASE kind WHEN 'primary' THEN 0 WHEN 'original' THEN 1 ELSE 2 END, id LIMIT ?`, tid, maxAkas)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a Aka
			var region, lang sql.NullString
			if err := rows.Scan(&a.Title, &a.Kind, &region, &lang); err != nil {
				rows.Close()
				return err
			}
			a.Region, a.Language = region.String, lang.String
			d.Akas = append(d.Akas, a)
		}
		rows.Close()
		if t.Type == "tvEpisode" {
			var parent int64
			var season, ep sql.NullInt64
			err := db.QueryRowContext(ctx, `SELECT parent, season, episode FROM episode WHERE tid = ?`, tid).Scan(&parent, &season, &ep)
			if err == nil {
				series, _, err := scanTitle(db.QueryRowContext(ctx, `SELECT `+titleCols+` FROM title t WHERE t.tid = ?`, parent))
				if err == nil {
					d.EpisodeOf = &EpisodeOf{Series: series, Season: intPtr(season), Episode: intPtr(ep)}
				}
			}
			return nil
		}
		seasons, total, err := seasonsOf(ctx, db, tid)
		d.Seasons, d.TotalEpisodes = seasons, total
		return err
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

func seasonsOf(ctx context.Context, db *sql.DB, tid int64) ([]Season, int, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.season, COUNT(*), MIN(t.start_year), MAX(t.start_year)
		FROM episode e JOIN title t ON t.tid = e.tid WHERE e.parent = ?
		GROUP BY e.season ORDER BY e.season IS NULL, e.season`, tid)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Season
	total := 0
	for rows.Next() {
		var season, first, last sql.NullInt64
		var n int
		if err := rows.Scan(&season, &n, &first, &last); err != nil {
			return nil, 0, err
		}
		out = append(out, Season{Season: intPtr(season), Episodes: n, FirstYear: intPtr(first), LastYear: intPtr(last)})
		total += n
	}
	return out, total, rows.Err()
}

// ---- episodes ----

type Episode struct {
	Season  *int `json:"season"`
	Episode *int `json:"episode"`
	Title
}

type EpisodeList struct {
	Series Title     `json:"series"`
	Season *int      `json:"season,omitempty"`
	Total  int       `json:"total"`
	Start  int       `json:"start"`
	Items  []Episode `json:"items"`
}

// Episodes lists a series' episodes in season/episode order; season nil = all.
func (s *Store) Episodes(ctx context.Context, idStr string, season *int, start, limit int) (*EpisodeList, error) {
	tid, err := parseTitleID(idStr)
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return nil, invalid("limit", "limit must be 1..1000")
	}
	if start < 0 {
		return nil, invalid("start", "start must be ≥ 0")
	}
	out := &EpisodeList{Season: season, Start: start, Items: []Episode{}}
	err = s.read(ctx, func(db *sql.DB) error {
		series, _, err := scanTitle(db.QueryRowContext(ctx, `SELECT `+titleCols+` FROM title t WHERE t.tid = ?`, tid))
		if err == sql.ErrNoRows {
			return notFound("no such title in the IMDb dataset")
		}
		if err != nil {
			return err
		}
		out.Series = series
		where, args := `e.parent = ?`, []any{tid}
		if season != nil {
			where += ` AND e.season = ?`
			args = append(args, *season)
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM episode e WHERE `+where, args...).Scan(&out.Total); err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, `SELECT `+titleCols+`, e.season, e.episode FROM episode e JOIN title t ON t.tid = e.tid
			WHERE `+where+` ORDER BY e.season IS NULL, e.season, e.episode IS NULL, e.episode, t.start_year, t.tid
			LIMIT ? OFFSET ?`, append(args, limit, start)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var se, ep sql.NullInt64
			t, _, err := scanTitle(rows, &se, &ep)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, Episode{Season: intPtr(se), Episode: intPtr(ep), Title: t})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- people ----

type Person struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	BirthYear   *int     `json:"birth_year"`
	DeathYear   *int     `json:"death_year,omitempty"`
	Professions []string `json:"professions"`
	KnownFor    []Title  `json:"known_for"`
	URL         string   `json:"url"`
}

type PersonHit struct {
	Person
	Exact bool    `json:"exact"`
	Score float64 `json:"score"`
}

type PeopleResult struct {
	Query string      `json:"query"`
	Items []PersonHit `json:"items"`
}

type PersonCredit struct {
	Category   string   `json:"category"`
	Job        string   `json:"job,omitempty"`
	Characters []string `json:"characters,omitempty"`
	Title
}

type PersonDetail struct {
	Person
	Credits []PersonCredit `json:"credits"`
}

type personRow struct {
	nid          int64
	p            Person
	norm         string
	knownFor     []int64
	knownForVote int64
}

func loadPeople(ctx context.Context, db *sql.DB, where string, args ...any) ([]*personRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT nid, name, norm, birth, death, professions, known_for FROM name WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*personRow
	for rows.Next() {
		r := &personRow{}
		var birth, death sql.NullInt64
		var prof, known sql.NullString
		if err := rows.Scan(&r.nid, &r.p.Name, &r.norm, &birth, &death, &prof, &known); err != nil {
			return nil, err
		}
		r.p.ID = FormatID("nm", r.nid)
		r.p.URL = "https://www.imdb.com/name/" + r.p.ID + "/"
		r.p.BirthYear, r.p.DeathYear = intPtr(birth), intPtr(death)
		r.p.Professions = []string{}
		if prof.Valid && prof.String != "" {
			r.p.Professions = strings.Split(prof.String, ",")
		}
		r.knownFor = splitIDs(known)
		out = append(out, r)
	}
	return out, rows.Err()
}

// fillKnownFor resolves known-for titles and sums their votes (popularity).
func fillKnownFor(ctx context.Context, db *sql.DB, people []*personRow) error {
	var ids []int64
	for _, r := range people {
		ids = append(ids, r.knownFor...)
	}
	titles := map[int64]Title{}
	votes := map[int64]int64{}
	if err := titlesByID(ctx, db, ids, func(t Title, tid, v int64) { titles[tid], votes[tid] = t, v }); err != nil {
		return err
	}
	for _, r := range people {
		r.p.KnownFor = []Title{}
		for _, tid := range r.knownFor {
			if t, ok := titles[tid]; ok {
				r.p.KnownFor = append(r.p.KnownFor, t)
				r.knownForVote += votes[tid]
			}
		}
	}
	return nil
}

func (s *Store) SearchPeople(ctx context.Context, query string, limit int) (*PeopleResult, error) {
	q := Normalize(query)
	if q == "" {
		return nil, invalid("query", "query must contain letters or digits")
	}
	if limit == 0 {
		limit = 10
	}
	if limit < 1 || limit > 50 {
		return nil, invalid("limit", "limit must be 1..50")
	}
	res := &PeopleResult{Query: query, Items: []PersonHit{}}
	err := s.read(ctx, func(db *sql.DB) error {
		if nid, ok := ParseID("nm", query); ok {
			people, err := loadPeople(ctx, db, `nid = ?`, nid)
			if err != nil || len(people) == 0 {
				return err
			}
			if err := fillKnownFor(ctx, db, people); err != nil {
				return err
			}
			res.Items = append(res.Items, PersonHit{Person: people[0].p, Exact: true, Score: 1000})
			return nil
		}
		people, err := loadPeople(ctx, db, `norm = ? LIMIT 500`, q)
		if err != nil {
			return err
		}
		if match := ftsQuery(q); match != "" {
			more, err := loadPeople(ctx, db, `nid IN (SELECT rowid FROM name_fts WHERE name_fts MATCH ? ORDER BY rank LIMIT 300)`, match)
			if err == nil {
				people = append(people, more...)
			}
		}
		seen := map[int64]bool{}
		people = slices.DeleteFunc(people, func(r *personRow) bool {
			dup := seen[r.nid]
			seen[r.nid] = true
			return dup
		})
		if err := fillKnownFor(ctx, db, people); err != nil {
			return err
		}
		for _, r := range people {
			exact := r.norm == q
			quality := 1.0
			if !exact {
				cover := float64(utf8.RuneCountInString(q)) / float64(max(1, utf8.RuneCountInString(r.norm)))
				quality = 0.3 + 0.6*math.Min(cover, 1)
			}
			score := 100*quality + 10*math.Log10(float64(r.knownForVote)+1)
			res.Items = append(res.Items, PersonHit{Person: r.p, Exact: exact, Score: math.Round(score*10) / 10})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(res.Items, func(i, j int) bool {
		if res.Items[i].Score != res.Items[j].Score {
			return res.Items[i].Score > res.Items[j].Score
		}
		return res.Items[i].ID < res.Items[j].ID
	})
	if len(res.Items) > limit {
		res.Items = res.Items[:limit]
	}
	return res, nil
}

func (s *Store) GetPerson(ctx context.Context, idStr string, limit int) (*PersonDetail, error) {
	nid, ok := ParseID("nm", idStr)
	if !ok {
		return nil, invalid("id", "id must look like nm0000209")
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 500 {
		return nil, invalid("limit", "limit must be 1..500")
	}
	d := &PersonDetail{Credits: []PersonCredit{}}
	err := s.read(ctx, func(db *sql.DB) error {
		people, err := loadPeople(ctx, db, `nid = ?`, nid)
		if err != nil {
			return err
		}
		if len(people) == 0 {
			return notFound("no such person in the IMDb dataset")
		}
		if err := fillKnownFor(ctx, db, people); err != nil {
			return err
		}
		d.Person = people[0].p
		rows, err := db.QueryContext(ctx, `SELECT `+titleCols+`, p.category, p.job, p.characters
			FROM principal p JOIN title t ON t.tid = p.tid WHERE p.nid = ?
			ORDER BY t.votes IS NULL, t.votes DESC, t.start_year DESC LIMIT ?`, nid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c PersonCredit
			var job, chars sql.NullString
			t, _, err := scanTitle(rows, &c.Category, &job, &chars)
			if err != nil {
				return err
			}
			c.Title, c.Job = t, job.String
			if chars.Valid {
				_ = json.Unmarshal([]byte(chars.String), &c.Characters)
			}
			d.Credits = append(d.Credits, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}
