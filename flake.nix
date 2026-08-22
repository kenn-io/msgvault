{
  description = "msgvault — offline Gmail archive with full-text search";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    gitignore.url = "github:hercules-ci/gitignore.nix";
    gitignore.inputs.nixpkgs.follows = "nixpkgs";

    bun2nix.url = "github:nix-community/bun2nix/2.1.2";
    bun2nix.inputs.nixpkgs.follows = "nixpkgs";
    bun2nix.inputs.systems.follows = "flake-utils/systems";
  };

  outputs =
    {
      nixpkgs,
      flake-utils,
      gitignore,
      bun2nix,
      ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        # Pin Go 1.27.0 until the locked nixpkgs revision ships it.
        # Scoped to msgvault only — do NOT export via overlay, that would
        # invalidate every Go derivation in the transitive closure.
        goPinned = pkgs.go_1_26.overrideAttrs (old: rec {
          version = "1.27.0";
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go${version}.src.tar.gz";
            hash = "sha256-cAJAPXzERSnvbSb2mkSBgmM5Xq18FsBaWAiuBH6+sOU=";
          };
          patches =
            builtins.filter (
              patch: !(pkgs.lib.hasSuffix "go_no_vendor_checks-1.26.patch" (toString patch))
            ) old.patches
            ++ [
              (pkgs.fetchurl {
                url = "https://raw.githubusercontent.com/NixOS/nixpkgs/67a70befab1966b026701e8e94cf19b1543075c5/pkgs/development/compilers/go/go_no_vendor_checks-1.27.patch";
                hash = "sha256-aTpc6kAX9bAyMMAHqzldcb2EEseCpke7QlJuQ1Bk6jc=";
              })
            ];
        });

        buildGoModule = pkgs.buildGoModule.override { go = goPinned; };

        bunPinned = pkgs.bun.overrideAttrs (old: rec {
          version = "1.3.14";
          src = passthru.sources.${system};
          passthru = old.passthru // {
            sources = {
              aarch64-darwin = pkgs.fetchurl {
                url = "https://github.com/oven-sh/bun/releases/download/bun-v${version}/bun-darwin-aarch64.zip";
                hash = "sha256-2LliIYKK1vl6x6wKt+lYcjQa92MAHogD6CZ2UsJlJiA=";
              };
              aarch64-linux = pkgs.fetchurl {
                url = "https://github.com/oven-sh/bun/releases/download/bun-v${version}/bun-linux-aarch64.zip";
                hash = "sha256-on/7Y6gxA3WDbg1vZorhf6jY0YuIw3yCHGUzGXOhmjs=";
              };
              x86_64-darwin = pkgs.fetchurl {
                url = "https://github.com/oven-sh/bun/releases/download/bun-v${version}/bun-darwin-x64-baseline.zip";
                hash = "sha256-PjWtb1OXGpg0v55nhuKt9ytfGSHMmpxf3gc9KXKUQHY=";
              };
              x86_64-linux = pkgs.fetchurl {
                url = "https://github.com/oven-sh/bun/releases/download/bun-v${version}/bun-linux-x64.zip";
                hash = "sha256-lR7iruhV8IWVruxiJSJqKY0/6oOj3NZGXAnLzN9+hI8=";
              };
            };
          };
        });

        bun2nixPinned = bun2nix.packages.${system}.default.overrideAttrs (old: {
          passthru = old.passthru // {
            fetchBunDeps = args: old.passthru.fetchBunDeps (args // { patchShebangs = false; });
            hook = old.passthru.hook.overrideAttrs (hookOld: {
              propagatedBuildInputs = map (
                input: if input.pname or null == "bun" then bunPinned else input
              ) hookOld.propagatedBuildInputs;
            });
          };
        });

        msgvault = pkgs.callPackage ./nix/package.nix {
          inherit buildGoModule;
          inherit (gitignore.lib) gitignoreSource;
          bun2nix = bun2nixPinned;
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
