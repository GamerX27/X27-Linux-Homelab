#!/usr/bin/env bash
# Publishes a GitHub Release for a finished build (build.yml's release job), with notes on
# what changed since the previous release: key versions, repo commits, and per-image package
# changes. Package lists are attached to each release so the next one can diff against them.
#
# Versioning: the weekly scheduled build is vYYYY.MM.DD; every other build (push, dispatch)
# is a minor release vYYYY.MM.DD.N.
#
# Env: EVENT (github.event_name), GITHUB_REPOSITORY, GITHUB_SHA, GH_TOKEN.
# DRY_RUN=1 prints the tag and notes instead of publishing. PREV_DIR=<dir> uses package
# lists from <dir> instead of downloading them from the previous release.
set -euo pipefail

REGISTRY="ghcr.io/gamerx27"
FEDORA=44
EVENT="${EVENT:-push}"
REPO="${GITHUB_REPOSITORY:-GamerX27/X27-Linux-Homelab}"
SHA="${GITHUB_SHA:-$(git rev-parse HEAD)}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# name:image pairs, in the order they appear in the notes.
IMAGES=("bare-metal:x27-linux-homelab" "vm:x27-linux-homelab-vm")

# --- Tag -----------------------------------------------------------------------------
git fetch --tags --quiet 2>/dev/null || true
DAY="$(date -u +%Y.%m.%d)"
if [[ "$EVENT" == schedule ]] && ! git rev-parse -q --verify "refs/tags/v${DAY}" >/dev/null; then
  TAG="v${DAY}"
else
  LAST_N="$(git tag -l "v${DAY}.*" | sed "s/^v${DAY}\.//" | grep -E '^[0-9]+$' | sort -n | tail -n1 || true)"
  TAG="v${DAY}.$(( ${LAST_N:-0} + 1 ))"
fi
TITLE="X27-Linux Homelab ${TAG#v}"

# --- Previous release ----------------------------------------------------------------
PREV_TAG="$(gh release list -R "$REPO" --exclude-drafts --limit 1 --json tagName --jq '.[0].tagName // ""' 2>/dev/null || true)"
PREV_DIR="${PREV_DIR:-}"
if [[ -z "$PREV_DIR" && -n "$PREV_TAG" ]]; then
  PREV_DIR="${WORK}/prev"
  mkdir -p "$PREV_DIR"
  gh release download "$PREV_TAG" -R "$REPO" -p 'packages-*.txt' -D "$PREV_DIR" 2>/dev/null || true
fi

# --- Package lists of this build -----------------------------------------------------
mkdir -p "${WORK}/new"
for entry in "${IMAGES[@]}"; do
  name="${entry%%:*}"
  image="${entry#*:}"
  # This commit's build; falls back to :44 when the image was skipped (single-recipe dispatch).
  ref="${REGISTRY}/${image}:${SHA::7}-${FEDORA}"
  docker pull -q "$ref" >/dev/null 2>&1 || ref="${REGISTRY}/${image}:${FEDORA}"
  docker run --rm --entrypoint rpm "$ref" -qa --qf '%{NAME} %{EVR}\n' \
    | grep -v '^gpg-pubkey ' | sort -u >"${WORK}/new/packages-${name}.txt"
done

# "name old new" per changed package: updated / added (old "-") / removed (new "-").
# Names with several installed versions (e.g. kernels) are compared as one joined list.
diff_packages() {
  awk '
    NR == FNR { old[$1] = ($1 in old) ? old[$1] "," $2 : $2; next }
              { new[$1] = ($1 in new) ? new[$1] "," $2 : $2 }
    END {
      for (n in new) if (!(n in old)) print "added", n, "-", new[n]
                     else if (old[n] != new[n]) print "updated", n, old[n], new[n]
      for (n in old) if (!(n in new)) print "removed", n, old[n], "-"
    }' "$1" "$2" | sort -k2
}

version_of() {
  awk -v p="$1" '$1 == p { v = (v ? v "," : "") $2 } END { print v }' "$2"
}

