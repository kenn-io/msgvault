{
  lib,
  buildGoModule,
  bun2nix,
  fetchFromGitHub,
  gitignoreSource,
  sqlite,
}:
let
  version = "0.19.3";
in
buildGoModule {
  pname = "msgvault";
  inherit version;

  src = gitignoreSource ../.;

  vendorHash = "sha256-jnmrZBb8rZChl/UvwMeMDjcIBj6Oyi7lmk9+jg8NnjY=";
  proxyVendor = true;

  bunDeps = bun2nix.fetchBunDeps {
    bunNix = ../web/bun.nix;
    overrides = {
      "@kenn-io/kit-ui@github:kenn-io/kit-ui#1e9dc7d" =
        _:
        fetchFromGitHub {
          owner = "kenn-io";
          repo = "kit-ui";
          rev = "1e9dc7d45525a471040b72b894432c4956542c38";
          hash = "sha256-XV7CuqMC+jlhaWQyXzcDukqnF73Lycy2Kueb7rMhxz8=";
        };
    };
  };
  bunRoot = "web";
  bunInstallFlags = [
    "--linker=hoisted"
    "--backend=copyfile"
  ];
  dontUseBunBuild = true;
  dontUseBunCheck = true;
  dontUseBunInstall = true;

  nativeBuildInputs = [ bun2nix.hook ];
  overrideModAttrs = _: previous: {
    nativeBuildInputs = builtins.filter (
      input: (input.name or "") != "bun2nix-hook"
    ) previous.nativeBuildInputs;
    preBuild = "";
  };

  preBuild = ''
    bun run --cwd web generate
    bun run --cwd web build
    mkdir -p internal/web/dist
    find internal/web/dist -mindepth 1 -maxdepth 1 ! -name stub.html -exec rm -rf {} +
    cp -R web/dist/. internal/web/dist/
    bun scripts/check-web-assets.mjs
  '';

  subPackages = [ "cmd/msgvault" ];

  # mattn/go-sqlite3, marcboeker/go-duckdb, and asg017/sqlite-vec-go-bindings
  # all link C code. buildGoModule defaults CGO_ENABLED to 1, but be explicit.
  env.CGO_ENABLED = 1;

  # sqlite-vec-go-bindings does `#include "sqlite3.h"` but ships no sqlite
  # source — provide the system header via buildInputs.
  buildInputs = [ sqlite ];

  tags = [
    "fts5"
    "sqlite_vec"
  ];

  ldflags = [
    "-s"
    "-w"
    "-X go.kenn.io/msgvault/cmd/msgvault/cmd.Version=${version}"
  ];

  doCheck = false;

  meta = {
    description = "Offline Gmail archive with full-text search";
    homepage = "https://github.com/kenn-io/msgvault";
    license = lib.licenses.asl20;
    mainProgram = "msgvault";
  };
}
