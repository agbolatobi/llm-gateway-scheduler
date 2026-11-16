# Browser dependency

`static/vendor/markdown-it.mjs` is the unmodified browser ESM bundle from
**markdown-it 15.0.3**, distributed under the MIT license. Its license is kept
beside the bundle. Source: https://github.com/markdown-it/markdown-it

The gateway embeds and serves this file locally; there is no CDN or Node.js
requirement at runtime. To update, copy `dist/browser/markdown-it.esm.min.mjs`
and `LICENSE` from an explicitly pinned npm package, update this version, and
run the Markdown and browser safety tests. Keep raw HTML disabled in `markdown.mjs`.
