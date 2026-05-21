{
  description = "msgvault — offline Gmail archive with full-text search";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    gitignore.url = "github:hercules-ci/gitignore.nix";
    gitignore.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      nixpkgs,
      flake-utils,
      gitignore,
      ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        # Pin Go 1.26.3 until nixpkgs-unstable ships it (currently 1.26.2).
        # Scoped to msgvault only — do NOT export via overlay, that would
        # invalidate every Go derivation in the transitive closure.
        goPinned = pkgs.go_1_26.overrideAttrs (_: rec {
          version = "1.26.3";
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go${version}.src.tar.gz";
            hash = "sha256-HGRoddCqh5kTMYTtV895/yS97+jIggRwYCqdPW2Rkrg=";
          };
        });

        buildGoModule = pkgs.buildGoModule.override { go = goPinned; };

        msgvault = pkgs.callPackage ./nix/package.nix {
          inherit buildGoModule;
          inherit (gitignore.lib) gitignoreSource;
        };
      in
      {
        packages = {
          default = msgvault;
          msgvault = msgvault;
        };

        apps.default = flake-utils.lib.mkApp { drv = msgvault; };

        devShells.default = pkgs.mkShell {
          packages = [
            goPinned
            pkgs.gopls
            pkgs.gotools
            pkgs.golangci-lint
            pkgs.delve
            pkgs.gcc
            pkgs.prek
            pkgs.sqlite-interactive
          ];
        };

        formatter = pkgs.nixfmt-rfc-style;
      }
    );
}
