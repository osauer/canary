#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
fixture_parent="${repo_root}/internal/daemon/testdata"
fixture_root="${fixture_parent}/upgrades"
v171_commit="57e130a06381583730afcaff822e512d01395158"
v221_commit="f1b5647e39edef3a4bf88ba6bf0bb7a9cd4a1c4a"
v230_commit="22375a798272130d476d25b1c66ab1c84cf55e99"
v254_commit="3b548f6d63286448ac132ca4ade66484952612f5"

if [[ "$(git rev-parse 'v1.7.1^{}')" != "${v171_commit}" ]]; then
  echo "v1.7.1 no longer peels to the pinned commit" >&2
  exit 1
fi
if [[ "$(git rev-parse 'v2.2.1^{}')" != "${v221_commit}" ]]; then
  echo "v2.2.1 no longer peels to the pinned commit" >&2
  exit 1
fi
if [[ "$(git rev-parse 'v2.3.0^{}')" != "${v230_commit}" ]]; then
  echo "v2.3.0 no longer peels to the pinned commit" >&2
  exit 1
fi
if [[ "$(git rev-parse 'v2.5.4^{}')" != "${v254_commit}" ]]; then
  echo "v2.5.4 no longer peels to the pinned commit" >&2
  exit 1
fi

scratch="$(mktemp -d "${TMPDIR:-/tmp}/canary-upgrade-fixtures.XXXXXX")"
trap 'rm -rf "${scratch}"' EXIT
stage="${scratch}/upgrades"
mkdir -p "${stage}"

generate_from_tag() {
  local commit="$1"
  local source_dir="$2"
  local archive="${scratch}/${commit}.tar"
  git archive --format=tar --output="${archive}" "${commit}"
  mkdir -p "${source_dir}"
  tar -xf "${archive}" -C "${source_dir}"
}

v171_source="${scratch}/v1.7.1-source"
generate_from_tag "${v171_commit}" "${v171_source}"
cp "${repo_root}/scripts/upgrade-fixtures/generators/v1_7_1_fixture_test.go.txt" \
  "${v171_source}/internal/daemon/historical_fixture_generation_test.go"
(
  cd "${v171_source}"
  CANARY_UPGRADE_FIXTURE_OUTPUT="${stage}/v1.7.1-file-authority" \
    GOCACHE="${scratch}/go-build-v171" \
    go test ./internal/daemon -run '^TestGenerateHistoricalV171FileAuthority$' -count=1
)

v221_source="${scratch}/v2.2.1-source"
generate_from_tag "${v221_commit}" "${v221_source}"
cp "${repo_root}/scripts/upgrade-fixtures/generators/v2_2_1_fixture_test.go.txt" \
  "${v221_source}/internal/daemon/historical_fixture_generation_test.go"
(
  cd "${v221_source}"
  CANARY_UPGRADE_FIXTURE_OUTPUT="${stage}/v2.2.1-file-authority" \
    GOCACHE="${scratch}/go-build-v221" \
    go test ./internal/daemon -run '^TestGenerateHistoricalV221FileAuthority$' -count=1
)

v230_source="${scratch}/v2.3.0-source"
generate_from_tag "${v230_commit}" "${v230_source}"
cp "${repo_root}/scripts/upgrade-fixtures/generators/v2_3_0_core_fixture_test.go.txt" \
  "${v230_source}/internal/daemon/corestore/historical_fixture_generation_test.go"
cp "${repo_root}/scripts/upgrade-fixtures/generators/v2_3_0_head_fixture_test.go.txt" \
  "${v230_source}/internal/daemon/historical_fixture_generation_test.go"
mkdir -p "${stage}/v2.3.0-schema-v1-authority"
(
  cd "${v230_source}"
  CANARY_UPGRADE_FIXTURE_OUTPUT="${stage}/v2.3.0-schema-v1-authority/daemon.db" \
    GOCACHE="${scratch}/go-build-v230" \
    go test ./internal/daemon/corestore -run '^TestGenerateHistoricalV230CoreAuthority$' -count=1
  CANARY_UPGRADE_FIXTURE_OUTPUT="${stage}/v2.3.0-schema-v1-authority/daemon.db" \
    GOCACHE="${scratch}/go-build-v230" \
    go test ./internal/daemon -run '^TestGenerateHistoricalV230AuthorityHead$' -count=1
)

v254_source="${scratch}/v2.5.4-source"
generate_from_tag "${v254_commit}" "${v254_source}"
cp "${repo_root}/scripts/upgrade-fixtures/generators/v2_5_4_core_fixture_test.go.txt" \
  "${v254_source}/internal/daemon/corestore/historical_fixture_generation_test.go"
cp "${repo_root}/scripts/upgrade-fixtures/generators/v2_5_4_head_fixture_test.go.txt" \
  "${v254_source}/internal/daemon/historical_fixture_generation_test.go"
mkdir -p "${stage}/v2.5.4-schema-v3-authority"
(
  cd "${v254_source}"
  CANARY_UPGRADE_FIXTURE_OUTPUT="${stage}/v2.5.4-schema-v3-authority/daemon.db" \
    GOCACHE="${scratch}/go-build-v254" \
    go test ./internal/daemon/corestore -run '^TestGenerateHistoricalV254CoreAuthority$' -count=1
  CANARY_UPGRADE_FIXTURE_OUTPUT="${stage}/v2.5.4-schema-v3-authority/daemon.db" \
    GOCACHE="${scratch}/go-build-v254" \
    go test ./internal/daemon -run '^TestGenerateHistoricalV254AuthorityHead$' -count=1
)

(
  cd "${repo_root}"
  GOCACHE="${scratch}/go-build-manifest" go run ./scripts/upgrade-fixtures \
    -root "${stage}" \
    -v171-commit "${v171_commit}" \
    -v221-commit "${v221_commit}" \
    -v230-commit "${v230_commit}" \
    -v254-commit "${v254_commit}"
)

mkdir -p "${fixture_parent}"
if [[ -e "${fixture_root}" ]]; then
  mv "${fixture_root}" "${scratch}/previous-upgrades"
fi
mv "${stage}" "${fixture_root}"
echo "refreshed ${fixture_root}"
