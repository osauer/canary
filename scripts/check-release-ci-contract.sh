#!/bin/sh

# Keep the checked-in release CI contract synchronized with every repo-owned
# workflow GitHub can start for a push to main. The release waiter consumes
# that manifest as one snapshot, so malformed trigger syntax, manifest drift,
# or a second/per-workflow waiter invocation must fail before tagging.

set -eu

authority_only=0
if [ "${1:-}" = "--authority-only" ]; then
	authority_only=1
	shift
fi
case "$#" in
	0) root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd) ;;
	1) root=$1 ;;
	*)
		echo "usage: $0 [--authority-only] [repo-root]" >&2
		exit 2
		;;
esac

if ! command -v python3 >/dev/null 2>&1; then
	echo "check-release-ci-contract: python3 is required" >&2
	exit 1
fi

exec python3 - "$root" "$authority_only" <<'PY'
import fnmatch
import json
import re
import shlex
import sys
from pathlib import Path


CONTRACT_PATH = "scripts/release-ci-contract.json"
LEGACY_CONTRACT_PATH = "scripts/release-ci-legacy-contracts.json"
REGISTRY_WORKFLOW_PATH = ".github/workflows/registry-publish.yml"
EXPECTED_REPOSITORY = "osauer/canary"
EXPECTED_CI_RUN_STEPS = {
    "check": {
        "make check": {
            "run": "make check CHECK_DEPS=parity-check",
        },
    },
    "test": {
        "make test-pkg": {
            "run": "make test-pkg",
        },
        "make test-support (-race; command and CI/release helpers)": {
            "run": "make test-support",
        },
        "make test-daemon (sharded -race)": {
            "run": "make test-daemon",
        },
    },
    "app-render": {
        "npm ci": {
            "working-directory": "web/app",
            "run": "npm ci",
        },
        "install Chromium": {
            "working-directory": "web/app",
            "run": "npx playwright install --with-deps chromium",
        },
        "make app-render-check": {
            "run": "make app-render-check",
        },
    },
}
EXPECTED_LEGACY_SHA = "3b548f6d63286448ac132ca4ade66484952612f5"
CHECKOUT_ACTION = "actions/checkout@11d5960a326750d5838078e36cf38b85af677262"
SETUP_GO_ACTION = "actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff"
PUBLISHER_SHA256 = "ab128162b0616090b47cf245afe0a23f3ef08936fdce19074f5ba0a4469281ac"
EXPECTED_WORKFLOWS = {
    "ci.yml": {
        "name": "ci",
        "jobs": (
            "make check (lint + vet + vulncheck + parity)",
            "make test (ubuntu-latest)",
            "isolated Canary app render",
            "cross-compile release matrix",
        ),
    },
    "pages-check.yml": {
        "name": "pages check",
        "jobs": ("local page targets",),
    },
}
EXPECTED_V254_LEGACY_CONTRACT = {
    "repository": "osauer/canary",
    "workflows": [
        {
            "file": "ci.yml",
            "name": "ci",
            "jobs": [
                "make check (lint + vet + vulncheck + parity)",
                "make test (ubuntu-latest)",
                "make test (macos-latest)",
                "cross-compile release matrix",
            ],
        },
        {
            "file": "pages-check.yml",
            "name": "pages check",
            "jobs": ["local page targets"],
        },
    ],
}
PUSH_FILTER_KEYS = {
    "branches",
    "branches-ignore",
    "tags",
    "tags-ignore",
    "paths",
    "paths-ignore",
}
EXPECTED_REGISTRY_STEPS = (
    "Resolve tag",
    "Checkout recovery controller",
    "Set up Go",
    "Hydrate and verify exact release asset set",
    "Verify exact release authority",
    "Install mcp-publisher",
    "Generate and validate dist/server.json",
    "Publish via OIDC",
)
EXPECTED_REGISTRY_COMMANDS = {
    "Resolve tag": (
        'set -euo pipefail '
        'tag="$RELEASE_TAG" '
        'if ! [[ "$tag" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+'
        '(-[A-Za-z0-9.-]+)?$ ]]; then '
        'echo "unexpected tag \'$tag\'" >&2 '
        "exit 1 "
        "fi "
        "printf 'tag=%s\\n' \"$tag\" >> \"$GITHUB_OUTPUT\""
    ),
    "Hydrate and verify exact release asset set": (
        "set -euo pipefail "
        'make release-github-assets RELEASE_VERSION="$RELEASE_TAG"'
    ),
    "Verify exact release authority": (
        "set -euo pipefail "
        'tag="$RELEASE_TAG" '
        'release_sha="$(git rev-parse "refs/tags/${tag}^{commit}")" '
        'contract="$(mktemp "${RUNNER_TEMP}/canary-release-ci-contract.XXXXXX")" '
        "trap 'rm -f \"$contract\"' EXIT HUP INT TERM "
        './scripts/check-release-source.sh --controller "$tag" '
        "./scripts/check-release-origin.sh "
        "./scripts/check-release-ci-contract.sh "
        'python3 ./scripts/materialize-release-ci-contract.py "$tag" "$contract" '
        "GOFLAGS= go run ./scripts/release-ci-wait "
        '-contract "$contract" -historical '
        '-sha "$release_sha" -branch main -event push '
        "-poll 15s -timeout 30m "
        './scripts/check-release-tag.sh "$tag" '
        './scripts/check-release-tag.sh --plugin "$tag" '
        './scripts/check-github-release.sh "$tag" dist'
    ),
    "Install mcp-publisher": (
        "set -euo pipefail "
        'archive="${RUNNER_TEMP}/mcp-publisher_linux_amd64.tar.gz" '
        "mkdir -p bin "
        'gh release download "$MCP_PUBLISHER_VERSION" '
        "--repo github.com/modelcontextprotocol/registry "
        "--pattern 'mcp-publisher_linux_amd64.tar.gz' "
        '--dir "$RUNNER_TEMP" '
        "printf '%s %s\\n' "
        '"$MCP_PUBLISHER_LINUX_AMD64_SHA256" "$archive" '
        "| sha256sum --check --strict - "
        'tar -xzf "$archive" '
        "-C bin mcp-publisher "
        "bin/mcp-publisher --version"
    ),
    "Generate and validate dist/server.json": (
        "make release-registry-server "
        'RELEASE_VERSION="$RELEASE_TAG"'
    ),
    "Publish via OIDC": (
        "set -euo pipefail "
        "bin/mcp-publisher login github-oidc "
        "MCP_REGISTRY_AUTO_LOGIN=0 "
        "./scripts/registry-publish-with-login.sh "
        "bin/mcp-publisher dist/server.json"
    ),
}


class ContractError(Exception):
    pass


class DuplicateJSONKey(Exception):
    pass


def fail(message):
    raise ContractError(message)


def unique_json_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise DuplicateJSONKey(key)
        result[key] = value
    return result


