import MarkdownIt from "./vendor/markdown-it.mjs";

// Model output is untrusted. Keep raw HTML and automatic URL conversion off;
// markdown-it escapes code and rejects unsafe link protocols by default.
const markdown = new MarkdownIt({
  html: false,
  linkify: false,
  typographer: false,
  breaks: false,
});

// A model must not cause the browser to contact an image host automatically.
markdown.renderer.rules.image = (tokens, tokenIndex) => {
  const alternativeText = tokens[tokenIndex].content || "Image";
  return `<span class="markdown-image">${markdown.utils.escapeHtml(alternativeText)}</span>`;
};

markdown.renderer.rules.link_open = (
  tokens,
  tokenIndex,
  options,
  environment,
  renderer,
) => {
  tokens[tokenIndex].attrSet("target", "_blank");
  tokens[tokenIndex].attrSet("rel", "noopener noreferrer");
  return renderer.renderToken(tokens, tokenIndex, options);
};

export function renderMarkdown(text) {
  return markdown.render(text);
}

export function renderAssistantMessage(element, text) {
  element.classList.add("markdown");
  // This is the only HTML insertion point. Never pass raw model text here.
  element.innerHTML = renderMarkdown(text);
}
