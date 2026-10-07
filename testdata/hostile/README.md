# Hostile HTML corpus

Inputs for the sanitizer and iframe assembly: DOMPurify (`WHOLE_DOCUMENT`) →
`target=_blank` rewrite → `cid:` rewrite → CSP `<meta>` first in head → `iframe
srcdoc` with `sandbox="allow-same-origin allow-popups
allow-popups-to-escape-sandbox"`. AGENTS.md requires any change to that chain to
be checked against every file here.

## Conventions

Each file is a complete HTML document, which is how `notmuch show --include-html` hands
mail to the renderer.

- **Script execution** always writes `top.__pwned = '<case-id>'`. After rendering a case,
  `window.__pwned` in the app window must be `undefined`. With `allow-same-origin`, any
  script that runs in the frame can reach `top`, so this one check catches every
  execution path.
- **Remote fetches** always target `tracker.invalid` (RFC 6761, never resolves). Any
  request to `*.invalid` in the network log is a failure, even a failed one: the request
  leaving is the leak. The exception is 38, whose point is the real host names of
  receipt-tracker tools; run.sh resolves every name but `remote.test` to nothing and
  holds those names to the same rule.
- **Visible content** is a `<p class="canary">` in every file. It must render.
  Stripping everything is not a pass.

Layer responsible: **S** sandbox (no `allow-scripts`), **P** DOMPurify, **C** CSP,
**A** the app's post-processing (link rewrite, cid rewrite, srcdoc assignment, sizing).
DOMPurify does not sanitize CSS. Every CSS `url()` case depends on CSP, plus
mailframe.js's URL filter, which runs the browser's CSS parser over style attributes and
`<style>` blocks and drops declarations that fetch from an IP literal, `localhost`, or
an app path outside the message's parts (35, 36), or a receipt-tracker host (38).

## Cases

