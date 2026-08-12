package main

// The library index: a compact, in-memory view of every archived clip, built
// by walking `<dataDir>/tracks/<YYYY>/<MM>/<id>/meta.json`.
//
// Why walk meta.json rather than read the daemon's index.json: meta.json is
// the complete per-clip object (lyrics, tags, persona, lineage, counts, flags)
// and is written once per clip and never rewritten, whereas index.json holds
// only backup bookkeeping and is rewritten after every clip. Everything the UI
// filters on lives in meta.json. (index.json is read for exactly one field —
// backfill_done — see BackfillDone.)
//
// Memory discipline matters here: europa has 7.8GB with ZFS ARC sized at 5GB,
// so a fat resident index would evict NAS cache. We therefore keep ONE
// lowercased search blob per clip (title+tags+description+persona+lyrics,
// ~1.5KB) plus small scalar fields, and never hold the raw meta.json — the
// full object is re-read from disk on demand for the detail view. That lands
// around 40MB for ~18.7k clips.
//
// The parsed index is persisted (gzipped JSON) to the state directory so a
// restart doesn't re-parse 117MB of JSON on a 1.6GHz Opteron; refresh only
// parses clip directories it hasn't seen before.

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// cacheVersion is bumped whenever the Track schema changes, so a deploy with
// new fields discards a stale cache instead of serving records with silently
// empty columns.
const cacheVersion = 1

// Edge kinds. These name *how* a clip derives from another; the underlying
// pointers are distinct fields in Suno's metadata, and `metadata.task` carries
// the finer-grained label (vox_cover, artist_cover, extend, …).
const (
	EdgeCover   = "cover"   // metadata.cover_clip_id
	EdgeEdit    = "edit"    // metadata.edited_clip_id
	EdgeArtist  = "artist"  // metadata.artist_clip_id
	EdgeStem    = "stem"    // metadata.stem_from_id
	EdgeStyles  = "styles"  // metadata.styles_lyrics_clip_id
	EdgeMashup  = "mashup"  // metadata.mashup_clip_ids[]
	EdgeExtend  = "extend"  // metadata.history[].id (ordered continue-chain)
	EdgeConcat  = "concat"  // metadata.concat_history[].id
	EdgePersona = "persona" // persona.root_clip_id
)

// Edge is one typed link from a clip to a clip it derives from.
type Edge struct {
	Target string `json:"target"`
	Kind   string `json:"kind"`
	// At is the continue_at offset for extend/concat edges (seconds), 0 else.
	At float64 `json:"at,omitempty"`
}

// Child is the reverse direction: who derives from this clip.
type Child struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// Track is the compact record the API serves and filters over.
type Track struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Created  time.Time `json:"created"`
	Duration float64   `json:"duration"`
	Model    string    `json:"model"`
	Version  string    `json:"version"`
	Tags     []string  `json:"tags"`
	NegTags  []string  `json:"negTags,omitempty"`
	Display  string    `json:"displayTag,omitempty"`
	Project  string    `json:"project,omitempty"`
	Kind     string    `json:"kind,omitempty"` // metadata.type (gen/upload/concat/…)
	Task     string    `json:"task,omitempty"` // metadata.task — the edge label
	Batch    int       `json:"batch"`
	Liked    bool      `json:"liked"`
	Public   bool      `json:"public"`
	Instr    bool      `json:"instrumental"`
	Remix    bool      `json:"remix"`
	Lyrics   bool      `json:"hasLyrics"`
	Plays    int       `json:"plays"`
	Upvotes  int       `json:"upvotes"`
	Cover    bool      `json:"hasCover"`
	WavBytes int64     `json:"wavBytes"`

	// Persona — a voice/artist identity. Roughly half the archive carries one
	// (223 distinct across ~9.3k clips), so this is a first-class facet.
	Persona      string `json:"persona,omitempty"`
	PersonaID    string `json:"personaId,omitempty"`
	PersonaRoot  string `json:"personaRoot,omitempty"` // clip the persona was built from
	VoicePersona bool   `json:"voicePersona,omitempty"`
	IsPersonaRoot bool  `json:"isPersonaRoot,omitempty"`

	// Parents are every typed derivation edge. Primary is the one used for
	// tree layout: the first non-null of cover/edit/artist/stem/styles.
	Parents []Edge `json:"parents,omitempty"`
	Primary string `json:"primaryParent,omitempty"`

	// Dir is the "<YYYY>/<MM>" path segment under tracks/.
	Dir string `json:"dir"`

	// Search is the lowercased haystack for free-text queries. Persisted (it
	// carries the lyrics, which are not otherwise held in memory) but omitted
	// from API responses — see forWire.
	Search string `json:"s,omitempty"`
}

