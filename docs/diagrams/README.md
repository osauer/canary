# Documentation diagram sources

The diagrams are deterministic, repository-owned SVG compositions. Generic
component symbols are selected from Tabler Icons 3.45.0 and inlined into the
SVG output; see `ICON-LICENSE.txt`.

Regenerate the SVG files and high-resolution PNG fallbacks from this directory:

```sh
node ../../scripts/render-architecture.mjs
rsvg-convert --zoom=2 system-architecture.svg --output system-architecture.png
rsvg-convert --zoom=2 data-and-persistence.svg --output data-and-persistence.png
rsvg-convert --zoom=2 policy-lifecycle.svg --output policy-lifecycle.png
rsvg-convert --zoom=2 policy-authority.svg --output policy-authority.png
rsvg-convert --zoom=2 storage-overview.svg --output storage-overview.png
rsvg-convert --zoom=2 sqlite-data-model.svg --output sqlite-data-model.png
rsvg-convert --zoom=2 sqlite-update-lifecycle.svg --output sqlite-update-lifecycle.png
rsvg-convert --zoom=2 sensor-authority-pipeline.svg --output sensor-authority-pipeline.png
rsvg-convert --zoom=2 sensor-freshness-timeline.svg --output sensor-freshness-timeline.png
```

Verify the checked-in SVGs without rewriting them:

```sh
node ../../scripts/render-architecture.mjs --check
```

The Canary logo is read from `../social/canary-icon.png` during generation.
