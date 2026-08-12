package main

// suno-web — a library browser for the Suno archive that modules/services/
// suno-backup.nix mirrors onto europa: search and filter across every
// metadata field, follow the derivation graph (covers, extends, stems,
// mashups, personas), build playlists, and play the lossless masters.
//
// Stdlib-only, in the house pattern of pkgs/nom-web and pkgs/suno-backup: no
// flake input, no database, embedded static assets.
//
// Paths served to clients are never built from user input — a clip id is
// resolved through the index first, and the on-disk location comes from the
// indexed record, so there is no path-traversal surface.

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed static/*
var staticFS embed.FS

// staticRoot serves the embedded UI with the "static/" prefix stripped.
func staticRoot() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type server struct {
	lib   *Library
	pls   *PlaylistStore
	log   *slog.Logger
	dataDir string
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	dataDir := env("SUNOWEB_DATA_DIR", "/tank/archive/suno")
	stateDir := env("SUNOWEB_STATE_DIR", "/var/lib/jupiter-suno-web")
	addr := ":" + env("SUNOWEB_PORT", "8093")
	refresh := envDuration("SUNOWEB_REFRESH", 5*time.Minute)

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		log.Error("state dir", "err", err)
		os.Exit(1)
	}

	pls, err := NewPlaylistStore(filepath.Join(stateDir, "playlists.json"))
	if err != nil {
		log.Error("playlist store", "err", err)
		os.Exit(1)
	}

	lib := NewLibrary(dataDir, filepath.Join(stateDir, "index-cache.json.gz"), log)
	// Index in the background so the UI comes up immediately and can show
	// progress — a cold parse of ~18.7k clips takes a while on europa, and the
	// archive is still being downloaded, so refresh keeps finding new clips.
	lib.Start(refresh)

	s := &server{lib: lib, pls: pls, log: log, dataDir: dataDir}
	log.Info("suno-web listening", "addr", addr, "dataDir", dataDir, "stateDir", stateDir)
	if err := http.ListenAndServe(addr, s.routes()); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/tracks", s.handleTracks)
	mux.HandleFunc("GET /api/track/{id}", s.handleTrack)
	mux.HandleFunc("GET /api/lineage/{id}", s.handleLineage)
	mux.HandleFunc("GET /api/lyrics/{id}", s.handleLyrics)
	mux.HandleFunc("GET /api/personas", s.handlePersonas)
	mux.HandleFunc("GET /api/audio/{id}", s.handleAudio)
	mux.HandleFunc("GET /api/cover/{id}", s.handleCover)

	mux.HandleFunc("GET /api/playlists", s.handlePlaylists)
	mux.HandleFunc("POST /api/playlists", s.handleCreatePlaylist)
	mux.HandleFunc("GET /api/playlists/{id}", s.handleGetPlaylist)
	mux.HandleFunc("PATCH /api/playlists/{id}", s.handlePatchPlaylist)
	mux.HandleFunc("DELETE /api/playlists/{id}", s.handleDeletePlaylist)
	mux.HandleFunc("POST /api/playlists/{id}/tracks", s.handleAddTracks)
	mux.HandleFunc("DELETE /api/playlists/{id}/tracks", s.handleRemoveTracks)

	mux.Handle("/", http.FileServerFS(staticRoot()))
	return mux
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := s.lib.Status()
	st["playlists"] = len(s.pls.List())
	writeJSON(w, http.StatusOK, st)
}

func (s *server) handleTracks(w http.ResponseWriter, r *http.Request) {
	q := ParseQuery(r.URL.Query())
	// A playlist filter restricts the result set to that playlist's members,
	// so all the same filters/search work inside a playlist.
	if pid := r.URL.Query().Get("playlist"); pid != "" {
		p, ok := s.pls.Get(pid)
		if !ok {
			httpError(w, http.StatusNotFound, "no such playlist")
			return
		}
		q.IDs = make(map[string]bool, len(p.TrackIDs))
		for _, id := range p.TrackIDs {
			q.IDs[id] = true
		}
	}
	writeJSON(w, http.StatusOK, Search(s.lib, q))
}

// handleTrack returns the indexed record, the complete raw meta.json (every
// field Suno shipped — the index deliberately holds only a subset), and the
// resolved lineage neighbours.
func (s *server) handleTrack(w http.ResponseWriter, r *http.Request) {
	t, ok := s.lib.Get(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, "no such track")
		return
	}
	resp := map[string]any{"track": t.forWire()}

	if b, err := os.ReadFile(s.lib.clipPath(t, "meta.json")); err == nil {
		var raw json.RawMessage = b
		resp["meta"] = &raw
	}

	// Parents and children, resolved where possible.
	parents := make([]map[string]any, 0, len(t.Parents))
	for _, e := range t.Parents {
		item := map[string]any{"id": e.Target, "kind": e.Kind, "resolved": false}
		if e.At > 0 {
			item["at"] = e.At
		}
		if pt, ok := s.lib.Get(e.Target); ok {
			item["resolved"] = true
			item["track"] = pt.forWire()
		}
		parents = append(parents, item)
	}
	kids := s.lib.Children(t.ID)
	children := make([]map[string]any, 0, len(kids))
	for _, k := range kids {
		item := map[string]any{"id": k.ID, "kind": k.Kind, "resolved": false}
		if ct, ok := s.lib.Get(k.ID); ok {
			item["resolved"] = true
			item["track"] = ct.forWire()
		}
		children = append(children, item)
	}
	resp["parents"] = parents
	resp["children"] = children
	resp["backfillDone"] = s.lib.Status()["backfillDone"]
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleLineage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.lib.Get(r.PathValue("id")); !ok {
		httpError(w, http.StatusNotFound, "no such track")
		return
	}
	lin := BuildLineage(s.lib, r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{
		"lineage":      lin,
		"backfillDone": s.lib.Status()["backfillDone"],
	})
}

// handleLyrics returns just the lyric text (and the description prompt) for
// the full-screen now-playing view. Deliberately separate from handleTrack:
// that endpoint ships the entire raw meta.json plus lineage, which is far more
// than a lyrics pane needs on every track change.
//
// Suno archives no word timings or alignment data of any kind, so there is
// nothing here to drive true karaoke sync — the client scrolls proportionally
// to playback position instead.
func (s *server) handleLyrics(w http.ResponseWriter, r *http.Request) {
	t, ok := s.lib.Get(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, "no such track")
		return
	}
	var m struct {
		Metadata struct {
			Prompt      string `json:"prompt"`
			Description string `json:"gpt_description_prompt"`
		} `json:"metadata"`
	}
	if b, err := os.ReadFile(s.lib.clipPath(t, "meta.json")); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          t.ID,
		"lyrics":      m.Metadata.Prompt,
		"description": m.Metadata.Description,
	})
}

// handlePersonas lists every persona with its clip count and root clip — the
// "artists" view. Roughly half the archive carries a persona.
func (s *server) handlePersonas(w http.ResponseWriter, r *http.Request) {
	type persona struct {
		Name   string `json:"name"`
		ID     string `json:"id,omitempty"`
		Root   string `json:"root,omitempty"`
		Voice  bool   `json:"voice"`
		Count  int    `json:"count"`
		Liked  int    `json:"liked"`
		Latest time.Time `json:"latest"`
	}
	agg := map[string]*persona{}
	for _, t := range s.lib.All() {
		if t.Persona == "" {
			continue
		}
		p, ok := agg[t.Persona]
		if !ok {
			p = &persona{Name: t.Persona, ID: t.PersonaID, Root: t.PersonaRoot, Voice: t.VoicePersona}
			agg[t.Persona] = p
		}
		p.Count++
		if t.Liked {
			p.Liked++
		}
		if t.Created.After(p.Latest) {
			p.Latest = t.Created
		}
	}
	out := make([]*persona, 0, len(agg))
	for _, p := range agg {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"personas": out, "total": len(out)})
}

// handleAudio streams the lossless WAV master. http.ServeContent gives Range
// support, so the player can seek without downloading the whole 35-45MB file.
func (s *server) handleAudio(w http.ResponseWriter, r *http.Request) {
	t, ok := s.lib.Get(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, "no such track")
		return
	}
	s.serveFile(w, r, s.lib.clipPath(t, t.ID+".wav"), "audio/wav")
}

func (s *server) handleCover(w http.ResponseWriter, r *http.Request) {
	t, ok := s.lib.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, s.lib.clipPath(t, "cover.jpg"), "image/jpeg")
}

func (s *server) serveFile(w http.ResponseWriter, r *http.Request, path, ctype string) {
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	// The archive is immutable once written, so let the browser cache hard.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

// --- playlists -----------------------------------------------------------

func (s *server) handlePlaylists(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"playlists": s.pls.List()})
}

type playlistBody struct {
	Name     *string  `json:"name"`
	Note     *string  `json:"note"`
	TrackIDs []string `json:"trackIds"`
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

func (s *server) handleCreatePlaylist(w http.ResponseWriter, r *http.Request) {
	var b playlistBody
	if err := decodeBody(r, &b); err != nil {
		httpError(w, http.StatusBadRequest, "bad body")
		return
	}
	name := ""
	if b.Name != nil {
		name = *b.Name
	}
	note := ""
	if b.Note != nil {
		note = *b.Note
	}
	p, err := s.pls.Create(name, note, b.TrackIDs)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *server) handleGetPlaylist(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pls.Get(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, "no such playlist")
		return
	}
	// Resolve members in stored order, so the UI can render without a second
	// round-trip. Unresolved ids (not yet archived) are skipped.
	tracks := make([]Track, 0, len(p.TrackIDs))
	for _, id := range p.TrackIDs {
		if t, ok := s.lib.Get(id); ok {
			tracks = append(tracks, t.forWire())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlist": p, "tracks": tracks})
}

func (s *server) handlePatchPlaylist(w http.ResponseWriter, r *http.Request) {
	var b playlistBody
	if err := decodeBody(r, &b); err != nil {
		httpError(w, http.StatusBadRequest, "bad body")
		return
	}
	p, err := s.pls.Update(r.PathValue("id"), b.Name, b.Note, b.TrackIDs)
	s.replyPlaylist(w, p, err)
}

func (s *server) handleDeletePlaylist(w http.ResponseWriter, r *http.Request) {
	if err := s.pls.Delete(r.PathValue("id")); err != nil {
		s.replyPlaylist(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleAddTracks(w http.ResponseWriter, r *http.Request) {
	var b playlistBody
	if err := decodeBody(r, &b); err != nil {
		httpError(w, http.StatusBadRequest, "bad body")
		return
	}
	p, err := s.pls.Add(r.PathValue("id"), b.TrackIDs)
	s.replyPlaylist(w, p, err)
}

func (s *server) handleRemoveTracks(w http.ResponseWriter, r *http.Request) {
	var b playlistBody
	if err := decodeBody(r, &b); err != nil {
		httpError(w, http.StatusBadRequest, "bad body")
		return
	}
	p, err := s.pls.Remove(r.PathValue("id"), b.TrackIDs)
	s.replyPlaylist(w, p, err)
}

func (s *server) replyPlaylist(w http.ResponseWriter, p *Playlist, err error) {
	switch {
	case errors.Is(err, errNotFound):
		httpError(w, http.StatusNotFound, "no such playlist")
	case err != nil:
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, p)
	}
}