# --- Notes ---------------------------------------------------------------------------
NOTES="${WORK}/notes.md"
BM_NEW="${WORK}/new/packages-bare-metal.txt"
BM_OLD="${PREV_DIR:+${PREV_DIR}/packages-bare-metal.txt}"
{
  if [[ "$EVENT" == schedule ]]; then
    echo "Weekly rebuild: picks up the latest Fedora ${FEDORA} and Docker CE updates."
  else
    echo "Minor release: repo changes since the last release."
  fi
  echo

  echo "## Highlights"
  echo
  echo "| | Version |"
  echo "|---|---|"
  for pkg in fedora-release-common kernel-core systemd docker-ce containerd.io bootc rpm-ostree; do
    new="$(version_of "$pkg" "$BM_NEW")"
    [[ -n "$new" ]] || continue
    old=""
    [[ -n "$BM_OLD" && -f "$BM_OLD" ]] && old="$(version_of "$pkg" "$BM_OLD")"
    if [[ -n "$old" && "$old" != "$new" ]]; then
      echo "| \`${pkg}\` | ${old} → **${new}** |"
    else
      echo "| \`${pkg}\` | ${new} |"
    fi
  done
  echo

  echo "## Repo changes"
  echo
  if [[ -n "$PREV_TAG" ]] && git rev-parse -q --verify "refs/tags/${PREV_TAG}" >/dev/null; then
    range="${PREV_TAG}..${SHA}"
  else
    range="$SHA"
  fi
  commits="$(git log --no-merges --format="- [\`%h\`](https://github.com/${REPO}/commit/%H) %s" "$range")"
  echo "${commits:-No repo changes; package updates only.}"
  echo

  echo "## Package changes"
  echo
  for entry in "${IMAGES[@]}"; do
    name="${entry%%:*}"
    image="${entry#*:}"
    new="${WORK}/new/packages-${name}.txt"
    old="${PREV_DIR:+${PREV_DIR}/packages-${name}.txt}"
    echo "### ${name} (\`${image}\`)"
    echo
    if [[ -z "$old" || ! -f "$old" ]]; then
      echo "$(wc -l <"$new") packages. No previous release to compare with."
      echo
      continue
    fi
    changes="$(diff_packages "$old" "$new")"
    updated="$(grep -c '^updated ' <<<"$changes" || true)"
    added="$(grep -c '^added ' <<<"$changes" || true)"
    removed="$(grep -c '^removed ' <<<"$changes" || true)"
    echo "${updated} updated, ${added} added, ${removed} removed ($(wc -l <"$new") packages)."
    echo
    if [[ -n "$changes" ]]; then
      echo "<details><summary>Package list</summary>"
      echo
      echo "| Package | Change |"
      echo "|---|---|"
      while read -r kind pkg from to; do
        case "$kind" in
          updated) echo "| \`${pkg}\` | ${from} → ${to} |" ;;
          added)   echo "| \`${pkg}\` | added ${to} |" ;;
          removed) echo "| \`${pkg}\` | removed (was ${from}) |" ;;
        esac
      done <<<"$changes"
      echo
      echo "</details>"
      echo
    fi
  done

  echo "## Images"
  echo
  for entry in "${IMAGES[@]}"; do
    echo "- \`${REGISTRY}/${entry#*:}:${FEDORA}\` (this build: \`${SHA::7}-${FEDORA}\`)"
  done
  echo
  echo "Update with \`sudo rpm-ostree upgrade\`, or let \`autoupdate\` do it."
} >"$NOTES"

if [[ "${DRY_RUN:-0}" == 1 ]]; then
  echo "TAG=${TAG}"
  echo "TITLE=${TITLE}"
  echo "PREV_TAG=${PREV_TAG:-<none>}"
  echo "-----"
  cat "$NOTES"
  exit 0
fi

gh release create "$TAG" -R "$REPO" --target "$SHA" --title "$TITLE" --notes-file "$NOTES" --latest \
  "${WORK}"/new/packages-*.txt
echo "Published ${TAG}"
