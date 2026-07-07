#!/usr/bin/env bash
# Commit the given files onto the PR's head branch through GitHub's GraphQL
# createCommitOnBranch mutation. Commits created via the API are signed by
# GitHub and show as "Verified" — which the branch's signed-commits ruleset
# requires. The plain `git commit` + `git push` we used before lands an
# *unsigned* commit (authored by github-actions[bot]) that blocks the PR from
# merging.
#
# Usage: commit-signed.sh "<commit message>" <file> [<file> ...]
# Prints the new commit's SHA on stdout; progress goes to stderr.
#
# Requires in the environment:
#   GH_TOKEN  a token with contents:write on the repo (the workflow's
#             GITHUB_TOKEN; fork PRs get a read-only token and must not call us)
#   REPO      owner/name of the repository
#   BRANCH    the head branch to commit onto (github.head_ref)
#
# The listed files must already exist in the working tree with the desired
# content. Retries on the optimistic-locking race where a concurrent job
# (e.g. the e2e screenshot push vs. the unit-test badge push) committed first
# and our expectedHeadOid went stale.
set -euo pipefail

msg=$1
shift
if [ "$#" -eq 0 ]; then
  echo "::error::commit-signed.sh: no files given" >&2
  exit 2
fi

# fileChanges.additions wants {path, base64-encoded contents} per file; a single
# additions entry covers both new and modified files. The base64 of each file is
# read into jq with --rawfile (from a process substitution), never as a command
# argument: a large file (e.g. a screenshot PNG) base64-encodes to well past a
# single argument's MAX_ARG_STRLEN limit, which is what made jq abort with
# "Argument list too long". The combined array is written to a temp file so the
# body step below can slurp it in the same way rather than passing it on argv.
additions_file=$(mktemp)
trap 'rm -f "$additions_file"' EXIT
{
  for f in "$@"; do
    jq -nc --arg path "$f" --rawfile contents <(base64 -w0 "$f") \
      '{path: $path, contents: $contents}'
  done | jq -sc .
} >"$additions_file"

query='mutation($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) { commit { oid } }
}'

for attempt in 1 2 3 4 5; do
  head=$(gh api "repos/$REPO/branches/$BRANCH" --jq .commit.sha)
  # --slurpfile reads the additions array from the file (a single JSON value, so it
  # arrives wrapped as $additions[0]); this keeps the large base64 payload off the
  # command line here too.
  body=$(jq -nc \
    --arg repo "$REPO" --arg branch "$BRANCH" --arg oid "$head" \
    --arg msg "$msg" --slurpfile additions "$additions_file" --arg query "$query" \
    '{query: $query,
      variables: {input: {
        branch: {repositoryNameWithOwner: $repo, branchName: $branch},
        message: {headline: $msg},
        fileChanges: {additions: $additions[0]},
        expectedHeadOid: $oid}}}')
  if resp=$(printf '%s' "$body" | gh api graphql --input -); then
    oid=$(printf '%s' "$resp" | jq -r '.data.createCommitOnBranch.commit.oid')
    echo "committed onto $BRANCH (was $head): $msg -> $oid" >&2
    printf '%s\n' "$oid"
    exit 0
  fi
  echo "createCommitOnBranch failed (attempt $attempt/5), retrying in 5s..." >&2
  sleep 5
done

echo "::error::failed to create signed commit on $BRANCH after 5 attempts" >&2
exit 1
