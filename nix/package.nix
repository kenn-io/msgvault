{
  lib,
  buildGoModule,
  bun2nix,
  fetchFromGitHub,
  gitignoreSource,
  nodejs,
  runCommand,
  sqlite,
}:
let
  version = "0.19.3";

  kitUiSrc = fetchFromGitHub {
    owner = "kenn-io";
    repo = "kit-ui";
    rev = "1e9dc7d45525a471040b72b894432c4956542c38";
    hash = "sha256-XV7CuqMC+jlhaWQyXzcDukqnF73Lycy2Kueb7rMhxz8=";
  };

in
buildGoModule {
  pname = "msgvault";
  inherit version;

  src = gitignoreSource ../.;

  vendorHash = "sha256-jnmrZBb8rZChl/UvwMeMDjcIBj6Oyi7lmk9+jg8NnjY=";
  proxyVendor = true;

  # bun2nix cannot express `github:` dependencies: the generated bun.nix names
  # the package npm-style, so its cache entry never matches the
  # `@GH@<owner>-<repo>-<committish>@@@1` folder bun looks up
  # (PackageManagerDirectories cached_github_folder_name). On sandboxed
  # builders bun then tries to download the tarball and fails with
  # FailedToOpenSocket. Add the exact entry bun expects, including the
  # `.bun-tag` marker bun writes when it extracts a GitHub tarball itself.
  #
  # Every cache entry must be a plain directory of regular files: bun's
  # copyfile backend silently installs empty packages from entries whose
  # files are symlinks — the shape symlinkJoin/lndir produces (and what
  # fetchBunDeps emits). bun run masks this by resolving modules straight
  # from the cache, so only direct node_modules consumers (CSS @import,
  # spawning node_modules/.bin) fail. Dereference everything with cp -RL.
  bunDeps =
    let
      base = bun2nix.fetchBunDeps {
        bunNix = ../web/bun.nix;
        overrides = {
          # Replace the generated (invalid npm-registry) fetch for the
          # github: dependency so it is never attempted; the usable cache
          # entry is added below.
          "@kenn-io/kit-ui@github:kenn-io/kit-ui#1e9dc7d" = _: kitUiSrc;
        };
      };
    in
    runCommand "msgvault-bun-deps" { } ''
      mkdir -p "$out/share/bun-cache"
      cp -RL ${base}/share/bun-cache/. "$out/share/bun-cache/"
      chmod -R u+w "$out/share/bun-cache"
      dest="$out/share/bun-cache/@GH@kenn-io-kit-ui-1e9dc7d@@@1"
      cp -R ${kitUiSrc} "$dest"
      chmod -R u+w "$dest"
      printf 'kenn-io-kit-ui-1e9dc7d' > "$dest/.bun-tag"
    '';
  bunRoot = "web";
  bunInstallFlags = [
    "--linker=hoisted"
    "--backend=copyfile"
  ];
  dontUseBunBuild = true;
  dontUseBunCheck = true;
  dontUseBunInstall = true;

  nativeBuildInputs = [
    bun2nix.hook
    nodejs
  ];
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