def load_manifest(root, enforce_current):
    path = root / CONTRACT_PATH
    try:
        text = path.read_bytes().decode("utf-8")
    except (OSError, UnicodeDecodeError) as error:
        fail(f"{path}: cannot read UTF-8 contract: {error}")
    try:
        document = json.loads(text, object_pairs_hook=unique_json_object)
    except DuplicateJSONKey as error:
        fail(f"{path}: duplicate JSON key {error.args[0]!r}")
    except json.JSONDecodeError as error:
        fail(f"{path}:{error.lineno}:{error.colno}: malformed JSON: {error.msg}")

    if type(document) is not dict:
        fail(f"{path}: top-level contract must be an object")
    expected_top_keys = {"repository", "workflows"}
    if set(document) != expected_top_keys:
        fail(
            f"{path}: top-level keys must be exactly "
            f"{sorted(expected_top_keys)!r}, got {sorted(document)!r}"
        )
    if document["repository"] != EXPECTED_REPOSITORY:
        fail(
            f"{path}: repository must be {EXPECTED_REPOSITORY!r}, "
            f"got {document['repository']!r}"
        )

    workflows = document["workflows"]
    if type(workflows) is not list:
        fail(f"{path}: workflows must be an array")

    entries = {}
    names = set()
    for index, entry in enumerate(workflows):
        context = f"{path}: workflows[{index}]"
        if type(entry) is not dict:
            fail(f"{context} must be an object")
        expected_entry_keys = {"file", "name", "jobs"}
        if set(entry) != expected_entry_keys:
            fail(
                f"{context} keys must be exactly "
                f"{sorted(expected_entry_keys)!r}, got {sorted(entry)!r}"
            )
        if type(entry["file"]) is not str or not entry["file"]:
            fail(f"{context}.file must be a non-empty string")
        if type(entry["name"]) is not str or not entry["name"]:
            fail(f"{context}.name must be a non-empty string")
        if type(entry["jobs"]) is not list or not entry["jobs"]:
            fail(f"{context}.jobs must be a non-empty array")
        if any(type(job) is not str or not job for job in entry["jobs"]):
            fail(f"{context}.jobs must contain only non-empty strings")
        if len(set(entry["jobs"])) != len(entry["jobs"]):
            fail(f"{context}.jobs contains a duplicate job")
        if entry["file"] in entries:
            fail(f"{context}: duplicate workflow file {entry['file']!r}")
        if entry["name"] in names:
            fail(f"{context}: duplicate workflow name {entry['name']!r}")
        names.add(entry["name"])
        entries[entry["file"]] = {
            "name": entry["name"],
            "jobs": tuple(entry["jobs"]),
        }

    if enforce_current:
        if set(entries) != set(EXPECTED_WORKFLOWS):
            missing = sorted(set(EXPECTED_WORKFLOWS) - set(entries))
            unexpected = sorted(set(entries) - set(EXPECTED_WORKFLOWS))
            fail(
                f"{path}: workflow files differ from the release authority; "
                f"missing={missing!r}, unexpected={unexpected!r}"
            )
        for workflow, expected in EXPECTED_WORKFLOWS.items():
            actual = entries[workflow]
            if actual != expected:
                fail(
                    f"{path}: {workflow} contract mismatch; "
                    f"expected={expected!r}, got={actual!r}"
                )
    return entries


def validate_legacy_contract(root):
    path = root / LEGACY_CONTRACT_PATH
    try:
        text = path.read_bytes().decode("utf-8")
    except (OSError, UnicodeDecodeError) as error:
        fail(f"{path}: cannot read UTF-8 legacy contracts: {error}")
    try:
        document = json.loads(text, object_pairs_hook=unique_json_object)
    except DuplicateJSONKey as error:
        fail(f"{path}: duplicate JSON key {error.args[0]!r}")
    except json.JSONDecodeError as error:
        fail(f"{path}:{error.lineno}:{error.colno}: malformed JSON: {error.msg}")

    expected = {EXPECTED_LEGACY_SHA: EXPECTED_V254_LEGACY_CONTRACT}
    if document != expected:
        fail(
            f"{path}: must contain only the exact v2.5.4 SHA-keyed CI contract"
        )


def strip_yaml_comment(text, context):
    """Strip comments for the deliberately narrow workflow-trigger subset."""
    quote = None
    escaped = False
    index = 0
    while index < len(text):
        char = text[index]
        if quote == '"':
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                quote = None
        elif quote == "'":
            if char == "'" and index + 1 < len(text) and text[index + 1] == "'":
                index += 1
            elif char == "'":
                quote = None
        else:
            begins_scalar = (
                index == 0
                or text[index - 1].isspace()
                or text[index - 1] in ":[,-"
            )
            if char in ("'", '"') and begins_scalar:
                quote = char
            elif char == "#" and (index == 0 or text[index - 1].isspace()):
                return text[:index].rstrip()
        index += 1
    if quote is not None:
        fail(f"{context}: unterminated quoted scalar")
    return text.rstrip()


def yaml_mapping_entry(text, context):
    text = strip_yaml_comment(text, context).strip()
    match = re.fullmatch(r"([A-Za-z_][A-Za-z0-9_-]*):(?:[ ]*(.*))?", text)
    if not match:
        fail(f"{context}: unsupported or malformed mapping entry")
    return match.group(1), (match.group(2) or "").strip()


def yaml_scalar(value, context):
    value = strip_yaml_comment(value, context).strip()
    if not value:
        fail(f"{context}: empty scalar")
    if value.startswith('"'):
        try:
            parsed = json.loads(value)
        except json.JSONDecodeError as error:
            fail(f"{context}: invalid double-quoted scalar: {error.msg}")
        if type(parsed) is not str:
            fail(f"{context}: expected a string scalar")
        return parsed
    if value.startswith("'"):
        if len(value) < 2 or not value.endswith("'"):
            fail(f"{context}: invalid single-quoted scalar")
        return value[1:-1].replace("''", "'")
    if value[0] in "!&*{}[]," or value[-1] in "{}[],":
        fail(f"{context}: unsupported plain scalar")
    return value


def yaml_flow_sequence(value, context):
    value = strip_yaml_comment(value, context).strip()
    if len(value) < 2 or value[0] != "[" or value[-1] != "]":
        fail(f"{context}: expected a complete flow sequence")
    body = value[1:-1]
    if not body.strip():
        return []

    items = []
    start = 0
    quote = None
    escaped = False
    index = 0
    while index < len(body):
        char = body[index]
        if quote == '"':
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                quote = None
        elif quote == "'":
            if char == "'" and index + 1 < len(body) and body[index + 1] == "'":
                index += 1
            elif char == "'":
                quote = None
        elif char in ("'", '"'):
            quote = char
        elif char in "[]{}":
            fail(f"{context}: nested flow collections are unsupported")
        elif char == ",":
            items.append(yaml_scalar(body[start:index], context))
            start = index + 1
        index += 1
    if quote is not None:
        fail(f"{context}: unterminated quoted sequence item")
    items.append(yaml_scalar(body[start:], context))
    return items


def workflow_yaml_lines(path):
    try:
        raw = path.read_bytes().decode("utf-8")
    except (OSError, UnicodeDecodeError) as error:
        fail(f"{path}: cannot read UTF-8 workflow: {error}")
    if "\x00" in raw:
        fail(f"{path}: NUL byte in workflow")

    lines = []
    for number, original in enumerate(raw.splitlines(), 1):
        if "\t" in original:
            fail(f"{path}:{number}: tabs are unsupported in workflow YAML")
        content = original.lstrip(" ")
        if not content or content.startswith("#"):
            continue
        indent = len(original) - len(content)
        if indent == 0 and content in ("---", "..."):
            fail(f"{path}:{number}: multiple-document YAML is unsupported")
        lines.append((number, indent, content))
    return lines


