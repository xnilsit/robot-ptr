#!/usr/bin/env bash
# Prints the vulnerabilities that affect the code of the last release (or the
# given ref) but no longer HEAD, and whose highest CVSS base score is at least
# MIN_CVSS (default 7.0). One line per vulnerability: <id> <score> <aliases>.
#
# govulncheck only reports vulnerabilities whose code is actually called. The
# Go vulnerability database has no scores, so they come from NVD for the CVE
# aliases and from GitHub for the GHSA aliases.
set -euo pipefail

min=${MIN_CVSS:-7.0}
base=${1:-$(git describe --tags --abbrev=0 --match '[0-9]*' 2>/dev/null || true)}
if [[ -z "$base" ]]; then
  echo "No release yet" >&2
  exit 0
fi

# Go version of a ref: the release image is built with the mise toolchain.
go_version() {
  git show "$1:.mise/config.toml" | sed -n 's/^go = "\(.*\)"$/\1/p'
}

# called_vulns <dir> <go version>
called_vulns() {
  (cd "$1" && GOTOOLCHAIN="go$2" govulncheck -format json ./...) |
    jq -r 'select(.finding.trace[0].function) | .finding.osv' | sort -u
}

# score <alias>...: highest CVSS base score of the CVE and GHSA aliases, 0 if
# none has one.
score() {
  local alias s max=0
  for alias in "$@"; do
    case $alias in
      CVE-*)
        s=$(curl -fsS --retry 5 --retry-all-errors --retry-delay 10 \
          "https://services.nvd.nist.gov/rest/json/cves/2.0?cveId=$alias" |
          jq '[.vulnerabilities[].cve.metrics | to_entries[] | select(.key | startswith("cvssMetric")) | .value[].cvssData.baseScore] | max // 0')
        ;;
      GHSA-*)
        s=$(gh api "/advisories/$alias" --jq '[.cvss.score, .cvss_severities[]?.score] | map(. // 0) | max')
        ;;
      *) continue ;;
    esac
    max=$(jq -n "[$max, $s] | max")
  done
  echo "$max"
}

worktree=$(mktemp -d)
trap 'git worktree remove --force "$worktree"' EXIT
git worktree add --quiet --detach "$worktree" "$base"

released=$(called_vulns "$worktree" "$(go_version "$base")")
current=$(called_vulns . "$(go_version HEAD)")

for id in $(comm -23 <(echo "$released") <(echo "$current")); do
  aliases=$(curl -fsS "https://api.osv.dev/v1/vulns/$id" | jq -r '.aliases // [] | join(",")')
  s=$(score ${aliases//,/ })
  if [[ $(jq -n "$s >= $min") == true ]]; then
    echo "$id $s $aliases"
  elif [[ $s == 0 ]]; then
    echo "$id has no CVSS score, not counted ($aliases)" >&2
  else
    echo "$id scores $s, below $min ($aliases)" >&2
  fi
done
