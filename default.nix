{
  lib,
  buildGoModule,
}:

# suno-web — a library browser for a local Suno archive: search and filter
# across every archived metadata field, follow the derivation graph between
# clips (covers, extends, stems, mashups, personas), build playlists, and
# stream the lossless masters.
#
# Stdlib-only: zero non-stdlib Go dependencies. The index is an in-memory
# structure built from the archive's meta.json files, deliberately not a
# database, so no cgo/sqlite driver is pulled in and the vendored-deps set is
# empty. If a dependency is ever added, run `go mod tidy` first and replace
# `vendorHash = null` with the real hash.
buildGoModule {
  pname = "suno-web";
  version = "0.1.0";

  src = ./.;

  vendorHash = null;

  ldflags = [ "-s" ];

  doCheck = true;

  meta = with lib; {
    description = "Library browser for a local Suno archive (search, lineage, playlists, playback)";
    homepage = "https://github.com/belikh/suno-web";
    mainProgram = "suno-web";
    license = licenses.mit;
    platforms = platforms.linux;
  };
}