def top_level_yaml(path, lines):
    entries = {}
    order = []
    for index, (number, indent, content) in enumerate(lines):
        if indent != 0:
            continue
        key, value = yaml_mapping_entry(content, f"{path}:{number}")
        if key in entries:
            fail(f"{path}:{number}: duplicate top-level key {key!r}")
        entries[key] = (index, number, value)
        order.append((index, key))

    for required in ("name", "on", "jobs"):
        if required not in entries:
            fail(f"{path}: missing top-level {required!r} key")
    yaml_scalar(entries["name"][2], f"{path}:{entries['name'][1]} name")
    if entries["on"][2]:
        fail(
            f"{path}:{entries['on'][1]}: inline top-level 'on' syntax is "
            "unsupported; use an event mapping"
        )
    if entries["jobs"][2]:
        fail(
            f"{path}:{entries['jobs'][1]}: inline top-level 'jobs' syntax is "
            "unsupported"
        )
    return entries, order


def yaml_nested_block(lines, order, top_index):
    end = len(lines)
    for candidate, _key in order:
        if candidate > top_index:
            end = candidate
            break
    return lines[top_index + 1 : end]


def workflow_declared_surface(path):
    lines = workflow_yaml_lines(path)
    entries, order = top_level_yaml(path, lines)
    display_name = yaml_scalar(
        entries["name"][2], f"{path}:{entries['name'][1]} name"
    )
    jobs_index, _jobs_number, _jobs_value = entries["jobs"]
    jobs_block = yaml_nested_block(lines, order, jobs_index)
    if not jobs_block:
        fail(f"{path}: jobs mapping is empty")

    job_blocks = {}
    current_job = None
    for number, indent, content in jobs_block:
        if indent == 2:
            job_id, value = yaml_mapping_entry(content, f"{path}:{number}")
            if value:
                fail(f"{path}:{number}: inline job definitions are unsupported")
            if job_id in job_blocks:
                fail(f"{path}:{number}: duplicate job id {job_id!r}")
            job_blocks[job_id] = [number, []]
            current_job = job_id
        elif indent > 2:
            if current_job is None:
                fail(f"{path}:{number}: job child appears before a job id")
            job_blocks[current_job][1].append((number, indent, content))
        else:
            fail(f"{path}:{number}: malformed jobs indentation")

    expanded_names = []
    for job_id, (job_number, block) in job_blocks.items():
        direct = {}
        direct_order = []
        current_key = None
        for number, indent, content in block:
            if indent == 4:
                key, value = yaml_mapping_entry(content, f"{path}:{number}")
                if key in direct:
                    fail(
                        f"{path}:{number}: duplicate {job_id}.{key} job key"
                    )
                direct[key] = [number, value, []]
                direct_order.append(key)
                current_key = key
            elif indent > 4:
                if current_key is None:
                    fail(f"{path}:{number}: malformed {job_id} job block")
                direct[current_key][2].append((number, indent, content))
            else:
                fail(f"{path}:{number}: malformed {job_id} job indentation")

        if "name" not in direct:
            fail(f"{path}:{job_number}: job {job_id!r} must declare a name")
        if direct["name"][2]:
            fail(f"{path}:{direct['name'][0]}: job name cannot be nested")
        name = yaml_scalar(
            direct["name"][1], f"{path}:{direct['name'][0]} {job_id}.name"
        )
        if "if" in direct:
            fail(
                f"{path}:{direct['if'][0]}: conditional jobs are unsupported "
                "by the exact release contract"
            )

        matrix_token = "${{ matrix.os }}"
        has_expression = "${{" in name
        if has_expression and name.count(matrix_token) != 1:
            fail(
                f"{path}:{direct['name'][0]}: unsupported dynamic job name "
                f"{name!r}"
            )
        if not has_expression:
            if "strategy" in direct and any(
                indent == 6
                and yaml_mapping_entry(content, f"{path}:{number}")[0]
                == "matrix"
                for number, indent, content in direct["strategy"][2]
            ):
                fail(
                    f"{path}:{direct['strategy'][0]}: matrix job {job_id!r} "
                    "must expose its exact matrix value in the job name"
                )
            expanded_names.append(name)
            continue

        if "strategy" not in direct or direct["strategy"][1]:
            fail(
                f"{path}:{direct['name'][0]}: dynamic job name requires a "
                "nested strategy matrix"
            )
        strategy_children = direct["strategy"][2]
        matrix_children = None
        current_strategy_key = None
        for number, indent, content in strategy_children:
            if indent == 6:
                key, value = yaml_mapping_entry(content, f"{path}:{number}")
                current_strategy_key = key
                if key == "matrix":
                    if value:
                        fail(f"{path}:{number}: inline matrix is unsupported")
                    matrix_children = []
            elif indent > 6 and current_strategy_key == "matrix":
                matrix_children.append((number, indent, content))
        if matrix_children is None:
            fail(f"{path}:{direct['strategy'][0]}: matrix block is missing")

        axes = {}
        for number, indent, content in matrix_children:
            if indent != 8:
                fail(f"{path}:{number}: nested matrix features are unsupported")
            key, value = yaml_mapping_entry(content, f"{path}:{number}")
            if key in axes:
                fail(f"{path}:{number}: duplicate matrix axis {key!r}")
            axes[key] = yaml_filter_list(value, [], path, number, key)
        if set(axes) != {"os"}:
            fail(
                f"{path}:{direct['strategy'][0]}: exact release job expansion "
                f"supports only matrix.os, got {sorted(axes)!r}"
            )
        expanded_names.extend(name.replace(matrix_token, os_name) for os_name in axes["os"])

    if len(set(expanded_names)) != len(expanded_names):
        fail(f"{path}: expanded workflow job names are not unique")
    return display_name, tuple(expanded_names)


def yaml_filter_list(value, nested, path, number, key):
    context = f"{path}:{number} push.{key}"
    if value:
        if nested:
            fail(f"{context}: flow value cannot also have a nested block")
        values = yaml_flow_sequence(value, context)
    else:
        if not nested:
            fail(f"{context}: empty filter list")
        values = []
        for child_number, indent, content in nested:
            if indent != 6:
                fail(
                    f"{path}:{child_number}: {key} entries must use "
                    "six-space list indentation"
                )
            item = strip_yaml_comment(
                content, f"{path}:{child_number}"
            ).strip()
            match = re.fullmatch(r"-[ ]+(.+)", item)
            if not match:
                fail(f"{path}:{child_number}: malformed {key} list entry")
            values.append(
                yaml_scalar(match.group(1), f"{path}:{child_number}")
            )
    if not values:
        fail(f"{context}: filter list must not be empty")
    return values


def pattern_matches_main(pattern, context, allow_negative):
    negative = pattern.startswith("!")
    if negative:
        if not allow_negative:
            fail(f"{context}: negative patterns are not allowed here")
        pattern = pattern[1:]
    if not pattern:
        fail(f"{context}: empty branch pattern")
    if any(character in pattern for character in "\\+@(){}|"):
        fail(f"{context}: unsupported branch pattern {pattern!r}")
    if pattern.count("[") != pattern.count("]"):
        fail(f"{context}: malformed branch pattern {pattern!r}")
    return negative, fnmatch.fnmatchcase("main", pattern)


