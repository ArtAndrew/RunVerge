# README diagrams

All three diagrams have English, Russian, Spanish, Brazilian Portuguese, German, French and Simplified Chinese versions. Each README links to its own language. Brand names, API, CLI, SDK, PostgreSQL and version identifiers remain unchanged.

Edit the copy in `labels.json`. Array positions follow the `title`, `desc` and `text` elements in the corresponding English SVG. The English SVGs also provide the shared layout; other locale SVGs are generated files. Run `python3 docs/images/render.py` from the repository root. No extra package or network access is needed.

Check the rendered text against its card boundaries before publication. `font-sizes.json` records reviewed font-size adjustments for longer translations. Keep labels short; do not compress glyphs or remove important meaning to make a translation fit. Use the descriptions and README prose to explain details.

The diagrams describe the current product and checked distribution. They do not announce provider or operating-system support. Documentation and generated SVGs use the repository's Elastic License 2.0.