func (t Track) forWire() Track {
	t.Search = ""
	return t
}

// GroupKey identifies one generation. Suno returns two clips per generation
// sharing an exact title and created_at (differing only in batch_index), so a
// flat listing shows most songs twice. Verified against the archive: siblings
// share created_at to the millisecond, while genuine re-generations of the
// same title land on different timestamps and stay separate.
func (t Track) GroupKey() string {
	return t.Title + "\x00" + t.Created.Format(time.RFC3339Nano)
}

// rawMeta is the subset of meta.json the index needs.
type rawMeta struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	CreatedAt   string `json:"created_at"`
	ModelName   string `json:"model_name"`
	MajorVer    string `json:"major_model_version"`
	IsLiked     bool   `json:"is_liked"`
	IsPublic    bool   `json:"is_public"`
	PlayCount   int    `json:"play_count"`
	UpvoteCount int    `json:"upvote_count"`
	BatchIndex  int    `json:"batch_index"`
	DisplayTags string `json:"display_tags"`
	IsPersonaRoot bool `json:"is_persona_root"`
	Persona     *struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		PersonaType    string `json:"persona_type"`
		RootClipID     string `json:"root_clip_id"`
		IsVoicePersona bool   `json:"is_voice_persona"`
	} `json:"persona"`
	Project *struct {
		Name string `json:"name"`
	} `json:"project"`
	Metadata struct {
		Duration         float64  `json:"duration"`
		Tags             string   `json:"tags"`
		NegativeTags     string   `json:"negative_tags"`
		Prompt           string   `json:"prompt"`
		Description      string   `json:"gpt_description_prompt"`
		Type             string   `json:"type"`
		Task             string   `json:"task"`
		MakeInstrumental bool     `json:"make_instrumental"`
		IsRemix          bool     `json:"is_remix"`
		CoverClipID      string   `json:"cover_clip_id"`
		EditedClipID     string   `json:"edited_clip_id"`
		ArtistClipID     string   `json:"artist_clip_id"`
		StemFromID       string   `json:"stem_from_id"`
		StylesLyricsID   string   `json:"styles_lyrics_clip_id"`
		MashupClipIDs    []string `json:"mashup_clip_ids"`
		History          []struct {
			ID         string  `json:"id"`
			ContinueAt float64 `json:"continue_at"`
		} `json:"history"`
		ConcatHistory []struct {
			ID         string  `json:"id"`
			ContinueAt float64 `json:"continue_at"`
		} `json:"concat_history"`
	} `json:"metadata"`
}

func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}