def branches_include_main(patterns, context):
    included = False
    positive_seen = False
    for pattern in patterns:
        negative, matches = pattern_matches_main(pattern, context, True)
        if not negative:
            positive_seen = True
        if matches:
            included = not negative
    if not positive_seen:
        fail(f"{context}: branches requires at least one positive pattern")
    return included


def branches_ignore_main(patterns, context):
    for pattern in patterns:
        _negative, matches = pattern_matches_main(pattern, context, False)
        if matches:
            return True
    return False


def push_includes_main(path, event_number, event_value, nested):
    if event_value:
        if nested:
            fail(
                f"{path}:{event_number}: inline push config cannot also have "
                "a nested block"
            )
        if event_value in ("{}", "null", "~"):
            return True
        fail(
            f"{path}:{event_number}: unsupported inline push config; "
            "use a mapping"
        )

    children = {}
    child_order = []
    current = None
    for number, indent, content in nested:
        if indent == 4:
            key, value = yaml_mapping_entry(content, f"{path}:{number}")
            if key not in PUSH_FILTER_KEYS:
                fail(f"{path}:{number}: unsupported push filter {key!r}")
            if key in children:
                fail(f"{path}:{number}: duplicate push filter {key!r}")
            children[key] = [number, value, []]
            child_order.append(key)
            current = key
        elif indent > 4:
            if current is None:
                fail(f"{path}:{number}: push child appears before a filter")
            children[current][2].append((number, indent, content))
        else:
            fail(f"{path}:{number}: malformed push event indentation")

    parsed = {}
    for key in child_order:
        number, value, child_lines = children[key]
        parsed[key] = yaml_filter_list(value, child_lines, path, number, key)

    if "paths" in parsed or "paths-ignore" in parsed:
        fail(
            f"{path}:{event_number}: path-filtered push workflows are "
            "candidate-dependent and unsupported by the release CI contract"
        )

    for left, right in (
        ("branches", "branches-ignore"),
        ("tags", "tags-ignore"),
        ("paths", "paths-ignore"),
    ):
        if left in parsed and right in parsed:
            fail(
                f"{path}:{event_number}: {left} and {right} are "
                "mutually exclusive"
            )

    if "branches" in parsed:
        return branches_include_main(
            parsed["branches"], f"{path}:{children['branches'][0]} branches"
        )
    if "branches-ignore" in parsed:
        return not branches_ignore_main(
            parsed["branches-ignore"],
            f"{path}:{children['branches-ignore'][0]} branches-ignore",
        )
    if "tags" in parsed or "tags-ignore" in parsed:
        # GitHub does not run a branch push when only tag filters are defined.
        return False
    return True


def workflow_pushes_main(path):
    lines = workflow_yaml_lines(path)
    entries, order = top_level_yaml(path, lines)
    on_index, _on_number, _on_value = entries["on"]
    block = yaml_nested_block(lines, order, on_index)
    if not block:
        fail(f"{path}: top-level 'on' mapping is empty")

    events = {}
    current = None
    for number, indent, content in block:
        if indent == 2:
            key, value = yaml_mapping_entry(content, f"{path}:{number}")
            if key in events:
                fail(f"{path}:{number}: duplicate event {key!r}")
            events[key] = [number, value, []]
            current = key
        elif indent > 2:
            if current is None:
                fail(f"{path}:{number}: event child appears before an event")
            events[current][2].append((number, indent, content))
        else:
            fail(f"{path}:{number}: malformed event indentation")

    if "push" not in events:
        return False
    number, value, nested = events["push"]
    return push_includes_main(path, number, value, nested)


def workflow_job_blocks(path):
    lines = workflow_yaml_lines(path)
    entries, order = top_level_yaml(path, lines)
    jobs_index, _jobs_number, _jobs_value = entries["jobs"]
    jobs_block = yaml_nested_block(lines, order, jobs_index)

    current_job = None
    job_children = {}
    for number, indent, content in jobs_block:
        if indent == 2:
            current_job, value = yaml_mapping_entry(content, f"{path}:{number}")
            if value:
                fail(f"{path}:{number}: inline job definitions are unsupported")
            job_children[current_job] = []
        elif indent > 2:
            if current_job is None:
                fail(f"{path}:{number}: job child appears before a job id")
            job_children[current_job].append((number, indent, content))
    return job_children


def workflow_step_blocks(path, job_name, job_block):
    steps = []
    current_step = None
    in_steps = False
    for number, indent, content in job_block:
        if indent == 4:
            key, value = yaml_mapping_entry(content, f"{path}:{number}")
            in_steps = key == "steps"
            if in_steps and value:
                fail(
                    f"{path}:{number}: inline {job_name} steps are unsupported"
                )
            current_step = None
            continue
        if not in_steps:
            continue
        if indent == 6:
            match = re.fullmatch(r"-[ ]+(name|uses):[ ]*(.+)", content)
            if not match:
                fail(
                    f"{path}:{number}: unsupported {job_name} step declaration"
                )
            current_step = {
                "number": number,
                match.group(1): yaml_scalar(
                    match.group(2), f"{path}:{number} {job_name} step"
                ),
                "children": [],
            }
            steps.append(current_step)
        elif indent > 6:
            if current_step is None:
                fail(
                    f"{path}:{number}: {job_name} step child appears before a step"
                )
            current_step["children"].append((number, indent, content))
    return steps


def direct_step_mapping(path, job_name, step):
    direct = {}
    for number, indent, content in step["children"]:
        if indent < 8:
            fail(f"{path}:{number}: malformed {job_name} step indentation")
        if indent > 8:
            continue
        key, value = yaml_mapping_entry(content, f"{path}:{number}")
        if key in direct:
            fail(
                f"{path}:{number}: duplicate {job_name} step key {key!r}"
            )
        direct[key] = (
            yaml_scalar(value, f"{path}:{number} {job_name}.{key}")
            if value
            else None
        )
    return direct


def validate_ci_commands(path):
    job_blocks = workflow_job_blocks(path)
    for job_name, expected_steps in EXPECTED_CI_RUN_STEPS.items():
        job_block = job_blocks.get(job_name)
        if job_block is None:
            fail(f"{path}: exact CI {job_name} job is missing")
        for number, indent, content in job_block:
            if indent == 4:
                key, _value = yaml_mapping_entry(content, f"{path}:{number}")
                if key in {"if", "continue-on-error"}:
                    fail(f"{path}:{number}: CI {job_name} job may not use {key}")

        steps = workflow_step_blocks(path, job_name, job_block)
        actual_run_steps = {}
        for step in steps:
            direct = direct_step_mapping(path, job_name, step)
            if "run" not in direct:
                continue
            step_name = step.get("name")
            if step_name is None:
                fail(
                    f"{path}:{step['number']}: every {job_name} run step "
                    "must have a name"
                )
            if step_name in actual_run_steps:
                fail(
                    f"{path}:{step['number']}: duplicate {job_name} run step "
                    f"{step_name!r}"
                )
            actual_run_steps[step_name] = (step["number"], direct)

        if set(actual_run_steps) != set(expected_steps):
            fail(
                f"{path}: {job_name} run-step names must be exactly "
                f"{sorted(expected_steps)!r}, got {sorted(actual_run_steps)!r}"
            )
        for step_name, expected in expected_steps.items():
            number, actual = actual_run_steps[step_name]
            if actual != expected:
                fail(
                    f"{path}:{number}: {step_name} keys and values must be "
                    f"exactly {expected!r}, got {actual!r}"
                )


