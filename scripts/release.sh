#!/usr/bin/env bash
# Release a new version of aii.
#
# Bumps the version constant in cmd/aii/main.go, commits, tags, pushes,
# and creates a GitHub release with auto-generated notes from the
# commits since the previous tag.
#
# Usage:
#   scripts/release.sh patch        # 0.2.0 → 0.2.1
#   scripts/release.sh minor        # 0.2.0 → 0.3.0
#   scripts/release.sh major        # 0.2.0 → 1.0.0
#   scripts/release.sh 1.2.3        # explicit version
#   scripts/release.sh current      # tag the current in-code version as-is
#                                   # (useful for the first release on a fresh repo)

set -euo pipefail

usage() {
    sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# \?//'
}

die() { echo "error: $*" >&2; exit 1; }

if [[ $# -ne 1 ]]; then
    usage
    exit 1
fi

cd "$(git rev-parse --show-toplevel)"

VERSION_FILE="cmd/aii/main.go"
CHANGELOG="CHANGELOG.md"
CURRENT=$(sed -n 's/^const aiiVersion = "\(.*\)"$/\1/p' "$VERSION_FILE")
[[ -n "$CURRENT" ]] || die "can't find aiiVersion in $VERSION_FILE"

bump_component() {
    local kind="$1" major minor patch
    IFS=. read -r major minor patch <<<"$CURRENT"
    case "$kind" in
        major) echo "$((major+1)).0.0" ;;
        minor) echo "$major.$((minor+1)).0" ;;
        patch) echo "$major.$minor.$((patch+1))" ;;
        *) die "unknown bump kind: $kind" ;;
    esac
}

case "$1" in
    -h|--help)                     usage; exit 0 ;;
    major|minor|patch)             NEW=$(bump_component "$1") ;;
    current)                       NEW="$CURRENT" ;;
    *)
        [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "invalid arg: $1"
        NEW="$1"
        ;;
esac

TAG="v$NEW"

# --- guardrails ----------------------------------------------------------

[[ -z "$(git status --porcelain)" ]] || die "working tree not clean; commit or stash first"

BRANCH=$(git rev-parse --abbrev-ref HEAD)
[[ "$BRANCH" == "main" ]] || die "not on main (on $BRANCH)"

! git rev-parse --verify "$TAG" >/dev/null 2>&1 || die "tag $TAG already exists"

command -v gh >/dev/null || die "gh CLI not on PATH (install https://cli.github.com)"
gh auth status >/dev/null 2>&1 || die "gh not authenticated (run 'gh auth login')"

# Require a non-empty [Unreleased] section in CHANGELOG.md when bumping.
# `current` mode skips the check — it's used for the first release on a
# fresh repo where the file may not exist yet.
if [[ -f "$CHANGELOG" && "$NEW" != "$CURRENT" ]]; then
    UNRELEASED=$(awk '
        /^## \[Unreleased\]/ { in_section=1; next }
        /^## \[/             { in_section=0 }
        in_section && /[^[:space:]]/ { print }
    ' "$CHANGELOG")
    [[ -n "$UNRELEASED" ]] || die "CHANGELOG.md [Unreleased] section is empty — add an entry before releasing"
fi

# --- bump + build --------------------------------------------------------

if [[ "$NEW" != "$CURRENT" ]]; then
    echo "bump $CURRENT → $NEW"
    # Portable in-place rewrite (works on both macOS bsd sed and gnu sed).
    tmp=$(mktemp)
    sed "s|^const aiiVersion = \".*\"\$|const aiiVersion = \"$NEW\"|" "$VERSION_FILE" > "$tmp"
    mv "$tmp" "$VERSION_FILE"
    grep -q "^const aiiVersion = \"$NEW\"\$" "$VERSION_FILE" || die "rewrite failed"
else
    echo "tagging current version $CURRENT (no bump)"
fi

echo "running go build..."
go build ./... >/dev/null || die "build failed after bump"

# Stamp [Unreleased] → [NEW] - DATE in CHANGELOG.md and reset the
# Unreleased heading. Also rewrite the comparison-link footnotes.
if [[ -f "$CHANGELOG" && "$NEW" != "$CURRENT" ]]; then
    DATE=$(date -u +%Y-%m-%d)
    tmp=$(mktemp)
    awk -v new="$NEW" -v date="$DATE" -v current="$CURRENT" '
        /^## \[Unreleased\]/ && !stamped {
            print "## [Unreleased]"
            print ""
            print "## [" new "] - " date
            stamped=1
            next
        }
        /^\[Unreleased\]:/ {
            print "[Unreleased]: https://github.com/ericmason/aii/compare/v" new "...HEAD"
            print "[" new "]: https://github.com/ericmason/aii/compare/v" current "...v" new
            next
        }
        { print }
    ' "$CHANGELOG" > "$tmp"
    mv "$tmp" "$CHANGELOG"
fi

# --- commit, tag, push, release -----------------------------------------

if [[ "$NEW" != "$CURRENT" ]]; then
    git add "$VERSION_FILE"
    [[ -f "$CHANGELOG" ]] && git add "$CHANGELOG"
    git commit -m "Release $TAG"
fi

git tag -a "$TAG" -m "Release $TAG"
git push origin main
git push origin "$TAG"

gh release create "$TAG" --title "$TAG" --generate-notes

URL=$(gh release view "$TAG" --json url -q .url)
echo "✓ released $TAG → $URL"
