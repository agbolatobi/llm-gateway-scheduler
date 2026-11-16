import test from "node:test";
import assert from "node:assert/strict";
import { renderMarkdown } from "../internal/chatui/static/markdown.mjs";

test("renders headings, emphasis, lists, tables and escaped code", () => {
  const output = renderMarkdown(
    "## Heading\n\n**Bold** and *italic*.\n\n- First\n- Second\n\n```html\n<script>alert(1)</script>\n```\n\n| Name | Value |\n| --- | --- |\n| Queue | FIFO |",
  );
  for (const expected of [
    "<h2>Heading</h2>",
    "<strong>Bold</strong>",
    "<em>italic</em>",
    "<ul>",
    "<li>First</li>",
    '<pre><code class="language-html">',
    "&lt;script&gt;",
    "<table>",
  ]) {
    assert.ok(output.includes(expected), expected);
  }
});

test("raw HTML is escaped and dangerous URLs never become links", () => {
  const output = renderMarkdown(
    '<script>alert(1)</script>\n<img src=x onerror=alert(1)>\n\n[bad](javascript:alert(1))\n\n[bad](data:text/html;base64,WA==)\n\n<iframe src="https://evil.example"></iframe>',
  );
  assert.doesNotMatch(output, /<(script|img|iframe)\b/i);
  assert.doesNotMatch(output, /href="(javascript|data):/i);
  assert.match(output, /&lt;script&gt;/);
});

test("remote images become escaped text and safe links isolate new tabs", () => {
  const output = renderMarkdown(
    "![<b>diagram</b>](https://example.com/tracker.png)\n\n[Docs](https://example.com)",
  );
  assert.doesNotMatch(output, /<img\b|src=|<b>/);
  assert.match(output, /&lt;b&gt;diagram&lt;\/b&gt;/);
  assert.match(output, /target="_blank"/);
  assert.match(output, /rel="noopener noreferrer"/);
});

test("partial streamed Markdown remains safe and completes on the next render", () => {
  assert.doesNotMatch(
    renderMarkdown("```html\n<img src=x onerror=alert(1)>"),
    /<img\b/,
  );
  assert.match(renderMarkdown("**Hello"), /\*\*Hello/);
  assert.match(renderMarkdown("**Hello**"), /<strong>Hello<\/strong>/);
});