def validate_workflow_inventory(root, manifest_entries, enforce_commands):
    directory = root / ".github" / "workflows"
    if not directory.is_dir():
        fail(f"{directory}: workflow directory is missing")
    try:
        candidates = sorted(directory.iterdir(), key=lambda path: path.name)
    except OSError as error:
        fail(f"{directory}: cannot list workflows: {error}")

    files = []
    for path in candidates:
        if path.suffix not in (".yml", ".yaml"):
            continue
        if path.is_symlink() or not path.is_file():
            fail(f"{path}: workflow must be a regular repo-owned file")
        files.append(path)
    if not files:
        fail(f"{directory}: no workflow YAML files found")

    triggered = sorted(path.name for path in files if workflow_pushes_main(path))
    contracted = sorted(manifest_entries)
    if triggered != contracted:
        missing = sorted(set(contracted) - set(triggered))
        unexpected = sorted(set(triggered) - set(contracted))
        fail(
            "push-to-main workflow inventory differs from the manifest; "
            f"missing={missing!r}, unexpected={unexpected!r}"
        )
    by_name = {path.name: path for path in files}
    for workflow, expected in manifest_entries.items():
        display_name, jobs = workflow_declared_surface(by_name[workflow])
        actual = {"name": display_name, "jobs": jobs}
        if actual != expected:
            fail(
                f"{by_name[workflow]}: rendered workflow contract mismatch; "
                f"expected={expected!r}, got={actual!r}"
            )
    if enforce_commands:
        validate_ci_commands(by_name["ci.yml"])


def direct_top_mapping(path, lines, entries, order, key):
    if key not in entries:
        fail(f"{path}: missing top-level {key!r} key")
    index, number, value = entries[key]
    if value:
        fail(f"{path}:{number}: inline top-level {key!r} is unsupported")
    block = yaml_nested_block(lines, order, index)
    values = {}
    for child_number, indent, content in block:
        if indent != 2:
            fail(f"{path}:{child_number}: nested {key!r} values are unsupported")
        child_key, child_value = yaml_mapping_entry(
            content, f"{path}:{child_number}"
        )
        if child_key in values:
            fail(f"{path}:{child_number}: duplicate {key}.{child_key}")
        values[child_key] = yaml_scalar(
            child_value, f"{path}:{child_number} {key}.{child_key}"
        )
    return values


def registry_event_contract(path, lines, entries, order):
    on_index, on_number, on_value = entries["on"]
    if on_value:
        fail(f"{path}:{on_number}: registry workflow events must be a mapping")
    block = yaml_nested_block(lines, order, on_index)
    events = {}
    current = None
    for number, indent, content in block:
        if indent == 2:
            key, value = yaml_mapping_entry(content, f"{path}:{number}")
            if key in events:
                fail(f"{path}:{number}: duplicate registry event {key!r}")
            if value:
                fail(f"{path}:{number}: inline registry events are unsupported")
            events[key] = [number, []]
            current = key
        elif indent > 2:
            if current is None:
                fail(f"{path}:{number}: registry event child precedes an event")
            events[current][1].append((number, indent, content))
        else:
            fail(f"{path}:{number}: malformed registry event indentation")
    if set(events) != {"release", "workflow_dispatch"}:
        fail(
            f"{path}: registry events must be exactly release and "
            f"workflow_dispatch, got {sorted(events)!r}"
        )

    release_number, release_block = events["release"]
    release_values = {}
    for number, indent, content in release_block:
        if indent != 4:
            fail(f"{path}:{number}: malformed release event contract")
        key, value = yaml_mapping_entry(content, f"{path}:{number}")
        if key in release_values:
            fail(f"{path}:{number}: duplicate release.{key}")
        release_values[key] = yaml_flow_sequence(
            value, f"{path}:{number} release.{key}"
        )
    if release_values != {"types": ["published"]}:
        fail(
            f"{path}:{release_number}: release event must be exactly "
            "types: [published]"
        )

    dispatch_number, dispatch_block = events["workflow_dispatch"]
    dispatch_lines = {}
    for number, indent, content in dispatch_block:
        key, value = yaml_mapping_entry(content, f"{path}:{number}")
        location = (indent, key)
        if location in dispatch_lines:
            fail(f"{path}:{number}: duplicate workflow_dispatch {key!r}")
        dispatch_lines[location] = (number, value)
    for required in ((4, "inputs"), (6, "tag"), (8, "required")):
        if required not in dispatch_lines:
            fail(
                f"{path}:{dispatch_number}: workflow_dispatch must require "
                "an exact tag input"
            )
    if dispatch_lines[(4, "inputs")][1] or dispatch_lines[(6, "tag")][1]:
        fail(f"{path}:{dispatch_number}: tag input must use nested mappings")
    if dispatch_lines[(8, "required")][1] != "true":
        fail(f"{path}:{dispatch_number}: workflow_dispatch tag must be required")
    allowed = {
        (4, "inputs"),
        (6, "tag"),
        (8, "description"),
        (8, "required"),
        (8, "type"),
    }
    if not set(dispatch_lines).issubset(allowed):
        fail(f"{path}:{dispatch_number}: unexpected workflow_dispatch input")
    if (8, "type") in dispatch_lines and dispatch_lines[(8, "type")][1] != "string":
        fail(f"{path}:{dispatch_number}: workflow_dispatch tag type must be string")


def registry_steps(path, raw):
    lines = raw.splitlines()
    starts = []
    for index, line in enumerate(lines):
        match = re.fullmatch(r" {6}- name:[ ]+(.+)", line)
        if match:
            starts.append((index, yaml_scalar(match.group(1), f"{path}:{index + 1}")))
        elif re.match(r"^ {6}-[ ]+", line):
            fail(f"{path}:{index + 1}: every registry step must have an exact name")
    steps = []
    for position, (start, name) in enumerate(starts):
        end = starts[position + 1][0] if position + 1 < len(starts) else len(lines)
        steps.append(
            {
                "name": name,
                "number": start + 1,
                "lines": lines[start:end],
            }
        )
    names = tuple(step["name"] for step in steps)
    if names != EXPECTED_REGISTRY_STEPS:
        fail(
            f"{path}: registry steps must be exactly "
            f"{list(EXPECTED_REGISTRY_STEPS)!r}, got {list(names)!r}"
        )
    return {step["name"]: step for step in steps}