// parseTrack turns one meta.json into a Track. dir is the "<YYYY>/<MM>"
// segment; id is the clip directory name (authoritative).
func parseTrack(path, dir, id string) (*Track, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m rawMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	created, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		// Not fatal — an unparseable timestamp shouldn't drop the clip; it
		// just sorts to the zero time.
		created = time.Time{}
	}
	t := &Track{
		ID:            id,
		Title:         m.Title,
		Created:       created,
		Duration:      m.Metadata.Duration,
		Model:         m.ModelName,
		Version:       m.MajorVer,
		Tags:          splitTags(m.Metadata.Tags),
		NegTags:       splitTags(m.Metadata.NegativeTags),
		Display:       strings.TrimSpace(m.DisplayTags),
		Kind:          m.Metadata.Type,
		Task:          m.Metadata.Task,
		Batch:         m.BatchIndex,
		Liked:         m.IsLiked,
		Public:        m.IsPublic,
		Instr:         m.Metadata.MakeInstrumental,
		Remix:         m.Metadata.IsRemix,
		Lyrics:        strings.TrimSpace(m.Metadata.Prompt) != "",
		Plays:         m.PlayCount,
		Upvotes:       m.UpvoteCount,
		IsPersonaRoot: m.IsPersonaRoot,
		Dir:           dir,
	}
	if m.Project != nil {
		t.Project = m.Project.Name
	}
	if m.Persona != nil {
		t.Persona = strings.TrimSpace(m.Persona.Name)
		t.PersonaID = m.Persona.ID
		t.PersonaRoot = m.Persona.RootClipID
		t.VoicePersona = m.Persona.IsVoicePersona
	}

	// Typed derivation edges. Order matters: Primary is the first non-empty of
	// cover/edit/artist/stem/styles (verified in-archive that cover and edit
	// point at the same clip when both are set).
	add := func(target, kind string, at float64) {
		if target == "" || target == id {
			return
		}
		t.Parents = append(t.Parents, Edge{Target: target, Kind: kind, At: at})
		if t.Primary == "" {
			t.Primary = target
		}
	}
	add(m.Metadata.CoverClipID, EdgeCover, 0)
	if m.Metadata.EditedClipID != m.Metadata.CoverClipID {
		add(m.Metadata.EditedClipID, EdgeEdit, 0)
	}
	add(m.Metadata.ArtistClipID, EdgeArtist, 0)
	add(m.Metadata.StemFromID, EdgeStem, 0)
	add(m.Metadata.StylesLyricsID, EdgeStyles, 0)
	for _, mid := range m.Metadata.MashupClipIDs {
		add(mid, EdgeMashup, 0)
	}
	// history is an ordered continue-chain; the last entry is the immediate
	// predecessor of an "extend".
	for _, h := range m.Metadata.History {
		add(h.ID, EdgeExtend, h.ContinueAt)
	}
	for _, h := range m.Metadata.ConcatHistory {
		add(h.ID, EdgeConcat, h.ContinueAt)
	}

	// One lowercased haystack: everything a free-text query should reach.
	var sb strings.Builder
	sb.Grow(len(m.Title) + len(m.Metadata.Tags) + len(m.Metadata.Description) + len(m.Metadata.Prompt) + 64)
	for _, s := range []string{
		m.Title, m.Metadata.Tags, m.DisplayTags, m.Metadata.Description,
		t.Project, t.Persona, m.Metadata.Task, m.Metadata.Prompt,
	} {
		if s != "" {
			sb.WriteString(strings.ToLower(s))
			sb.WriteByte('\n')
		}
	}
	t.Search = sb.String()
	return t, nil
}

// Library holds the index and serves reads under an RWMutex.
type Library struct {
	dataDir   string
	cachePath string
	log       *slog.Logger

	mu       sync.RWMutex
	byID     map[string]*Track
	sorted   []*Track           // by Created desc — the default listing order
	children map[string][]Child // reverse index: clip id -> who derives from it

	ready        atomic.Bool
	scanned      atomic.Int64
	total        atomic.Int64
	backfillDone atomic.Bool
	lastErr      atomic.Pointer[string]
}

func NewLibrary(dataDir, cachePath string, log *slog.Logger) *Library {
	return &Library{
		dataDir:   dataDir,
		cachePath: cachePath,
		log:       log,
		byID:      make(map[string]*Track),
		children:  make(map[string][]Child),
	}
}

func (l *Library) tracksRoot() string { return filepath.Join(l.dataDir, "tracks") }

func (l *Library) clipPath(t *Track, name string) string {
	return filepath.Join(l.tracksRoot(), t.Dir, t.ID, name)
}

type clipRef struct{ dir, id string }

