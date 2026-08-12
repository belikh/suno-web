package main

// Filtering, faceting, variant grouping, and lineage traversal over the
// in-memory index. Everything here is a linear pass over ~18.7k records, which
// is sub-millisecond — no secondary indexes beyond the reverse-lineage map.

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Query is one parsed request from the UI.
type Query struct {
	Text     []string // lowercased AND terms
	Models   []string
	Versions []string
	Projects []string
	Personas []string
	Tasks    []string
	Kinds    []string
	Tags     []string // AND — a track must carry all of them

	Liked   *bool
	Public  *bool
	Instr   *bool
	Remix   *bool
	HasLyr  *bool
	HasPers *bool

	From, To         time.Time
	DurMin, DurMax   float64
	IDs              map[string]bool // restrict to a set (playlist view)

	Sort   string
	Desc   bool
	Group  bool
	Offset int
	Limit  int
}

func multi(v url.Values, key string) []string {
	var out []string
	for _, raw := range v[key] {
		for _, part := range strings.Split(raw, "\x1f") {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func triState(v url.Values, key string) *bool {
	s := v.Get(key)
	if s == "" {
		return nil
	}
	b := s == "1" || s == "true"
	return &b
}

func ParseQuery(v url.Values) Query {
	q := Query{
		Models:   multi(v, "model"),
		Versions: multi(v, "version"),
		Projects: multi(v, "project"),
		Personas: multi(v, "persona"),
		Tasks:    multi(v, "task"),
		Kinds:    multi(v, "kind"),
		Tags:     multi(v, "tag"),
		Liked:    triState(v, "liked"),
		Public:   triState(v, "public"),
		Instr:    triState(v, "instrumental"),
		Remix:    triState(v, "remix"),
		HasLyr:   triState(v, "hasLyrics"),
		HasPers:  triState(v, "hasPersona"),
		Sort:     v.Get("sort"),
		Group:    v.Get("group") != "0",
		Limit:    100,
	}
	if s := strings.TrimSpace(strings.ToLower(v.Get("q"))); s != "" {
		q.Text = strings.Fields(s)
	}
	if t, err := time.Parse("2006-01-02", v.Get("from")); err == nil {
		q.From = t
	}
	if t, err := time.Parse("2006-01-02", v.Get("to")); err == nil {
		q.To = t.Add(24 * time.Hour)
	}
	q.DurMin, _ = strconv.ParseFloat(v.Get("durMin"), 64)
	q.DurMax, _ = strconv.ParseFloat(v.Get("durMax"), 64)
	if n, err := strconv.Atoi(v.Get("offset")); err == nil && n > 0 {
		q.Offset = n
	}
	if n, err := strconv.Atoi(v.Get("limit")); err == nil && n > 0 && n <= 500 {
		q.Limit = n
	}
	q.Desc = v.Get("order") != "asc"
	if q.Sort == "" {
		q.Sort = "created"
	}
	return q
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// anyOf reports whether val matches one of the selected values (empty
// selection matches everything).
func anyOf(sel []string, val string) bool {
	if len(sel) == 0 {
		return true
	}
	return containsFold(sel, val)
}

func (q Query) match(t *Track) bool {
	if q.IDs != nil && !q.IDs[t.ID] {
		return false
	}
	if !anyOf(q.Models, t.Model) || !anyOf(q.Versions, t.Version) ||
		!anyOf(q.Projects, t.Project) || !anyOf(q.Personas, t.Persona) ||
		!anyOf(q.Tasks, t.Task) || !anyOf(q.Kinds, t.Kind) {
		return false
	}
	for _, want := range q.Tags {
		if !containsFold(t.Tags, want) {
			return false
		}
	}
	if q.Liked != nil && t.Liked != *q.Liked {
		return false
	}
	if q.Public != nil && t.Public != *q.Public {
		return false
	}
	if q.Instr != nil && t.Instr != *q.Instr {
		return false
	}
	if q.Remix != nil && t.Remix != *q.Remix {
		return false
	}
	if q.HasLyr != nil && t.Lyrics != *q.HasLyr {
		return false
	}
	if q.HasPers != nil && (t.Persona != "") != *q.HasPers {
		return false
	}
	if !q.From.IsZero() && t.Created.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && !t.Created.Before(q.To) {
		return false
	}
	if q.DurMin > 0 && t.Duration < q.DurMin {
		return false
	}
	if q.DurMax > 0 && t.Duration > q.DurMax {
		return false
	}
	for _, term := range q.Text {
		if !strings.Contains(t.Search, term) {
			return false
		}
	}
	return true
}

// Facet is one selectable value plus how many matches carry it.
type Facet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

func topFacets(counts map[string]int, limit int) []Facet {
	out := make([]Facet, 0, len(counts))
	for v, c := range counts {
		if v == "" {
			continue
		}
		out = append(out, Facet{Value: v, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Row is a listing entry: one track, plus its sibling variants when grouping
// is on (Suno emits two clips per generation).
type Row struct {
	Track
	Variants []Track `json:"variants,omitempty"`
	// Kids is how many clips derive from this one — the lineage affordance in
	// the listing.
	Kids int `json:"kids,omitempty"`
}

// Result is the listing payload.
type Result struct {
	Rows   []Row            `json:"rows"`
	Total  int              `json:"total"`  // rows after grouping
	Tracks int              `json:"tracks"` // matching clips before grouping
	Offset int              `json:"offset"`
	Limit  int              `json:"limit"`
	Facets map[string][]Facet `json:"facets"`
	Totals map[string]any   `json:"totals"`
}

// Search filters, facets, groups, sorts and paginates in one pass.
func Search(l *Library, q Query) Result {
	all := l.All()

	matched := make([]*Track, 0, 256)
	models := map[string]int{}
	versions := map[string]int{}
	projects := map[string]int{}
	personas := map[string]int{}
	tasks := map[string]int{}
	kinds := map[string]int{}
	tags := map[string]int{}
	var totalDur float64
	var totalBytes int64
	liked := 0

	for _, t := range all {
		if !q.match(t) {
			continue
		}
		matched = append(matched, t)
		models[t.Model]++
		versions[t.Version]++
		projects[t.Project]++
		personas[t.Persona]++
		tasks[t.Task]++
		kinds[t.Kind]++
		for _, tag := range t.Tags {
			tags[tag]++
		}
		totalDur += t.Duration
		totalBytes += t.WavBytes
		if t.Liked {
			liked++
		}
	}

	rows := groupRows(l, matched, q.Group)
	sortRows(rows, q.Sort, q.Desc)

	total := len(rows)
	start := q.Offset
	if start > total {
		start = total
	}
	end := start + q.Limit
	if end > total {
		end = total
	}

	return Result{
		Rows:   rows[start:end],
		Total:  total,
		Tracks: len(matched),
		Offset: start,
		Limit:  q.Limit,
		Facets: map[string][]Facet{
			"model":   topFacets(models, 0),
			"version": topFacets(versions, 0),
			"project": topFacets(projects, 0),
			"persona": topFacets(personas, 0),
			"task":    topFacets(tasks, 0),
			"kind":    topFacets(kinds, 0),
			"tag":     topFacets(tags, 200),
		},
		Totals: map[string]any{
			"duration": totalDur,
			"bytes":    totalBytes,
			"liked":    liked,
			"personas": len(personas),
		},
	}
}

// groupRows collapses same-generation variants into one row. With grouping off
// every clip is its own row.
func groupRows(l *Library, matched []*Track, group bool) []Row {
	if !group {
		rows := make([]Row, 0, len(matched))
		for _, t := range matched {
			rows = append(rows, Row{Track: t.forWire(), Kids: len(l.Children(t.ID))})
		}
		return rows
	}
	index := make(map[string]int, len(matched))
	rows := make([]Row, 0, len(matched))
	for _, t := range matched {
		key := t.GroupKey()
		if i, ok := index[key]; ok {
			rows[i].Variants = append(rows[i].Variants, t.forWire())
			continue
		}
		index[key] = len(rows)
		rows = append(rows, Row{Track: t.forWire(), Kids: len(l.Children(t.ID))})
	}
	return rows
}

func sortRows(rows []Row, key string, desc bool) {
	less := func(i, j int) bool { return rows[i].Created.After(rows[j].Created) }
	switch key {
	case "title":
		less = func(i, j int) bool { return strings.ToLower(rows[i].Title) < strings.ToLower(rows[j].Title) }
	case "duration":
		less = func(i, j int) bool { return rows[i].Duration > rows[j].Duration }
	case "plays":
		less = func(i, j int) bool { return rows[i].Plays > rows[j].Plays }
	case "upvotes":
		less = func(i, j int) bool { return rows[i].Upvotes > rows[j].Upvotes }
	case "family":
		less = func(i, j int) bool { return rows[i].Kids > rows[j].Kids }
	case "size":
		less = func(i, j int) bool { return rows[i].WavBytes > rows[j].WavBytes }
	}
	sort.SliceStable(rows, less)
	if !desc {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
}

// --- lineage -------------------------------------------------------------

// LineageNode is one clip in a family tree. Track is nil when the clip is
// referenced by an edge but not present in the archive — which, while the
// backfill is still running, usually means "not downloaded yet" rather than
// "gone"; the API reports backfillDone so the UI can say which.
type LineageNode struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind,omitempty"` // edge kind linking to parent
	At          float64        `json:"at,omitempty"`
	Resolved    bool           `json:"resolved"`
	Track       *Track         `json:"track,omitempty"`
	Children    []*LineageNode `json:"children,omitempty"`
	Descendants int            `json:"descendants"`
	Truncated   bool           `json:"truncated,omitempty"`
}

// Lineage is the family view for one clip.
type Lineage struct {
	Focus     string         `json:"focus"`
	Root      *LineageNode   `json:"root"`
	Ancestors []*LineageNode `json:"ancestors"` // root-first chain down to focus
	Nodes     int            `json:"nodes"`
	Truncated bool           `json:"truncated"`
}

const maxFamilyNodes = 1500

// BuildLineage walks from id up to the family root via primary-parent edges,
// then expands the whole descendant tree beneath that root.
func BuildLineage(l *Library, id string) *Lineage {
	// Walk up. Cycle-guarded: the data is a DAG in practice, but a corrupt or
	// self-referential edge must not hang the request.
	seen := map[string]bool{id: true}
	chain := []string{id}
	cur := id
	for {
		t, ok := l.Get(cur)
		if !ok || t.Primary == "" || seen[t.Primary] {
			break
		}
		cur = t.Primary
		seen[cur] = true
		chain = append(chain, cur)
	}
	rootID := chain[len(chain)-1]

	budget := maxFamilyNodes
	root := expand(l, rootID, "", 0, map[string]bool{}, &budget)

	// Ancestors, root-first, each carrying the edge kind that links it to its
	// parent — the breadcrumb the UI renders above the focused clip.
	anc := make([]*LineageNode, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		n := &LineageNode{ID: chain[i]}
		if t, ok := l.Get(chain[i]); ok {
			n.Resolved = true
			w := t.forWire()
			n.Track = &w
			if i > 0 {
				// The edge kind by which chain[i-1] derives from chain[i].
				if child, ok := l.Get(chain[i-1]); ok {
					for _, e := range child.Parents {
						if e.Target == chain[i] {
							// Recorded on the child node below.
							_ = e
							break
						}
					}
				}
			}
		}
		anc = append(anc, n)
	}
	// Label each ancestor with how it derives from the one above it.
	for i := 1; i < len(anc); i++ {
		if child, ok := l.Get(anc[i].ID); ok {
			for _, e := range child.Parents {
				if e.Target == anc[i-1].ID {
					anc[i].Kind = e.Kind
					anc[i].At = e.At
					break
				}
			}
		}
	}

	return &Lineage{
		Focus:     id,
		Root:      root,
		Ancestors: anc,
		Nodes:     maxFamilyNodes - budget,
		Truncated: budget <= 0,
	}
}

// expand builds the descendant tree under id, breadth-limited by a shared node
// budget so a 426-child family can't produce an unbounded response.
func expand(l *Library, id, kind string, at float64, seen map[string]bool, budget *int) *LineageNode {
	n := &LineageNode{ID: id, Kind: kind, At: at}
	if t, ok := l.Get(id); ok {
		n.Resolved = true
		w := t.forWire()
		n.Track = &w
	}
	if seen[id] || *budget <= 0 {
		n.Truncated = true
		return n
	}
	seen[id] = true
	*budget--

	kids := l.Children(id)
	// Stable, meaningful order: by edge kind then creation time.
	sort.SliceStable(kids, func(i, j int) bool {
		if kids[i].Kind != kids[j].Kind {
			return kids[i].Kind < kids[j].Kind
		}
		a, aok := l.Get(kids[i].ID)
		b, bok := l.Get(kids[j].ID)
		if aok && bok {
			return a.Created.Before(b.Created)
		}
		return kids[i].ID < kids[j].ID
	})

	total := 0
	for _, k := range kids {
		if *budget <= 0 {
			n.Truncated = true
			break
		}
		var childAt float64
		if t, ok := l.Get(k.ID); ok {
			for _, e := range t.Parents {
				if e.Target == id && e.Kind == k.Kind {
					childAt = e.At
					break
				}
			}
		}
		c := expand(l, k.ID, k.Kind, childAt, seen, budget)
		n.Children = append(n.Children, c)
		total += 1 + c.Descendants
	}
	n.Descendants = total
	return n
}