def registry_step_mapping(path, step, key):
    lines = step["lines"]
    matches = []
    for index, line in enumerate(lines):
        match = re.fullmatch(rf" {{8}}{re.escape(key)}:(?:[ ]*(.*))?", line)
        if match:
            matches.append((index, (match.group(1) or "").strip()))
    if len(matches) != 1:
        fail(
            f"{path}:{step['number']}: step {step['name']!r} needs exactly "
            f"one {key!r} mapping"
        )
    start, value = matches[0]
    if value:
        fail(
            f"{path}:{step['number'] + start}: inline step {key!r} is unsupported"
        )
    values = {}
    for offset in range(start + 1, len(lines)):
        line = lines[offset]
        if line and len(line) - len(line.lstrip(" ")) <= 8:
            break
        match = re.fullmatch(
            r" {10}([A-Za-z_][A-Za-z0-9_-]*):(?:[ ]*(.*))?", line
        )
        if not match:
            if line.strip() and not line.lstrip().startswith("#"):
                fail(
                    f"{path}:{step['number'] + offset}: malformed "
                    f"{step['name']} {key} mapping"
                )
            continue
        child_key = match.group(1)
        if child_key in values:
            fail(
                f"{path}:{step['number'] + offset}: duplicate "
                f"{step['name']} {key}.{child_key}"
            )
        child_value = strip_yaml_comment(
            (match.group(2) or "").strip(),
            f"{path}:{step['number'] + offset}",
        ).strip()
        if not child_value:
            fail(
                f"{path}:{step['number'] + offset}: empty "
                f"{step['name']} {key}.{child_key}"
            )
        values[child_key] = child_value
    return values