| File | Attacks with | Must NOT | Must still | Layer |
|---|---|---|---|---|
| `01-inline-script.html` | `<script>` in head and body, external `src`, `type=module`, uppercase, a `<scr<script>ipt>` split | execute; fetch `01-external.js` | render the canary and trailing text; leftover split fragments may show as text | S, P |
| `02-event-handlers.html` | `onerror`, `onload` on body, `onmouseover`, `autofocus`+`onfocus`, `ontoggle`, `<source onerror>`, `onclick`, `<marquee onstart>`, a second `<body>` | execute any handler, including on hover or focus | render the link, the details element, and the hover box | S, P |
| `03-javascript-href.html` | `javascript:` plain, mixed case, entity-encoded, tab inside the scheme, leading control characters, `vbscript:`, `data:text/html`, `<area href>` | execute or navigate on click | keep the `https:` and `mailto:` links working, the https one in a new tab | S, P |
| `04-svg-script.html` | `<svg><script>`, `svg onload`, `xlink:href=javascript:`, `<animate attributeName=href>`, `<set>` handler, remote `<use>` and `<image>`, `<foreignObject>` img onerror | execute; fetch `04-use.svg` or `04-image.png` | render the green SVG rectangle and SVG text | S, P, C |
| `05-iframe-object-embed.html` | nested `<iframe src>` remote, `srcdoc` and `javascript:`; `<object>` remote and `data:text/html`; `<embed>`; `<portal>`; `<frameset>` | create any nested browsing context or plugin; fetch anything | render the canary | P, C |
| `06-meta-refresh.html` | `<meta http-equiv=refresh>` to a remote and to `javascript:`, a competing `<meta http-equiv=Content-Security-Policy>` that tries to loosen, `set-cookie`, `referrer` | navigate the frame; relax the app's CSP. A second CSP can only tighten, but DOMPurify should still strip it | stay on this message and render the canary | P, C |
| `07-base-href.html` | `<base href>` to a remote and `<base target=_top>` | resolve relative `src`/`href` against `tracker.invalid`; retarget links at the app window | render the canary; relative URLs resolve against about:srcdoc, which inherits the app's base (see `30-self-origin.html`) | P, A |
| `08-form-remote.html` | a credential-phishing `<form action=remote method=post>`, detached `button formaction`, `<input type=image src>`, `<isindex>` | submit anywhere, including by Enter in the field; fetch `08-input-image.png` | text may render. Stripping the form entirely is acceptable | P, S (no `allow-forms`), C |
| `09-link-stylesheet.html` | `<link rel=stylesheet>`, `preload`, `prefetch`, `dns-prefetch`, `preconnect`, `icon` | fetch, prefetch, or DNS-resolve `tracker.invalid` | render the canary unstyled | P, C |
| `10-css-url-style-attr.html` | `url()` in `style` attributes: `background`, `background-image`, `list-style-image`, `cursor`, `border-image`, CSS-escaped `\75 rl(`, `image-set()` | fetch any `10-*` resource | **apply inline styles**: the canary is green, bold, and bordered | C |
| `11-css-url-style-element.html` | `url()` in a `<style>` block: background, `@font-face src`, `content: url()`, `mask-image` | fetch any `11-*` resource | **keep the `<style>` block**: the canary is dark red | C |
| `12-css-import.html` | `@import url()`, `@import "string"`, protocol-relative `@import` with a media query | fetch any `12-*` stylesheet | apply the `p` rule after the imports: the canary is dark blue | C |
| `13-body-background.html` | legacy `background=` attribute on `<body>`, `<table>`, `<td>`; `background-image` on body | fetch any `13-*` image. DOMPurify allows `background`, so CSP is the only wall | render on the light sheet: a surviving `background` counts as the mail's own colors | C |
| `14-tracking-pixel.html` | 1×1 `<img>` over http, https, and protocol-relative URLs; a visible remote logo; `<video poster>`; `<audio src>`; an image inside `display:none` | fetch any `14-*` URL before click-to-load | render the canary. The visible logo appears as a placeholder, and click-to-load re-renders with a relaxed `img-src` | C |
| `15-cid-image.html` | `cid:` references: one present, one uppercase `CID:`, one missing, plus a remote pixel | fetch `15-pixel.gif`; request anything for the missing cid except the part endpoint | **load the `cid:logo@hostile.test` image from the part endpoint**. Also exists end to end as `hostile-15-cid@partner-agency.example` in the work maildir, with the part present | A, C |
| `16-head-style.html` | nothing hostile: a marketing layout that depends on head `<style>` and a media-query `<style>` | lose head styles, which is the reason `WHOLE_DOCUMENT` is on | **style the heading large and red, the lede grey and italic, the button blue**. `!important` survives | P config |
| `17-target-blank.html` | `target=_top`, `_parent`, `_self`, a named target, an in-page anchor | navigate the app window or the frame to a remote page; reuse a named window | **open the plain https link in a new browser tab**. Every link gets `target=_blank`. The anchor may scroll | A, S |
| `18-mxss-svg-p-style.html` | namespace confusion: `<svg></p><style><a id="</style><img onerror>">`. Masato Kinugawa's DOMPurify 2.0.0 bypass (Sep 2019, fixed in 2.0.1) | execute after serialize and re-parse | render the canary | S, P |
| `19-mxss-form-math.html` | nested-form namespace confusion: `<form><math><mtext></form><form><mglyph><style></math><img onerror>`. Michał Bentkowski, "Mutation XSS via namespace confusion – DOMPurify < 2.0.17 bypass", securitum.com (2020) | execute after re-parse | render the canary. Also exists end to end as `hostile-19-mxss@acc0unts-verify.example` in the personal maildir, QP-encoded | S, P |
| `20-mxss-mglyph-comment.html` | `<math><mtext><table><mglyph><style><!--</style><img title="--&gt;…onerror…">`. Gareth Heyes, "Bypassing DOMPurify again with mutation XSS", PortSwigger Research (2020), DOMPurify 2.2.2 bypass | execute after re-parse | render the canary | S, P |
| `21-noscript.html` | `<noscript><p title="</noscript><img onerror>">`, which parses differently with scripting on and off. From Masato Kinugawa's Google Search mXSS (2019). Also a `<noscript><style>` variant and a remote pixel inside noscript | execute. A sandbox without `allow-scripts` parses noscript content as markup, the opposite of DOMPurify's parse, which is exactly this mismatch | render the canary; fetch nothing | S, P, C |
| `22-template.html` | `<template>` containing script, onerror, and `@import`; declarative shadow DOM `<template shadowrootmode=open>` with onerror and `@import`; `<select><template>` | execute; fetch `22-*`; attach a shadow root that hides content from DOMPurify | render the canary and the light-DOM text | S, P, C |
| `23-math-vectors.html` | MathML: `href=javascript:` on `<math>`, `<maction xlink:href>`, `<math><style>`, `<annotation-xml encoding=text/html>` with img and style, `<mi><svg><style>` | execute | render the canary and the x² formula | S, P |
| `24-srcset.html` | remote `srcset` on `<img>`, `<picture><source>`, `w` descriptors with `sizes`, `<link imagesrcset>` | fetch any srcset candidate; a `data:` src doesn't stop the browser from preferring a `2x` candidate | render the three green squares from their `data:` src | C |
| `25-a-ping.html` | `<a ping>`, `<area ping>`, `attributionsrc` | POST a ping or attribution beacon to `tracker.invalid` on click | open the link in a new tab | P, C |
| `26-css-expression-binding.html` | IE `expression()`, `-moz-binding`, `behavior:url()`, `url(javascript:)` in both attributes and `<style>` | execute or fetch. These are dead in Chromium, and the case pins that | render the canary | C |
| `27-fixed-overlay.html` | a `position:fixed; inset:0; z-index:max` fake "session expired" sign-in overlay; absolute negative offsets; negative margins | cover or intercept anything outside the frame: message list, header, key handling | render inside the frame only. Covering the frame's own body is acceptable. Auto-sizing must not make the overlay fill the window | A |
| `28-wide-element.html` | a 100000px-wide `div`, `table`, and `img`; 5000 unbroken characters; a long `<pre>` | widen the app layout or add a horizontal scrollbar to the app | scroll horizontally inside the frame, or clip | A |
| `29-data-uri-image.html` | a `data:image/png` image; a `data:image/svg+xml` image containing `<script>`; a `data:text/html` link | run the SVG's script, since SVG in `<img>` is inert; navigate to the `data:text/html` link | **render both data: images**, allowed by `img-src data:` | P, C |
| `30-self-origin.html` | relative URLs such as `/api/tag?add=trash`, `/logout`, and `../../etc/passwd` in `<img>`, `<a>`, `<form>`. srcdoc inherits the app's base URL and the CSP allows `img-src` for the app origin | change state through a GET. Mutations are POST with Origin and token checks. Submit the form | render the canary. Broken images are fine | A (server), P |
| `31-deep-nesting.html` | 600 nested `div`s around an svg/p/style mXSS and 300 nested `table`s around the form/math vector. Motivated by DOMPurify's nesting-depth bypasses: GitHub advisories for CVE-2024-45801 and CVE-2024-47875, fixed in 2.5.4 and 3.1.3 | execute; hang the sanitizer | finish quickly and render the canary | S, P |
| `32-comments-cdata-pi.html` | `<!-->` short comment, comment text inside an attribute, `<![CDATA[` in SVG and in SVG `<title>`, `<?xml-stylesheet?>` and bogus processing instructions. DOMPurify's `SAFE_FOR_XML` class of issues (3.1.x) | execute; fetch `32-pi.css` | render the canary | S, P, C |
| `33-dom-clobbering.html` | `name`/`id` values that clobber document properties the parent's same-origin post-processing may touch: `querySelectorAll`, `getElementsByTagName`, `body`, `documentElement`, `cookie`, `defaultView` | break the link rewrite or frame auto-sizing. DOMPurify's `SANITIZE_DOM` strips these, and the parent should read via `Document.prototype` methods anyway | rewrite the final link to `target=_blank` and size the frame to its content | P, A |
| `34-srcdoc-breakout.html` | text and attribute values containing `"`, `onload=`, and `</iframe>`. Serialized text nodes do not escape `"` | break out of a `srcdoc="…"` attribute. The only safe assembly assigns `iframe.srcdoc` as a DOM property, never by string-concatenating HTML | render the quote characters as literal text | A |
| `35-css-app-paths.html` | CSS `url()` to app endpoints (`/status`, `/events`) in style attributes, a `<style>` block, `@media`, `image-set()`, a CSS escape, a custom property via `var()`, `/part/../status`, and a part that isn't this message's. Rendered with `remoteImages: true` | request any app endpoint, before or after click-to-load. When `img-src` still allowed `http:`, the app origin itself matched it | render the canary purple: the rest of the `<style>` block survives the rebuild | A, C |
| `36-css-loopback-lan.html` | CSS `url()` to `http://127.0.0.1:1/`, `http://192.168.0.1/`, and over https to `127.0.0.1`, `[::1]`, `localhost`, `pneu.localhost` on another port, `0x7f.1`, `2130706433`, an escaped `\75 rl(` to `10.0.0.1`, and an `@font-face` on loopback. Rendered with `remoteImages: true` | request any of those hosts. `http:` ones are also outside the CSP; the https ones are stopped only by the URL filter | render the canary teal | A, C |
| `37-img-private-ip.html` | `<img src>` to `http://10.0.0.1/`, `https://10.0.0.1/`, `169.254.169.254`, `[fe80::1]`, `localhost`, `LOCALHOST.`; a srcset candidate on `127.0.0.1`; a `<video poster>` on `192.168.1.1`. Rendered with `remoteImages: true` | request any of them; keep the `src` | render the canary and the `data:` srcset image | A, C |
| `38-receipt-pixels.html` | remote `<img>`s on `tracker.invalid` that are 1×1 or 0×0 by attribute, a lone `width=1`, 2×2 by inline style overriding larger attributes, `display:none`, `opacity:0`, `visibility:hidden`, inside a `display:none` wrapper, inside a `max-height:0` wrapper, and a 1×1 with a remote `srcset`; normal-sized images on real receipt-tracker hosts (Mailtrack, Superhuman with uppercase and a trailing dot, a Bananatag subdomain, protocol-relative Yesware); a Mixmax `srcset` candidate, a `<video poster>` on GMass, and HubSpot Sales hosts in a style attribute and a `<style>` block. Rendered with `remoteImages: true` | request any of them, or keep any `38-` URL. Every name but `remote.test` resolves to nothing under run.sh, so a leak shows in the net log without reaching the tracker | render the canary purple, a 1×1 `data:` image, the `data:` src under the dropped `srcset`, and ordinary remote images on `remote.test` (8px, 4px inside a styled wrapper, and 600×1 / 600×2 dividers: tiny means every declared dimension is tiny), which must load | A |
| `39-root-and-custom-props.html` | `style` (a Mailtrack and a loopback URL) and `background` (Yesware) on the `<html>` root itself, which a descendant-only query never visits; custom properties holding `image-set()` with a single-quoted URL, a single-quoted `url()`, and an escaped `\75 rl(`, applied through `var()` in `<style>` rules and style attributes; single-quoted `image-set()` to loopback; `@property` rules (top level and inside `@media`, syntax written `"\3c image>"` because DOMPurify drops a `<style>` holding a literal `<image>`) whose `initial-value` names Mailtrack, consumed through `var()`; escaped `u\72l(` and `\69mage-set(` in a `var()` fallback; `var()` fallbacks inside shorthands (`background`, `list-style`, and `background` in `@keyframes`), whose longhands read `''`; the same with a longhand overriding part of the shorthand (`marker` then `marker-mid`, `background` then `background-color`), which drops the value from `cssText` as well. Rendered with `remoteImages: true` | request any `39-` URL: a custom property keeps its raw tokens, so the declaration filter sees no URL until `var()` assembles one | render the canary blue through a plain `var(--ink)` custom property, so ordinary custom properties survive | A, C |
| `40-svg-presentation-function.html` | SVG presentation attributes naming Mailtrack or loopback: `mask`, `fill` with a fallback colour, a single-quoted `filter`, `clip-path`, `marker-start`, `cursor`, and an escaped `u\72l(` `mask`; an `@function` whose parameter default is a Mailtrack `url()`, called from a rule; an `@counter-style` with an image symbol; SVG `<style>` elements with `<g>`, `<desc>` and `<title>` children splitting a `/*…*/` comment, so the filter's `textContent` and the browser's own-text sheet disagree. Rendered with `remoteImages: true` | request any `40-` URL. Presentation attributes never show in `element.style`, and at-rule values outside `.style`/`.cssRules` are never walked, so each needs its own pass: every other attribute is checked, and unknown rule types are deleted | render the canary amber, and keep the same-document `fill="url(#fade)"` | A, C |
| `41-colors-stripped.html` | backgrounds that exist only as receipt-tracker or loopback `url()`s: a `<style>` `background` shorthand and `background-image`, an inline `background`, and a `bgcolor` holding a `url()`. Rendered with a theme | request any of them; count as the mail's own colors once the filter has dropped them. Detection reads the filtered declarations and attributes, and a shorthand's leftover `initial` longhands set nothing | render themed (the theme's `html{}` rule, no `sheet` class), with the rest of the `<style>` block applied | A |
| `42-bgcolor-attr.html` | a `bgcolor` on a table. Rendered with a theme | render in the app's colors | render on the light sheet (`#f2f2ef`, class `sheet`), sized with its border so nothing clips | A |
| `43-inline-color.html` | a `color:` in a style attribute. Rendered with a theme | render in the app's colors | render on the light sheet, the mail's color intact | A |
| `44-no-colors.html` | no colors: `transparent`, `inherit`, `currentcolor` and an empty `<font color>`. Rendered with a theme; also as `harness-theme-invalid` with a `--bg` carrying a CSS injection, and `harness-theme-missing` with no theme | leave the theme's values anywhere in the document unless each is a 6-digit hex (or `light`/`dark`) | render in the theme's bg, fg and accent; fall back to the light sheet when the theme is malformed or absent | A |
| `45-font-substitution.html` | Gmail's `font-family:sans-serif` span, a newsletter stack behind an unloadable web font, a serif and a mono stack, an `!important` Arial, a legacy `<font face>`, and an `@font-face` named Arial | rewrite serif or mono stacks, a family the sender put ahead of the stand-ins, `!important`, or an `@font-face` name | put the mail sans (Inter) ahead of the first sans stand-in (Arial, Helvetica, `sans-serif`…) in style attributes, `<style>` rules and `face` | A |
| `46-ldjson-markup.html` | an ld+json block whose JSON string holds `</script><img onerror>` (the parser ends the script there, so the img is markup), and a valid block whose `name` holds `<img onerror>` and an escaped `<\/script><svg onload>` | execute; take anything from the broken blocks | capture three blocks; yield the valid block's action, its name the markup as text, capped at 60 code points | S, P, A |
| `47-ldjson-huge.html` | a valid small block beside a block past 64 KiB | yield an action: past any bound there is none, not the small block's | render the canary | A |
| `48-ldjson-hidden.html` | ld+json inside a comment, `<template>`, `<noscript>`, `<textarea>`, `<xmp>`, `<svg>`, `<math><mi>`, and with types `application/json` and `application/ld+json;x`; one real block in head | capture any but the head block (DOMPurify's own parse is the truth: none of those is an HTML script in the document tree with exactly that type) | capture one block and yield its action; a second capture would make two actions, and so none | P, A |
| `49-ldjson-javascript-url.html` | the one declared action's `url` is `javascript:` | yield an action | render the canary | A |
| `50-ldjson-data-url.html` | the one declared action's `url` is `data:text/html` | yield an action | render the canary | A |
| `51-ldjson-relative-url.html` | the one declared action's `url` is relative (`/status`) | yield an action (with no base, a relative URL is refused, not resolved against the app) | render the canary | A |
| `52-ldjson-ip-url.html` | the one declared action's `url` is on `127.0.0.1` | yield an action | render the canary | A |
| `53-ldjson-github.html` | nothing hostile: GitHub's notification shape, an array holding an `EmailMessage` whose `potentialAction` is a `ViewAction` with both `url` and `target`, and a `publisher` | lose the action | yield `View Pull Request` and the pull request's URL | A |
| `54-cta-hidden.html` | 38 button-styled "Verify account" links above a real one, each hidden one way, arranged so that every check of the button heuristic is the only one catching at least one of them (so breaking any check fails the case): opacity 0.3, and 0.6 × 0.7 nested; `visibility:hidden` with only its text made visible; `display:none`; a 10px `overflow:hidden` window onto its middle; `clip-path`, `clip`, `mask-image`, `mix-blend-mode`, `filter` each leaving the centre hit-testable; zero size; `scale(0.02)`; 75% off the left edge; far off-screen; covered by a relatively positioned div (only the hit test sees it); `pointer-events:none`; 4px text; a 1px run of text inside; text at 0.3 opacity or under `filter` in a span; text the button's colour (hex, gradient, `oklab()`), nearly transparent, or `-webkit-text-fill-color: transparent`; text indented away; text centred past both edges of a narrow box; text covered while the box's centre isn't. Paint tricks: a `pointer-events:none` absolute cover; an absolute cover over part of the text away from every hit-tested point; an image inside the link over its text; a background image (gradient) on the link, on an ancestor over a colour that would pass, and on a span inside; a `::before` with content; a `::first-line` in the button's colour; the text in an `<svg>`; a running CSS animation. Image-only (alt text) variants where a text check would otherwise stand in | be a candidate at all | pick the real "Confirm your order" (its text colour `oklab()`, so a broken canvas conversion refuses it) | A |
| `55-cta-footer.html` | a button-styled "Verify account" to another host in the footer, under unsubscribe/privacy/help links, against the real button, which appears twice (two table-cell buttons to one URL) | win; count the real CTA's second button as its runner-up | pick the real destination (labelled by its best-scoring button) by the margin | A |
| `56-cta-repoint.html` | a single clear "Reset your password" button; the app checks then re-point, hide, fade, retext and remove it after the pick, and redraw the frame | open anything after any of those | pick it; open it while unchanged, and again once each change is undone | A |
| `57-cta-mismatch.html` | a button whose text says "Log in to paypal.com" and whose href goes to `login.evil.example` | show paypal.com as the destination | pick it, labelled with its text, the chip's destination `https://login.evil.example` | A |
| `58-cta-pseudo.html` | an empty positioned `::after` painting the button colour over a button's text, and a real button further down | be a candidate (the first by its pseudo-element; the real one because a positioned pseudo-element has no rect and counts as covering the document) | pick nothing | A |
| `59-cta-image-only.html` | a big, early, styled image link whose `alt` is "Shop the sale", and a styled button with no text | be picked (the alt is not a label; no text, no pick) | rank both as candidates, pick nothing | A |
| `60-cta-label-hosts.html` | "Track package" twice on real.example (different parameters) and once on other.example | merge other.example into real.example's choice | rank two choices, pick real.example's, other.example the runner-up | A |
| `61-cta-budget.html` | one link holding 2,100 separately drawn text nodes before a clear button | pick anything once the guess's budget (2,000 text ranges) is spent | report `over`, pick nothing | A |
| `62-quote-gmail.html` | a Gmail reply whose quote carries a script, an `onerror` image and its own `details ontoggle`, rendered with `fold` | run anything when the fold is made or opened; leave the quote showing closed | fold the quote (attribution included) behind the pill, show it once opened, keep the reply's link | P, A |
| `63-quote-forward.html` | a Gmail forward: the same `gmail_quote` markup, "Forwarded message" | fold the forwarded message | show it whole | A |
| `64-quote-inline.html` | an inline reply: a Thunderbird quote with the answer after it, then a trailing one | fold the first quote | fold only the trailing one, its `moz-cite-prefix` with it | A |
| `65-quote-outlook.html` | an Outlook reply: `#appendonsend`, the `<hr>`, `#divRplyFwdMsg` and the copy as siblings | leave the rule or the From:/Sent: block showing | fold from the rule to the end | A |
| `66-quote-all.html` | a body that is nothing but quote markup | fold it (a blank message) | show it | A |

## Sources

- DOMPurify's own regression suite, `test/fixtures/expect.mjs` (`expect.js` in older
  releases) at github.com/cure53/DOMPurify, covers most vectors in 01–05 and 21–23.
- Masato Kinugawa: DOMPurify 2.0.0 bypass (2019), and the Google Search noscript mXSS
  write-up (2019).
- Michał Bentkowski, "Mutation XSS via namespace confusion – DOMPurify < 2.0.17 bypass",
  securitum.com, 2020.
- Gareth Heyes, "Bypassing DOMPurify again with mutation XSS", portswigger.net/research,
  2020.
- GitHub security advisories for DOMPurify: CVE-2024-45801 and CVE-2024-47875, both about
  nesting depth.

Payloads were adapted to the `top.__pwned` / `tracker.invalid` conventions above. The
attribution for the cited bypasses is from memory of the public write-ups and was not
re-fetched. Check the linked sources before quoting them elsewhere.

## Keeping the maildir copies in sync

Two of these files are embedded in `testdata/mail` messages. `internal/testmail`'s
`TestHostileMessagesCarryCorpus` fails if a message's HTML part no longer matches its
corpus file. If you edit `15-cid-image.html` or `19-mxss-form-math.html`, update the
corresponding message too. The 19 message is quoted-printable encoded.

## Harness

`harness.html` renders every case through `web/static/mailframe.js` — the file the app
ships, loaded by relative path together with the vendored DOMPurify — and writes a
JSON verdict per case into `<pre id="results">`. `cases.json` lists the cases and the
per-case expectations (computed styles, images that must or must not load, links that
must survive, cid mappings, substrings that must be `absent` from the assembled
document, CSP violations that must be `blocked`, `partBase`, `remoteImages`). It also
carries harness-only cases that need the live port:

- `harness-remote-off` / `harness-remote-rerender`: a remote image blocked, then
  click-to-load re-rendering into the same frame. The remote host is
  `https://remote.test:<port>`: run.sh serves the tree over TLS with a throwaway
  self-signed cert and maps the name to 127.0.0.1 (`--host-resolver-rules`,
  `--ignore-certificate-errors`), because the relaxed `img-src` is `https:` only and
  IP literals and `localhost` are dropped by the URL filter. Case files name it as
  `https://remote.test/` and the harness fills in the port (38).
- `harness-cid-css`: `cid:` inside `srcset` and CSS `url()`.
- `harness-partbase`: `opts.partBase` narrowed to `/part/personal/m%40hostile.test/`;
  the message's own part loads, another part under `/part/` is dropped from both the
  `src` and CSS.
- `harness-key-forward`: keydown forwarding out of the frame.
- `harness-link-noopener`: the harness clicks a message link (`HTMLElement.click` from
  the parent realm; run.sh passes `--disable-popup-blocking`). The link opens
  `opener.html` on the harness origin, which can't be seen under `--dump-dom`, so it
  reports `window.opener` and `document.referrer` through `localStorage` (same origin,
  same profile) under the case id in its fragment. Must be: no opener, no referrer, the
  frame still at `about:srcdoc`, the harness URL unchanged.
- `harness-theme-invalid` / `harness-theme-missing`: 44 rendered with a malformed
  theme and with none; both must fall back to the light sheet. Cases carrying
  `theme` pass it as `opts.theme`; `colors`, `sheet` and `fits` assert the
  detection result, the frame mode and class, and that the frame's height
  covers its content, sheet border included, without clipping or slack.
- Quote-fold cases (62–66) carry `fold` (passed as `opts.fold`), `folded`, the
  expected `built.folded`, `hidden`, selectors the closed fold must hide
  (`checkVisibility`: a closed details still lays out its content), and
  `foldOpens`, a selector that must show once the fold is opened from the
  parent realm. `harness-quote-nofold` is 62 without `fold`: nothing folds.
  The frame growing to an opened fold is ResizeObserver's, which never fires
  under `--dump-dom`'s virtual time; it's checked in the app.
- Button-heuristic cases (54–61) carry `guess`, the pick `{text, host}`
  (text as the chip labels it) or null, from `Pneu.actions.guess` on the
  laid-out frame, and optionally `notCandidates`, hosts that must not be
  among the ranked candidates at all, `rankedHosts`, hosts that must be,
  `runnerUp`, the runner-up's host, and `over`, that the budget ran out;
  the frame's serialized document must be the same before and after. Every check in the heuristic was broken
  once to confirm a case fails (docs/actions.md).
- JSON-LD cases (46–53) carry `ldBlocks`, the number of ld+json blocks the sanitize
  hook captured, and `action`, what `declaredAction` (actions.js, loaded by the
  harness) made of them: `{url, name}` or null. Each also checks that the hook was
  removed after the call (DOMPurify's hooks are global).
- After the cases, checks of the sinks that put sender data into the app's own
  document, run in an app-shaped document (`#app`: a thread pane, the key bar
  and status line, the shipping `app.css`, `mailframe.js` and `actions.js`),
  with `window.open` intercepted, each reported like a case:
  `app-chip-sinks` (every chip the corpus yields contains only the chip's own
  elements and attributes, 46's markup shown as text, and its destination
  stays inside the chip and the pane at 600px and 180px, wrapping rather than
  clipped); `app-o-reveal` (`o` with the chip scrolled away reveals and opens
  nothing, a repeat does nothing, the next `o` opens); `app-hints-positioning`
  (fixed, absolute, transformed, negative-margin and huge links, 3-letter
  labels, and 2px slivers on each edge of the clip: every label's box inside
  it); `app-hints-frozen-url` (anchors re-pointed after hints open; the URL
  shown at selection is what opens, and Shift+Enter opens nothing);
  `app-hints-status-scroll` (a long destination scrolls by the status line's
  own scroll and the inspection keys without closing hints or reaching the
  app); `app-hints-yield-status` (another flash ends the session);
  `app-guess-repoint` (56's pick opens while unchanged; re-pointed,
  hidden, faded, retexted, removed or redrawn after the pick, `o` opens
  nothing and says why, and opens again once undone); `app-guess-chip` (57's
  chip is only the chip's own elements, dashed and tagged `guess`, its name
  the text and its destination the href's host; the highlight is in the app
  document, a layer exactly the frame's box that clips, its box over the
  link, hidden off the cursor message, gone on `stop`; the frame's document
  untouched; re-validated while it shows, it hides within 250ms of the link
  moving, being hidden or being removed, comes back on the next update, and
  isn't re-validated off the cursor message; its 250ms timers, counted by
  wrapping the app realm's `setTimeout`/`clearTimeout`, run only while it
  shows: none off the cursor (after `sync()`, or by itself without one),
  none while hidden, none after a forced throw in re-validation, none
  after `stop`); `app-hints-listeners` (three rounds of opening and ending hints every way,
  frame replacement included: every listener and ResizeObserver a session
  added is gone); `sanitize-throw-hook` (a sanitize call that throws still
  removes the capture hook); `sanitize-hook-identical` (every case's assembled
  document is byte-identical with the capture hook installed and without).
  Under the harness's virtual time no rendering step runs, so where the
  browser would fire a scroll event the harness dispatches one.
- Then the shipping `app.js` on the real thread page: `app-thread.html` is
  `base.html` and `thread.html` as the server renders them (checked in;
  `go test ./internal/web -run TestHarnessThreadPage` fails when the templates
  change, `-update-harness` regenerates it), loaded in a same-origin frame
  with a script ahead of the page's own that stands in for the network only
  (fetch answers the body URL with a mail carrying a JSON-LD action and a grid
  of long links, and 404s the rest; EventSource is inert; `window.open` is
  recorded). run.sh serves `/static/` and a fixed `/theme.css` for it. In the
  narrow layout (700px) and the split (1400px), keys dispatched on the thread
  pane and in the mail frame's document: `page-*-o-under-chrome` (a wrapped
  chip half under the fixed key bar, narrow, or the split's sticky title: the
  first `o` goes through `primary` and reveals for the bounds, the second
  opens); `page-*-o-covered` (another element over the chip's top row, its
  centre clear: no open); `page-*-o-from-frame`; `page-*-hints-status` (the
  selected label under the status line is hidden, every label that meets it
  stacks below it, and `Pneu.flash` ends the session so `Enter` opens
  nothing). Then `page-narrow-guess`: the body is 56 (no JSON-LD), and
  app.js must guess it on load: a guess chip, the highlight over the link
  on the cursor message, `o` opening it; after its href is changed in the
  frame, `o` opens nothing, the status line says `Not opened: …`, and the
  chip is guessed afresh with the new destination; `p` moves the cursor
  away and the highlight hides, its re-validation timer stopped. Then
  `page-narrow-text-guess`, on the same page: the first message's text
  body (a sign-in link above a signature with a link of its own) gets a
  guess chip named from its line and pointing at the sign-in link, the
  link marked in place on the cursor message only, `o` opens it, and once
  its href is changed `o` opens nothing and the chip is guessed afresh.
  Then `page-narrow-guess-throw`: the same page with the first computed style
  of a link in the frame forced to throw; the body still renders
  (`data-state=done`) with no chip, highlight, timer or page error.
- `harness-link-opener-control`: the positive control. The shipping assembly, then
  `rel=opener` put back on the link before `srcdoc`. `opener.html` sees an opener and
  uses it to navigate the frame (reverse tabnabbing); the case passes only if the
  harness detects both, which shows the negative case can fail.

`harness-pixel.svg` stands in for a message part.

Every case is also checked for: `top.__pwned` unset, the canary rendered, frame height
above zero, the frame still at `about:srcdoc`, the CSP `<meta>` first in `<head>`, no
foreign `<meta>`/`<base>`, no script/frame/plugin/form elements, no `on*` attributes,
and every link `target=_blank rel="noopener noreferrer"` with an http(s)/mailto href.
CSP violations are recorded per case from the parent realm (the `csp` column): a
listener on the frame's `contentWindow` right after insertion survives the srcdoc
commit (the Window of the initial `about:blank` is reused), so first renders are
complete. A re-render gets a new Window that is polled for, so it may miss early
violations.

`run.sh` serves the repo root on a throwaway 127.0.0.1 port (the harness origin plays
the app origin; `/part/` is a small tree with the pixel), loads the harness in headless
Chromium with a net log, and fails on:

- any failed case, or any mention of `tracker.invalid` or one of case 38's
  receipt-tracker hosts in the net log;
- any request to a host:port other than the two harness servers (catches 36, 37);
- any request the harness origin served outside the harness's own files,
  `/web/static/` and `/part/` — a message reaching an app endpoint such as `/status`
  or `/events` (30, 35) — and anything but the pixel on the https server.

Run it with `go test ./testdata/hostile` (`./...` skips testdata) or directly;
`CHROMIUM=` overrides the binary, `RUN_SH_KEEP=1` keeps the temp dir (net log,
`http.log`, dumped DOM). Chromium's own background requests to Google are reported,
not failed. Needs python3 and openssl.
