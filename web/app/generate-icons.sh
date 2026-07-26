#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

master="${CANARY_ICON_MASTER:-icon-512.png}"
source_sheet="${CANARY_ICON_SOURCE_SHEET:-}"
crop_geometry="${CANARY_ICON_CROP:-}"
tmp_dir="$(mktemp -d -t canary-icons)"
tmp_crop="$tmp_dir/crop.png"
tmp_512="$tmp_dir/icon-512.png"

cleanup() {
  rm -f "$tmp_crop" "$tmp_512"
  rmdir "$tmp_dir"
}
trap cleanup EXIT

require_sips() {
  if ! command -v sips >/dev/null 2>&1; then
    echo "generate-icons: sips is required on this machine" >&2
    exit 1
  fi
}

resize_png() {
  local size="$1"
  local src="$2"
  local dst="$3"
  sips -z "$size" "$size" "$src" --out "$dst" >/dev/null
}

if [[ -n "$source_sheet" ]]; then
  if [[ ! -f "$source_sheet" ]]; then
    echo "generate-icons: source sheet not found: $source_sheet" >&2
    exit 1
  fi
  if [[ -z "$crop_geometry" ]]; then
    echo "generate-icons: set CANARY_ICON_CROP as y,x,height,width for source-sheet recrops" >&2
    exit 1
  fi

  IFS=, read -r crop_y crop_x crop_h crop_w extra <<<"$crop_geometry"
  if [[ -n "${extra:-}" || -z "${crop_y:-}" || -z "${crop_x:-}" || -z "${crop_h:-}" || -z "${crop_w:-}" ]]; then
    echo "generate-icons: invalid CANARY_ICON_CROP; expected y,x,height,width" >&2
    exit 1
  fi

  require_sips
  sips -c "$crop_h" "$crop_w" --cropOffset "$crop_y" "$crop_x" "$source_sheet" --out "$tmp_crop" >/dev/null
  master="$tmp_crop"
fi

if [[ ! -f "$master" ]]; then
  echo "generate-icons: master icon not found: $master" >&2
  exit 1
fi

require_sips
resize_png 512 "$master" "$tmp_512"
mv "$tmp_512" icon-512.png
master="icon-512.png"
resize_png 192 "$master" icon-192.png
resize_png 64 "$master" favicon-64.png
resize_png 32 "$master" favicon-32.png
resize_png 16 "$master" favicon-16.png

echo "generated canonical 512px and derived Canary PNG icons"