def registry_step_keys(path, step):
    keys = []
    seen = set()
    for offset, line in enumerate(step["lines"][1:], 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        indent = len(line) - len(line.lstrip(" "))
        if indent != 8:
            continue
        key, _value = yaml_mapping_entry(
            line.strip(), f"{path}:{step['number'] + offset}"
        )
        if key in seen:
            fail(
                f"{path}:{step['number'] + offset}: duplicate "
                f"{step['name']} step key {key!r}"
            )
        seen.add(key)
        keys.append(key)
    return tuple(keys)


def registry_step_run(path, step):
    lines = step["lines"]
    matches = []
    for index, line in enumerate(lines):
        match = re.fullmatch(r" {8}run:(?:[ ]*(.*))?", line)
        if match:
            matches.append((index, (match.group(1) or "").strip()))
    if len(matches) != 1:
        fail(
            f"{path}:{step['number']}: step {step['name']!r} needs exactly "
            "one run command"
        )
    start, value = matches[0]
    if value and value not in ("|", "|-", ">", ">-"):
        command = value
    else:
        command_lines = []
        for offset in range(start + 1, len(lines)):
            line = lines[offset]
            if line and len(line) - len(line.lstrip(" ")) <= 8:
                break
            if line.lstrip().startswith("#"):
                continue
            command_lines.append(line[10:] if len(line) >= 10 else "")
        command = "\n".join(command_lines)
    command = re.sub(r"\\[ \t]*\n", " ", command)
    return re.sub(r"\s+", " ", command).strip()


def require_substrings_in_order(path, step, command, markers):
    previous = -1
    for marker in markers:
        position = command.find(marker)
        if position < 0:
            fail(
                f"{path}:{step['number']}: step {step['name']!r} is missing "
                f"binding command {marker!r}"
            )
        if position <= previous:
            fail(
                f"{path}:{step['number']}: step {step['name']!r} has an "
                "unsafe authority order"
            )
        previous = position


def require_exact_registry_command(path, step, command):
    expected = EXPECTED_REGISTRY_COMMANDS[step["name"]]
    if command != expected:
        fail(
            f"{path}:{step['number']}: step {step['name']!r} command is not "
            "the exact fail-closed registry contract"
        )


def validate_registry_workflow(root):
    path = root / REGISTRY_WORKFLOW_PATH
    try:
        raw = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as error:
        fail(f"{path}: cannot read UTF-8 workflow: {error}")
    lines = workflow_yaml_lines(path)
    entries, order = top_level_yaml(path, lines)
    expected_top_keys = {"name", "on", "permissions", "env", "jobs"}
    if set(entries) != expected_top_keys:
        fail(
            f"{path}: top-level registry workflow keys must be exactly "
            f"{sorted(expected_top_keys)!r}, got {sorted(entries)!r}"
        )
    if yaml_scalar(entries["name"][2], f"{path} name") != "registry-publish":
        fail(f"{path}: workflow name must be exactly registry-publish")
    registry_event_contract(path, lines, entries, order)

    permissions = direct_top_mapping(
        path, lines, entries, order, "permissions"
    )
    expected_permissions = {
        "actions": "read",
        "contents": "read",
        "id-token": "write",
    }
    if permissions != expected_permissions:
        fail(
            f"{path}: permissions must be exactly {expected_permissions!r}, "
            f"got {permissions!r}"
        )
    environment = direct_top_mapping(path, lines, entries, order, "env")
    expected_environment = {
        "MCP_PUBLISHER_VERSION": "v1.7.9",
        "MCP_PUBLISHER_LINUX_AMD64_SHA256": PUBLISHER_SHA256,
    }
    if environment != expected_environment:
        fail(f"{path}: mcp-publisher version/digest pins are not exact")

    jobs_index, jobs_number, _jobs_value = entries["jobs"]
    jobs_block = yaml_nested_block(lines, order, jobs_index)
    job_ids = []
    for number, indent, content in jobs_block:
        if indent == 2:
            job_id, value = yaml_mapping_entry(content, f"{path}:{number}")
            if value:
                fail(f"{path}:{number}: inline registry jobs are unsupported")
            job_ids.append(job_id)
    if job_ids != ["publish"]:
        fail(f"{path}:{jobs_number}: registry workflow must have one publish job")
    job_direct = {}
    for number, indent, content in jobs_block:
        if indent != 4:
            continue
        key, value = yaml_mapping_entry(content, f"{path}:{number}")
        if key in job_direct:
            fail(f"{path}:{number}: duplicate publish job key {key!r}")
        job_direct[key] = (number, value)
    if set(job_direct) != {"runs-on", "steps"}:
        fail(
            f"{path}:{jobs_number}: publish job keys must be exactly "
            "runs-on and steps"
        )
    if job_direct["runs-on"][1] != "ubuntu-latest":
        fail(f"{path}:{job_direct['runs-on'][0]}: registry runner is not exact")
    if job_direct["steps"][1]:
        fail(f"{path}:{job_direct['steps'][0]}: inline registry steps are unsupported")
    for number, _indent, content in lines:
        if re.match(r"(?:if|continue-on-error):", content):
            fail(
                f"{path}:{number}: conditional or best-effort registry "
                "authority is forbidden"
            )

    steps = registry_steps(path, raw)
    expected_step_keys = {
        "Resolve tag": ("id", "env", "run"),
        "Checkout recovery controller": ("uses", "with"),
        "Set up Go": ("uses", "with"),
        "Hydrate and verify exact release asset set": ("env", "run"),
        "Verify exact release authority": ("env", "run"),
        "Install mcp-publisher": ("env", "run"),
        "Generate and validate dist/server.json": ("env", "run"),
        "Publish via OIDC": ("run",),
    }
    for step_name, expected_keys in expected_step_keys.items():
        actual_keys = registry_step_keys(path, steps[step_name])
        if actual_keys != expected_keys:
            fail(
                f"{path}:{steps[step_name]['number']}: {step_name} step keys "
                f"must be exactly {expected_keys!r}, got {actual_keys!r}"
            )
    if sum(
        line == "        id: tag" for line in steps["Resolve tag"]["lines"]
    ) != 1:
        fail(f"{path}:{steps['Resolve tag']['number']}: tag step id is not exact")
    checkout = steps["Checkout recovery controller"]
    checkout_text = "\n".join(checkout["lines"])
    if checkout_text.count(f"uses: {CHECKOUT_ACTION}") != 1:
        fail(f"{path}:{checkout['number']}: checkout action pin is not exact")
    checkout_with = registry_step_mapping(path, checkout, "with")
    expected_checkout = {
        "repository": "osauer/canary",
        "ref": "${{ github.workflow_sha }}",
        "fetch-depth": "0",
        "fetch-tags": "true",
    }
    if checkout_with != expected_checkout:
        fail(
            f"{path}:{checkout['number']}: checkout authority must be exactly "
            f"{expected_checkout!r}, got {checkout_with!r}"
        )
    setup = steps["Set up Go"]
    setup_text = "\n".join(setup["lines"])
    if setup_text.count(f"uses: {SETUP_GO_ACTION}") != 1:
        fail(f"{path}:{setup['number']}: setup-go action pin is not exact")
    if registry_step_mapping(path, setup, "with") != {
        "go-version-file": "go.mod"
    }:
        fail(
            f"{path}:{setup['number']}: setup-go must use the controller go.mod"
        )

    expected_step_environments = {
        "Resolve tag": {
            "RELEASE_TAG": "${{ github.event.release.tag_name || inputs.tag }}",
        },
        "Hydrate and verify exact release asset set": {
            "GH_TOKEN": "${{ github.token }}",
            "RELEASE_TAG": "${{ steps.tag.outputs.tag }}",
        },
        "Verify exact release authority": {
            "GH_TOKEN": "${{ github.token }}",
            "RELEASE_TAG": "${{ steps.tag.outputs.tag }}",
        },
        "Install mcp-publisher": {
            "GH_TOKEN": "${{ github.token }}",
        },
        "Generate and validate dist/server.json": {
            "RELEASE_TAG": "${{ steps.tag.outputs.tag }}",
        },
    }
    for step_name, expected_step_environment in expected_step_environments.items():
        if (
            registry_step_mapping(path, steps[step_name], "env")
            != expected_step_environment
        ):
            fail(
                f"{path}:{steps[step_name]['number']}: {step_name} must use "
                f"the exact trusted environment {expected_step_environment!r}"
            )

    resolve = registry_step_run(path, steps["Resolve tag"])
    require_exact_registry_command(path, steps["Resolve tag"], resolve)
    require_substrings_in_order(
        path,
        steps["Resolve tag"],
        resolve,
        (
            "set -euo pipefail",
            'tag="$RELEASE_TAG"',
            '[[ "$tag" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+'
            "(-[A-Za-z0-9.-]+)?$ ]]",
            "GITHUB_OUTPUT",
        ),
    )

    assets = registry_step_run(
        path, steps["Hydrate and verify exact release asset set"]
    )
    require_exact_registry_command(
        path, steps["Hydrate and verify exact release asset set"], assets
    )
    require_substrings_in_order(
        path,
        steps["Hydrate and verify exact release asset set"],
        assets,
        (
            "set -euo pipefail",
            'make release-github-assets RELEASE_VERSION="$RELEASE_TAG"',
        ),
    )

    authority = registry_step_run(path, steps["Verify exact release authority"])
    require_exact_registry_command(
        path, steps["Verify exact release authority"], authority
    )
    waiter = (
        "GOFLAGS= go run ./scripts/release-ci-wait "
        '-contract "$contract" -historical '
        '-sha "$release_sha" -branch main -event push '
        "-poll 15s -timeout 30m"
    )
    require_substrings_in_order(
        path,
        steps["Verify exact release authority"],
        authority,
        (
            "set -euo pipefail",
            'tag="$RELEASE_TAG"',
            'release_sha="$(git rev-parse "refs/tags/${tag}^{commit}")"',
            'contract="$(mktemp "${RUNNER_TEMP}/canary-release-ci-contract.XXXXXX")"',
            "trap 'rm -f \"$contract\"' EXIT HUP INT TERM",
            './scripts/check-release-source.sh --controller "$tag"',
            "./scripts/check-release-origin.sh",
            "./scripts/check-release-ci-contract.sh",
            'python3 ./scripts/materialize-release-ci-contract.py "$tag" "$contract"',
            waiter,
            './scripts/check-release-tag.sh "$tag"',
            './scripts/check-release-tag.sh --plugin "$tag"',
            './scripts/check-github-release.sh "$tag" dist',
        ),
    )

    install = registry_step_run(path, steps["Install mcp-publisher"])
    require_exact_registry_command(path, steps["Install mcp-publisher"], install)
    require_substrings_in_order(
        path,
        steps["Install mcp-publisher"],
        install,
        (
            "set -euo pipefail",
            'archive="${RUNNER_TEMP}/mcp-publisher_linux_amd64.tar.gz"',
            'gh release download "$MCP_PUBLISHER_VERSION"',
            "--repo github.com/modelcontextprotocol/registry",
            "--pattern 'mcp-publisher_linux_amd64.tar.gz'",
            '"$MCP_PUBLISHER_LINUX_AMD64_SHA256" "$archive"',
            "sha256sum --check --strict -",
            'tar -xzf "$archive" -C bin mcp-publisher',
            "bin/mcp-publisher --version",
        ),
    )

    generate = registry_step_run(
        path, steps["Generate and validate dist/server.json"]
    )
    require_exact_registry_command(
        path, steps["Generate and validate dist/server.json"], generate
    )

    publish = registry_step_run(path, steps["Publish via OIDC"])
    require_exact_registry_command(path, steps["Publish via OIDC"], publish)
    require_substrings_in_order(
        path,
        steps["Publish via OIDC"],
        publish,
        (
            "set -euo pipefail",
            "bin/mcp-publisher login github-oidc",
            "MCP_REGISTRY_AUTO_LOGIN=0 "
            "./scripts/registry-publish-with-login.sh "
            "bin/mcp-publisher dist/server.json",
        ),
    )

    commands = " ".join(
        registry_step_run(path, step)
        for step in steps.values()
        if any(re.fullmatch(r" {8}run:.*", line) for line in step["lines"])
    )
    if "${{" in commands:
        fail(
            f"{path}: GitHub expressions are forbidden in run commands; "
            "pass untrusted values through exact environment mappings"
        )
    if re.search(
        r"(?:^|[\s;&|])(?:[^\s;&|]*/)?mcp-publisher[ ]+publish"
        r"(?:$|[\s;&|])",
        commands,
    ):
        fail(f"{path}: raw mcp-publisher publish is forbidden")
    if commands.count("./scripts/registry-publish-with-login.sh") != 1:
        fail(f"{path}: typed registry publish wrapper must be invoked exactly once")
    for forbidden in (
        "registry.modelcontextprotocol.io",
        "registry-publish-verify-first.sh",
        "${{ github.repository }}",
        "GH_REPO",
    ):
        if forbidden in commands:
            fail(f"{path}: forbidden ambient or pre-verification sink {forbidden!r}")
    if re.search(r"\|\|[ ]*(?:true|:)|;[ ]*(?:true|:)(?:[ ]|$)|set[ ]+\+e", commands):
        fail(f"{path}: shell-masked registry authority is forbidden")


def make_target_body(makefile, target):
    try:
        lines = makefile.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeDecodeError) as error:
        fail(f"{makefile}: cannot read UTF-8 Makefile: {error}")

    headers = [
        index
        for index, line in enumerate(lines)
        if re.match(rf"^{re.escape(target)}[ ]*:(?!=)", line)
    ]
    if len(headers) != 1:
        fail(
            f"{makefile}: expected exactly one {target} target, "
            f"found {len(headers)}"
        )
    start = headers[0]
    end = len(lines)
    for index in range(start + 1, len(lines)):
        if re.match(r"^[A-Za-z0-9_.%/+@-][^=]*:(?!=)", lines[index]):
            end = index
            break

    logical = []
    buffer = ""
    for number, line in enumerate(lines[start + 1 : end], start + 2):
        if not line.strip() or line.startswith("#"):
            if buffer:
                fail(
                    f"{makefile}:{number}: comment interrupted "
                    f"{target} recipe continuation"
                )
            continue
        if not line.startswith("\t"):
            fail(f"{makefile}:{number}: {target} recipe line lacks a tab")
        command = line[1:]
        if re.search(r"\\[ ]*$", command):
            buffer += re.sub(r"\\[ ]*$", "", command) + " "
        else:
            buffer += command
            logical.append((number, buffer))
            buffer = ""
    if buffer:
        fail(f"{makefile}: unterminated {target} recipe continuation")
    if not logical:
        fail(f"{makefile}: {target} recipe is empty")
    return logical