// scanDirs enumerates every clip directory under tracks/<YYYY>/<MM>/. A
// directory walk only — no meta.json is opened — so it stays cheap enough to
// run on every refresh while the daemon is still backfilling.
func (l *Library) scanDirs() ([]clipRef, error) {
	root := l.tracksRoot()
	years, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var refs []clipRef
	for _, y := range years {
		if !y.IsDir() {
			continue
		}
		months, err := os.ReadDir(filepath.Join(root, y.Name()))
		if err != nil {
			continue
		}
		for _, mo := range months {
			if !mo.IsDir() {
				continue
			}
			dir := filepath.Join(y.Name(), mo.Name())
			clips, err := os.ReadDir(filepath.Join(root, dir))
			if err != nil {
				continue
			}
			for _, c := range clips {
				if c.IsDir() {
					refs = append(refs, clipRef{dir: dir, id: c.Name()})
				}
			}
		}
	}
	return refs, nil
}

// readBackfillDone reads the one field of the daemon's index.json the UI
// needs: whether the historical backfill has finished. It decides how an
// unresolved lineage edge is presented — "not archived yet" (backfill still
// running) vs "not in archive" (backfill complete, so it never will be).
func (l *Library) readBackfillDone() {
	f, err := os.Open(filepath.Join(l.dataDir, "index.json"))
	if err != nil {
		return
	}
	defer f.Close()
	// Decode only the leading scalar fields; the huge `index` map is skipped
	// by the streaming decoder once the struct fields are satisfied... it is
	// not, so decode into a struct and let encoding/json ignore the rest. The
	// file is ~9MB; this runs once per refresh interval, not per request.
	var head struct {
		BackfillDone bool `json:"backfill_done"`
	}
	if err := json.NewDecoder(f).Decode(&head); err != nil {
		return
	}
	l.backfillDone.Store(head.BackfillDone)
}

