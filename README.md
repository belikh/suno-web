# suno-web

A library browser for a **local Suno archive** — the kind you get from mirroring
your own account's tracks to disk. Search and filter across every archived
metadata field, follow the derivation graph between clips (covers, extends,
stems, mashups, personas), build playlists, and play the lossless masters.

Single stdlib-only Go binary with the UI embedded. No database, no build step,
no external services.

---

## What it expects on disk

suno-web is read-only over an archive laid out like this:

```
<dataDir>/
  index.json                    # optional: only `backfill_done` is read
  tracks/
    2026/
      07/
        <clip-id>/
          <clip-id>.wav         # the master
          cover.jpg             # cover art
          meta.json             # the complete clip object from Suno
```

`meta.json` is the full per-clip object as Suno's API returns it. Everything
the UI filters on comes from there — nothing is re-fetched from the network.

The archive is never written to. Playlists (the only data suno-web owns) live
in a separate state directory.

## Features

**Search** — one box over titles, lyrics, style tags, description prompts,
persona names and task types.

**Faceted filters with live counts** — persona/voice, relationship type, model,
style tags, project, model version, clip type, plus yes/no toggles (liked, has
lyrics, has persona, instrumental, remix, public) and date/duration ranges.
Facet counts reflect the current filter set.

**Personas** — Suno's voice/artist identities are first-class. Browse them as
cards with track counts, or filter by one. (Note the persona name lives in the
`persona` *object*, not a top-level `persona_name` field — an easy thing to get
wrong.)

**Relationships** — this is the interesting part. Suno clips form a real DAG,
and the metadata carries several distinct typed edges:

| Field | Meaning |
|---|---|
| `metadata.cover_clip_id` | this clip is a cover of another |
| `metadata.edited_clip_id` | derived by editing another clip |
| `metadata.artist_clip_id` | artist/voice reference clip |
| `metadata.stem_from_id` | stem extracted from another clip |
| `metadata.styles_lyrics_clip_id` | styles/lyrics taken from another clip |
| `metadata.mashup_clip_ids[]` | mashup sources (many) |
| `metadata.history[]` | ordered extend/continue chain, with `continue_at` |
| `metadata.concat_history[]` | concatenation lineage |
| `persona.root_clip_id` | the clip a persona was built from |

`metadata.task` (≈25 values: `cover`, `vox_cover`, `artist_cover`, `extend`,
`gen_stem`, `mashup_condition`, …) labels *how* the derivation happened.

suno-web builds a reverse index over all of these and renders the family: an
ancestry breadcrumb from the root, then the full descendant tree, colour-coded
per edge kind and collapsible. Families get large — a single source clip can
have hundreds of descendants — so the whole family is playable as a queue, as
is any single branch.

**Playlists** — created in the UI, stored as one JSON file, written
temp-then-rename.

**Playback** — `Range`-seekable streaming of the masters, with a full-screen
now-playing view: cover art blown up behind, lyrics in front, `[Section]`
markers styled.

> **On lyric scrolling:** Suno archives no word timings, alignment data or LRC
> of any kind, so true karaoke sync is impossible from the metadata alone. The
> lyrics scroll *proportionally to playback position* — a decent approximation
> that drifts on tracks with long intros or instrumental outros. Auto-scroll is
> a toggle, and scrolling by hand suspends it. Real sync would need forced
> alignment against the audio.

## Running it

```sh
go build -o suno-web .
SUNOWEB_DATA_DIR=/path/to/archive ./suno-web
```

Then open <http://localhost:8093>.

| Variable | Default | Meaning |
|---|---|---|
| `SUNOWEB_DATA_DIR` | `/tank/archive/suno` | archive root (read-only) |
| `SUNOWEB_STATE_DIR` | `/var/lib/jupiter-suno-web` | playlists + index cache |
| `SUNOWEB_PORT` | `8093` | listen port |
| `SUNOWEB_REFRESH` | `5m` | how often to re-scan for new clips |

### Indexing

On first run every `meta.json` is parsed into a compact in-memory record; the
result is persisted (gzipped) to the state directory, so restarts load
instantly and only newly-appeared clips are parsed. Rescans are incremental and
run on `SUNOWEB_REFRESH`, so an archive that is still downloading fills in
without a restart.

Memory is deliberately bounded: one lowercased search blob per clip plus small
scalars, never the raw `meta.json` (that is re-read on demand for the detail
view). A ~19k-clip archive indexes in seconds and sits around 250MB resident.

Because an archive can be mid-download, lineage edges pointing at clips you
don't have yet are shown as *"not archived yet"* rather than *"not in archive"*
— it reads `backfill_done` from `index.json` to tell those apart, if present.

## Nix

```nix
{
  inputs.suno-web.url = "github:belikh/suno-web";
}
```

Exposes `packages.<system>.suno-web` and an additive `overlays.default`.
Build directly with:

```sh
nix build github:belikh/suno-web
```

## API

| Endpoint | Purpose |
|---|---|
| `GET /api/status` | index progress, counts, backfill state |
| `GET /api/tracks?…` | search/filter/facet/group/sort/paginate |
| `GET /api/track/{id}` | indexed record + full raw `meta.json` + neighbours |
| `GET /api/lineage/{id}` | ancestry chain + full descendant tree |
| `GET /api/lyrics/{id}` | lyric text only |
| `GET /api/personas` | every persona with counts |
| `GET /api/audio/{id}` | the master, `Range`-seekable |
| `GET /api/cover/{id}` | cover art |
| `GET/POST/PATCH/DELETE /api/playlists…` | playlist CRUD |

## Scope

This browses an archive; it does not create one. Getting the tracks onto disk
(authenticating to Suno, converting and downloading masters, mirroring
metadata) is a separate concern and not part of this repo.

## License

MIT — see [LICENSE](LICENSE).
