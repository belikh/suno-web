<p align="right">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/belikh/design/main/packages/graphics/logo/jupiter-logo-dark.svg">
    <img alt="jupiter.au" src="https://raw.githubusercontent.com/belikh/design/main/packages/graphics/logo/jupiter-logo-light.svg" width="260">
  </picture>
</p>

# suno-web

A library browser for a local Suno archive, the kind you get by mirroring your own account's tracks to disk. It searches and filters on every archived metadata field, shows the derivation graph between clips (covers, extends, stems, mashups, personas), builds playlists and plays the lossless masters.

It is one Go binary using only the standard library, with the UI embedded. It needs no database, build step or external service.

<p>
  <a href="#install">Install</a> ·
  <a href="#archive-layout">Archive layout</a> ·
  <a href="#features">Features</a> ·
  <a href="#configuration">Configuration</a> ·
  <a href="#api">API</a> ·
  <a href="#license">License</a>
</p>

<br>

---

<br>

## Install

With Go:

```sh
go build -o suno-web .
SUNOWEB_DATA_DIR=/path/to/archive ./suno-web
```

Open <http://localhost:8093>.

With Nix:

```sh
nix build github:belikh/suno-web
```

To use it from another flake:

```nix
{
  inputs.suno-web.url = "github:belikh/suno-web";
}
```

The flake exposes `packages.<system>.suno-web` and an additive `overlays.default`.

<br>

## Archive layout

suno-web only reads the archive. It expects this layout:

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

`meta.json` is the full per-clip object as Suno's API returns it. Everything the UI filters on comes from there, and nothing is fetched from the network.

Playlists are the only data suno-web owns. They live in a separate state directory.

<br>

## Features

- **Search.** One box over titles, lyrics, style tags, description prompts, persona names and task types.
- **Faceted filters with live counts.** Persona, relationship type, model, style tags, project, model version and clip type, plus yes/no toggles (liked, has lyrics, has persona, instrumental, remix, public) and date and duration ranges. Facet counts follow the current filter set.
- **Personas.** Browse Suno's voice and artist identities as cards with track counts, or filter by one. The persona name is in the `persona` object, not in a top-level `persona_name` field.
- **Relationships.** Clips form a graph, described below.
- **Playlists.** Created in the UI and stored as one JSON file, written to a temporary file and renamed.
- **Playback.** `Range`-seekable streaming of the masters, with a full-screen now-playing view: cover art enlarged behind, lyrics in front, `[Section]` markers styled.

### Relationships

The clip metadata carries several typed edges:

| Field | Meaning |
|---|---|
| `metadata.cover_clip_id` | this clip is a cover of another |
| `metadata.edited_clip_id` | derived by editing another clip |
| `metadata.artist_clip_id` | artist or voice reference clip |
| `metadata.stem_from_id` | stem extracted from another clip |
| `metadata.styles_lyrics_clip_id` | styles and lyrics taken from another clip |
| `metadata.mashup_clip_ids[]` | mashup sources (many) |
| `metadata.history[]` | ordered extend and continue chain, with `continue_at` |
| `metadata.concat_history[]` | concatenation lineage |
| `persona.root_clip_id` | the clip a persona was built from |

`metadata.task` has about 25 values (`cover`, `vox_cover`, `artist_cover`, `extend`, `gen_stem`, `mashup_condition` and others) and labels how a derivation happened.

suno-web builds a reverse index over all of these edges. For a clip it shows an ancestry breadcrumb from the root, then the full descendant tree, coloured by edge kind and collapsible. A single source clip can have hundreds of descendants, so you can play a whole family, or any one branch, as a queue.

> [!NOTE]
> Suno archives no word timings, alignment data or LRC, so lyrics cannot be synced exactly. They scroll in proportion to playback position, which drifts on tracks with long intros or instrumental outros. Auto-scroll is a toggle, and scrolling by hand pauses it. Exact sync would need forced alignment against the audio.

<br>

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `SUNOWEB_DATA_DIR` | `/tank/archive/suno` | archive root (read-only) |
| `SUNOWEB_STATE_DIR` | `/var/lib/jupiter-suno-web` | playlists and index cache |
| `SUNOWEB_PORT` | `8093` | listen port |
| `SUNOWEB_REFRESH` | `5m` | how often to rescan for new clips |

<details><summary><b>Indexing</b></summary>

On first run every `meta.json` is parsed into a compact in-memory record. The result is saved gzipped in the state directory, so restarts load at once and only new clips are parsed. Rescans are incremental and run every `SUNOWEB_REFRESH`, so an archive that is still downloading fills in without a restart.

Memory use is bounded: one lowercased search string per clip plus small scalar fields. The raw `meta.json` is read again only for the detail view. An archive of about 19,000 clips indexes in seconds and uses around 250 MB resident.

Lineage edges that point at clips you do not have yet show as "not archived yet" instead of "not in archive". suno-web reads `backfill_done` from `index.json`, when present, to tell the two apart.

</details>

<br>

## API

| Endpoint | Purpose |
|---|---|
| `GET /api/status` | index progress, counts, backfill state |
| `GET /api/tracks?…` | search, filter, facet, group, sort, paginate |
| `GET /api/track/{id}` | indexed record, full raw `meta.json` and neighbours |
| `GET /api/lineage/{id}` | ancestry chain and full descendant tree |
| `GET /api/lyrics/{id}` | lyric text only |
| `GET /api/personas` | every persona with counts |
| `GET /api/audio/{id}` | the master, `Range`-seekable |
| `GET /api/cover/{id}` | cover art |
| `GET/POST /api/playlists`, `GET/PATCH/DELETE /api/playlists/{id}` | playlist CRUD |
| `POST/DELETE /api/playlists/{id}/tracks` | add or remove tracks in a playlist |

<br>

## Scope

suno-web browses an archive and does not create one. Getting the tracks onto disk (signing in to Suno, converting and downloading masters, mirroring metadata) happens outside this repo.

<br>

## License

MIT. See [LICENSE](LICENSE).

<br>

<p align="right"><sub>jupiter.au</sub></p>