// Refresh brings the index in line with the archive: new clip directories are
// parsed, vanished ones dropped, and the reverse (children) index rebuilt.
func (l *Library) Refresh() error {
	refs, err := l.scanDirs()
	if err != nil {
		return err
	}
	l.total.Store(int64(len(refs)))

	l.mu.RLock()
	todo := make([]clipRef, 0, 64)
	for _, r := range refs {
		if _, ok := l.byID[r.id]; !ok {
			todo = append(todo, r)
		}
	}
	known := len(l.byID)
	l.mu.RUnlock()
	l.scanned.Store(int64(known))

	if len(todo) > 0 {
		l.log.Info("indexing clips", "new", len(todo), "known", known)
	}

	// Parse in parallel — europa is only 2 cores, but meta.json reads are
	// ZFS-bound as much as CPU-bound, so a small pool still helps.
	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	if workers > 8 {
		workers = 8
	}
	jobs := make(chan clipRef)
	results := make(chan *Track, workers*4)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				base := filepath.Join(l.tracksRoot(), r.dir, r.id)
				t, err := parseTrack(filepath.Join(base, "meta.json"), r.dir, r.id)
				if err != nil {
					// A clip mid-write (the daemon is still backfilling) or a
					// malformed file: skip it; the next refresh retries.
					l.scanned.Add(1)
					continue
				}
				if fi, err := os.Stat(filepath.Join(base, r.id+".wav")); err == nil {
					t.WavBytes = fi.Size()
				}
				if _, err := os.Stat(filepath.Join(base, "cover.jpg")); err == nil {
					t.Cover = true
				}
				l.scanned.Add(1)
				results <- t
			}
		}()
	}
	go func() {
		for _, r := range todo {
			jobs <- r
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	added := make([]*Track, 0, len(todo))
	for t := range results {
		added = append(added, t)
	}

	present := make(map[string]bool, len(refs))
	for _, r := range refs {
		present[r.id] = true
	}

	l.mu.Lock()
	for _, t := range added {
		l.byID[t.ID] = t
	}
	for id := range l.byID {
		if !present[id] {
			delete(l.byID, id)
		}
	}
	l.resortLocked()
	l.rebuildChildrenLocked()
	n := len(l.byID)
	l.mu.Unlock()

	l.readBackfillDone()
	l.ready.Store(true)
	if len(added) > 0 {
		if err := l.saveCache(); err != nil {
			l.log.Warn("index cache save failed", "err", err)
		}
		l.log.Info("index updated", "added", len(added), "total", n)
	}
	return nil
}

func (l *Library) resortLocked() {
	l.sorted = make([]*Track, 0, len(l.byID))
	for _, t := range l.byID {
		l.sorted = append(l.sorted, t)
	}
	sort.Slice(l.sorted, func(i, j int) bool {
		a, b := l.sorted[i], l.sorted[j]
		if a.Created.Equal(b.Created) {
			if a.Batch != b.Batch {
				return a.Batch < b.Batch
			}
			return a.ID < b.ID
		}
		return a.Created.After(b.Created)
	})
}

// rebuildChildrenLocked rebuilds the reverse lineage index. Rebuilt wholesale
// on every refresh rather than patched incrementally: the archive is still
// downloading, so edges routinely arrive before their targets do, and a clip
// indexed today may become the parent of clips already indexed. Keyed on the
// raw target id whether or not it currently resolves.
func (l *Library) rebuildChildrenLocked() {
	l.children = make(map[string][]Child, len(l.byID))
	for _, t := range l.sorted {
		for _, e := range t.Parents {
			l.children[e.Target] = append(l.children[e.Target], Child{ID: t.ID, Kind: e.Kind})
		}
	}
}

func (l *Library) All() []*Track {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.sorted
}

func (l *Library) Get(id string) (*Track, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t, ok := l.byID[id]
	return t, ok
}

// Children returns the clips derived from id (may include ids not themselves
// archived yet — callers resolve via Get).
func (l *Library) Children(id string) []Child {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.children[id]
}

func (l *Library) Status() map[string]any {
	l.mu.RLock()
	n := len(l.byID)
	l.mu.RUnlock()
	st := map[string]any{
		"ready":        l.ready.Load(),
		"indexed":      n,
		"scanned":      l.scanned.Load(),
		"total":        l.total.Load(),
		"backfillDone": l.backfillDone.Load(),
	}
	if e := l.lastErr.Load(); e != nil {
		st["error"] = *e
	}
	return st
}

// --- derived-index cache -------------------------------------------------

type cacheEnvelope struct {
	Version int      `json:"version"`
	Tracks  []*Track `json:"tracks"`
}

func (l *Library) loadCache() error {
	f, err := os.Open(l.cachePath)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	var env cacheEnvelope
	if err := json.NewDecoder(zr).Decode(&env); err != nil {
		return err
	}
	if env.Version != cacheVersion {
		return fmt.Errorf("cache version %d != %d", env.Version, cacheVersion)
	}
	l.mu.Lock()
	for _, t := range env.Tracks {
		l.byID[t.ID] = t
	}
	l.resortLocked()
	l.rebuildChildrenLocked()
	n := len(l.byID)
	l.mu.Unlock()
	l.log.Info("index cache loaded", "tracks", n)
	return nil
}

func (l *Library) saveCache() error {
	l.mu.RLock()
	env := cacheEnvelope{Version: cacheVersion, Tracks: make([]*Track, len(l.sorted))}
	copy(env.Tracks, l.sorted)
	l.mu.RUnlock()

	tmp := l.cachePath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if err := json.NewEncoder(zw).Encode(env); err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, l.cachePath)
}

// Start loads any cached index, then refreshes in the background and keeps
// refreshing on the given interval. The HTTP server comes up immediately so
// the UI shows indexing progress rather than an empty page — and because the
// archive is still downloading, refresh keeps finding new clips.
func (l *Library) Start(interval time.Duration) {
	if err := l.loadCache(); err != nil && !os.IsNotExist(err) {
		l.log.Warn("index cache unusable, rebuilding", "err", err)
	}
	go func() {
		for {
			if err := l.Refresh(); err != nil {
				msg := err.Error()
				l.lastErr.Store(&msg)
				l.log.Warn("refresh failed", "err", err)
			} else {
				l.lastErr.Store(nil)
			}
			time.Sleep(interval)
		}
	}()
}
