{
  description = "suno-web — a library browser for a local Suno archive (search, lineage, playlists, playback)";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        suno-web = pkgs.callPackage ./default.nix { };
        default = suno-web;
      });

      # Additive overlay — it only introduces `suno-web`, so applying it does
      # not perturb any existing package in a consumer's package set.
      overlays.default = final: _prev: {
        suno-web = final.callPackage ./default.nix { };
      };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
