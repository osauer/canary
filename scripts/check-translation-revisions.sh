#!/bin/sh
# Every translation's source-revision must be a git blob: the hash of the
# English file it follows (git hash-object). A commit hash has the same shape,
# so the generator's format check passes it, and the page then renders as
# behind its English source for good; git tells the two apart. On 2026-11-07
# a commit hash marked the current Traditional Chinese Hong Kong and Singapore
# guide as behind.

set -eu

cd "$(git rev-parse --show-toplevel)"
status=0
for file in $(git grep -l '^source-revision:' -- docs/docs); do
	revision=$(sed -n 's/^source-revision: *//p' "$file")
	kind=$(git cat-file -t "$revision" 2>/dev/null || true)
	if [ "$kind" != blob ]; then
		printf '%s: source-revision %s is a git %s, not a blob: use git hash-object on the English source\n' \
			"$file" "$revision" "${kind:-object it does not know}" >&2
		status=1
	fi
done
exit "$status"
