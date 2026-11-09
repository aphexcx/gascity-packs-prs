#!/usr/bin/env bash
# Build from a committed snapshot; publishing is an explicit maintainer action.
set -euo pipefail

usage() {
  echo 'Usage: scripts/release-pack-binaries.sh {gchat|slack-full} [--commit SHA] [--publish]' >&2
}
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $# -gt 0 ]] || { usage; exit 2; }
pack=$1
shift
case "$pack" in
  gchat|slack-full) ;;
  *) usage; exit 2 ;;
esac
source_ref=HEAD
publish=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --commit)
      [[ $# -ge 2 && -n "$2" ]] || { usage; exit 2; }
      source_ref=$2
      shift 2
      ;;
    --publish) publish=1; shift ;;
    *) usage; exit 2 ;;
  esac
done

repo_dir=$(cd "$(dirname "$0")/.." && pwd)
commit=$(git -C "$repo_dir" rev-parse --verify --end-of-options "$source_ref^{commit}")
short_commit=$(git -C "$repo_dir" rev-parse --short=8 "$commit")
release_repo=aphexcx/gascity-packs-prs
go_bin=$(type -P go || true)
[[ -n "$go_bin" ]] || die 'Go is required to build release binaries'
# A relative PATH entry must survive the module-directory change below.
go_bin=$(cd "$(dirname "$go_bin")" && pwd)/$(basename "$go_bin")
if type -P sha256sum >/dev/null; then
  checksum=(sha256sum)
elif type -P shasum >/dev/null; then
  checksum=(shasum -a 256)
else
  die 'sha256sum or shasum is required'
fi

if (( publish )); then
  type -P gh >/dev/null || die 'gh is required for --publish'
  # Check both tags before building or publishing either architecture. A failed
  # API request is an error, never evidence that a tag is available.
  for arch in amd64 arm64; do
    release="$pack-linux-$arch-$short_commit"
    if git -C "$repo_dir" show-ref --verify --quiet "refs/tags/$release"; then
      die "refusing existing tag $release"
    fi
    existing=$(gh api "repos/$release_repo/git/matching-refs/tags/$release" \
      --jq ".[] | select(.ref == \"refs/tags/$release\") | .ref")
    [[ -z "$existing" ]] || die "refusing existing tag $release"
  done
fi

scratch=$(mktemp -d "${TMPDIR:-/tmp}/pack-release.XXXXXX")
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/source" "$scratch/assets"
git -C "$repo_dir" archive "$commit" "$pack" | tar -x -C "$scratch/source"
version=$(sed -n 's/^version = "\([^"]*\)".*/\1/p' "$scratch/source/$pack/pack.toml")
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.+-][a-zA-Z0-9.+-]+)?$ ]] || die "invalid pack version: $version"

case "$pack" in
  gchat)
    binaries=(gc-gchat-adapter)
    modules=(adapter)
    packages=(./cmd/gc-gchat-adapter)
    ;;
  slack-full)
    binaries=(gc-slack-adapter gc-slack-cli)
    modules=(adapter cli)
    packages=(. .)
    ;;
esac

printf 'Building %s version %s from %s\n' "$pack" "$version" "$commit" >&2
for arch in amd64 arm64; do
  release="$pack-linux-$arch-$short_commit"
  asset_dir="$scratch/assets/$release"
  mkdir -p "$asset_dir"
  for i in "${!binaries[@]}"; do
    binary=${binaries[$i]}
    (
      cd "$scratch/source/$pack/${modules[$i]}"
      CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOWORK=off GOFLAGS='' \
        "$go_bin" build -trimpath -ldflags "-s -w -X main.Version=$version" \
        -o "$asset_dir/$binary" "${packages[$i]}"
    )
    # Run beside the asset so the sidecar contains only its bare file name.
    (cd "$asset_dir" && "${checksum[@]}" "$binary" > "$binary.sha256")
  done
done

printf 'release\tasset\tsha256\tsize_bytes\n'
for arch in amd64 arm64; do
  release="$pack-linux-$arch-$short_commit"
  dest="$repo_dir/dist/$release"
  mkdir -p "$dest"
  assets=()
  for binary in "${binaries[@]}"; do
    cp "$scratch/assets/$release/$binary" "$scratch/assets/$release/$binary.sha256" "$dest/"
    read -r digest _ < "$dest/$binary.sha256"
    size=$(wc -c < "$dest/$binary" | tr -d '[:space:]')
    printf '%s\t%s\t%s\t%s\n' "$release" "$binary" "$digest" "$size"
    assets+=("$dest/$binary" "$dest/$binary.sha256")
  done
  if (( publish )); then
    gh release create "$release" --repo "$release_repo" --target "$commit" \
      --prerelease --title "$pack $version (linux/$arch)" \
      --notes "Pack version $version. Source commit: $commit. Static Linux binaries with SHA-256 sidecars." \
      "${assets[@]}"
  fi
done
