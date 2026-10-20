# Source

Vendored from https://github.com/bigskysoftware/htmx/releases/tag/v2.0.9
(`htmx.min.js`, latest stable release as of this vendoring -- v4.0.0-betaN
existed but was pre-1.0-stability for that major version, so v2.0.9 was
chosen instead, consistent with this project's policy of pinning stable
releases everywhere else).

Embedded via `go:embed` (KTD10) so the webui ships as part of the single
backend binary with no CDN dependency at runtime.

To update: download the new version's `htmx.min.js` from the GitHub
release assets, verify its size against the release's published asset
size, and update the version number in this file.
