{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs =
    { nixpkgs, ... }:
    let
      forAllSystems =
        fn:
        nixpkgs.lib.genAttrs [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin" ] (
          system: fn nixpkgs.legacyPackages.${system}
        );

      # Pin Go 1.26.3 until nixpkgs-unstable catches up from staging
      goPinned = pkgs: pkgs.go_1_26.overrideAttrs (old: rec {
        version = "1.26.3";
        src = pkgs.fetchurl {
          url = "https://go.dev/dl/go${version}.src.tar.gz";
          hash = "sha256-HGRoddCqh5kTMYTtV895/yS97+jIggRwYCqdPW2Rkrg=";
        };
      });
    in
    {
      packages = forAllSystems (pkgs: {
        default = (pkgs.buildGoModule.override { go = goPinned pkgs; }) {
          pname = "msgvault";
          version = "0.14.1";
          src = ./.;
          vendorHash = "sha256-rhijudB9WzFKtLAhfpuPw3osbo2lymTw1V+ZjuVyigU=";
          proxyVendor = true;
          subPackages = [ "cmd/msgvault" ];
          tags = [ "fts5" ];
          ldflags = [
            "-X github.com/wesm/msgvault/cmd/msgvault/cmd.Version=nix-dev"
          ];
        };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            (goPinned pkgs)
            pkgs.golangci-lint
            pkgs.gcc
            pkgs.prek
          ];
        };
      });
    };
}