def validate_make_target_invocation(root, target, require_historical):
    makefile = root / "Makefile"
    helper = ["go", "run", "./scripts/release-ci-wait"]
    expected = ["GOFLAGS="] + helper + ["-contract", CONTRACT_PATH]
    if require_historical:
        expected[-1] = "$$contract"
        expected.append("-historical")
        expected_sha = "$$release_sha"
    else:
        expected_sha = "$$(git rev-parse HEAD)"
    expected.extend(
        [
            "-sha",
            expected_sha,
            "-branch",
            "$(MAIN_BRANCH)",
            "-event",
            "push",
            "-poll",
            "$(RELEASE_CI_POLL)",
            "-timeout",
            "$(RELEASE_CI_TIMEOUT)",
        ]
    )
    historical_command = (
        '@release_sha=$$(git rev-parse --verify '
        '"refs/tags/$(RELEASE_VERSION)^{commit}") || { '
        'echo "_release-ci-wait-historical: cannot resolve release tag '
        '$(RELEASE_VERSION)" >&2; exit 1; }; '
        'contract=$$(mktemp "$${TMPDIR:-/tmp}/'
        'canary-release-ci-contract.XXXXXX") || exit 1; '
        "trap 'rm -f \"$$contract\"' EXIT HUP INT TERM; "
        "python3 ./scripts/materialize-release-ci-contract.py "
        '"$(RELEASE_VERSION)" "$$contract"; '
        "GOFLAGS= go run ./scripts/release-ci-wait "
        '-contract "$$contract" -historical '
        '-sha "$$release_sha" -branch "$(MAIN_BRANCH)" -event push '
        '-poll "$(RELEASE_CI_POLL)" -timeout "$(RELEASE_CI_TIMEOUT)"'
    )
    legacy_flags = {"-workflow", "-workflow-name", "-job"}
    invocations = []

    for number, command in make_target_body(makefile, target):
        try:
            lexer = shlex.shlex(command, posix=True, punctuation_chars=";&|")
            lexer.whitespace_split = True
            lexer.commenters = "#"
            tokens = list(lexer)
        except ValueError as error:
            fail(f"{makefile}:{number}: malformed shell recipe: {error}")
        operators = [
            token
            for token in tokens
            if token and all(character in ";&|" for character in token)
        ]
        normalized = [token for token in tokens if token not in operators]
        if normalized and normalized[0].lstrip("@+-") == "GOFLAGS=":
            if normalized[0] not in ("GOFLAGS=", "@GOFLAGS="):
                fail(
                    f"{makefile}:{number}: Make ignore/recursive prefixes "
                    "are forbidden on the release-ci-wait invocation"
                )
            normalized[0] = normalized[0].lstrip("@")
        positions = [
            index
            for index in range(max(0, len(normalized) - 2))
            if normalized[index : index + 3] == helper
        ]
        if len(positions) > 1:
            fail(
                f"{makefile}:{number}: release-ci-wait helper is invoked "
                "more than once in one recipe"
            )
        helper_position = positions[0] if positions else None
        assignment_position = (
            helper_position - 1 if helper_position is not None else None
        )
        if positions and (
            assignment_position is None
            or assignment_position < 0
            or normalized[assignment_position] != "GOFLAGS="
        ):
            fail(
                f"{makefile}:{number}: release-ci-wait invocation must be "
                "the shell command after an exact GOFLAGS= assignment"
            )
        if positions and require_historical:
            compact_command = re.sub(r"\s+", " ", command).strip()
            if compact_command != historical_command:
                fail(
                    f"{makefile}:{number}: historical release-ci-wait must "
                    "resolve and wait for the exact release-tag commit"
                )
        elif positions and (positions != [1] or operators):
            fail(
                f"{makefile}:{number}: shell control operators are forbidden "
                "on the release-ci-wait invocation"
            )

        is_invocation = bool(positions)
        legacy_tokens = [
            token
            for token in normalized
            if any(
                token == flag
                or token == "--" + flag[1:]
                or token.startswith(flag + "=")
                or token.startswith("--" + flag[1:] + "=")
                for flag in legacy_flags
            )
        ]
        if legacy_tokens:
            fail(
                f"{makefile}:{number}: per-workflow flags are forbidden; "
                f"use {CONTRACT_PATH}"
            )
        if not is_invocation and "-contract" in normalized:
            fail(
                f"{makefile}:{number}: -contract appears outside a "
                "release-ci-wait invocation"
            )
        if not is_invocation:
            continue

        invocations.append((number, normalized[assignment_position:]))

    if len(invocations) != 1:
        fail(
            f"{makefile}: {target} must invoke the Go helper exactly "
            f"once, found {len(invocations)}"
        )
    number, actual = invocations[0]
    if actual != expected:
        fail(
            f"{makefile}:{number}: {target} waiter command must be exactly "
            f"{expected!r}, got {actual!r}"
        )


def validate_make_invocation(root):
    validate_make_target_invocation(root, "release-ci-wait", False)
    validate_make_target_invocation(
        root, "_release-ci-wait-historical", True
    )


def main():
    if len(sys.argv) != 3:
        fail("internal usage error")
    root = Path(sys.argv[1]).resolve()
    authority_only = sys.argv[2] == "1"
    if not root.is_dir():
        fail(f"{root}: repository root is not a directory")
    manifest_entries = load_manifest(root, not authority_only)
    validate_workflow_inventory(root, manifest_entries, not authority_only)
    if not authority_only:
        validate_legacy_contract(root)
        validate_registry_workflow(root)
        validate_make_invocation(root)
    print("check-release-ci-contract: OK")


try:
    main()
except ContractError as error:
    print(f"check-release-ci-contract: {error}", file=sys.stderr)
    sys.exit(1)
PY
