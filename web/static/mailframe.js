// mailframe.js — the one path by which message HTML reaches the screen.
//
//   raw html → DOMPurify (inert DOMParser document) → post-process the DOM
//   (cid:, remote-image policy, links) → CSP/referrer/base/style prepended to
//   <head> → serialize → iframe.srcdoc on a sandbox with no allow-scripts.
//
// Used by app.js and by testdata/hostile/harness.html; any change here is
// checked with testdata/hostile/run.sh (AGENTS.md). Case numbers below refer
// to testdata/hostile/README.md.
//
// Layers, in order of what they are trusted for:
//   sandbox — the script wall. Never add allow-scripts: with allow-same-origin
//             a single script in the frame owns the app (top.__pwned).
//   CSP     — the network wall. DOMPurify does not parse CSS, so every url()
//             in style attributes and <style> (10–13, 26) is stopped here.
//   DOMPurify — depth: drops script/handlers/javascript: (01–04, 18–23, 31, 32),
//             and the elements that would act before or around CSP (06, 07,
//             08, 09).
//   post-processing — link targets (17), cid rewrite (15), srcset (24),
//             same-origin GETs (30), sizing (27, 28, 33), srcdoc as a DOM
//             property only (34), loopback/LAN/app-path image and CSS URLs
//             (35–37), receipt pixels (38).
//
// Remote images load by default (app.js passes remoteImages except for spam
// and trash, which keep click-to-load). What that gives away is the open, so
// the obvious beacons go first: every URL on a read-receipt tool's host, and
// every remote <img> that is too small or hidden to be meant for reading
// (38). That is a courtesy, not a wall — a sender who hosts the pixel on
// their own domain at 8x8 still learns the open.
//
// Loading remote images is partial by construction: once img-src allows
// https:, a message can still make the browser fetch from any public host
// name, and a public name can resolve to a loopback or LAN address. The URL
// filter below drops what can be recognised from the URL alone (IP literals,
// localhost, *.localhost, app paths outside the message's parts). The
// complete fix is a server-side image proxy that resolves and checks the
// address itself (PLAN.md, Later). The same goes for redirects: only the
// first URL is checked here, and CSP allows any https: destination, so an
// accepted host can bounce the fetch to a receipt tracker.
(function () {
  'use strict';

  var Pneu = (window.Pneu = window.Pneu || {});

  var SANDBOX = 'allow-same-origin allow-popups allow-popups-to-escape-sandbox';
  var MAX_HEIGHT = 60000; // stops a vh-driven layout from growing the frame forever

  var PURIFY_CONFIG = {
    // Keep <html>/<head>/<body> so head <style> survives (16) — marketing mail
    // is built on it. RETURN_DOM hands back the <html> element of DOMPurify's
    // DOMParser document: no browsing context, so nothing in it fetches or
    // runs while we post-process.
    WHOLE_DOCUMENT: true,
    RETURN_DOM: true,
    FORBID_TAGS: [
      'meta', // refresh navigation, a competing CSP, set-cookie, referrer (06)
      'base', // href re-roots relative URLs at a tracker, target=_top (07)
      'link', // stylesheet/preload/prefetch/dns-prefetch/preconnect/icon (09, 24)
      'form', // phishing forms (08); the sandbox also lacks allow-forms
      // Form controls survive without a <form> in DOMPurify's default list; a
      // mail has no use for them, and a password box is the phishing surface
      // (08, 27). The text around them stays (KEEP_CONTENT).
      'input', 'button', 'textarea', 'select', 'option', 'optgroup', 'datalist', 'output',
      // Content that is never rendered but parses differently depending on
      // scripting (21) or hosts a declarative shadow root (22).
      'template', 'noscript',
    ],
    // Not in DOMPurify's default allowlist today; named so an upstream change
    // can't quietly admit beacons (25).
    FORBID_ATTR: ['ping', 'attributionsrc'],
    // DOMPurify's default minus tel/callto/sms/ftp/xmpp/matrix: links may only
    // be http(s)/mailto (enforced again below), images http(s)/cid (data: for
    // images is DATA_URI_TAGS, separate). Relative and fragment URLs still
    // match here and are handled in post-processing.
    ALLOWED_URI_REGEXP: /^(?:(?:https?|mailto|cid):|[^a-z]|[a-z+.\-]+(?:[^a-z+.\-:]|$))/i,
    ALLOW_DATA_ATTR: false,
    // SANITIZE_DOM (default on) strips name/id values that clobber document
    // properties (33); left at its default deliberately.
  };

  // Read through prototype getters so named elements in a message can never
  // shadow what the parent reads (33). Same-origin frames give us their
  // objects; the parent realm's getters work on them.
  function getter(proto, name) {
    return Object.getOwnPropertyDescriptor(proto, name).get;
  }
  var docElement = getter(Document.prototype, 'documentElement');
  var docBody = getter(Document.prototype, 'body');
  var scrollHeight = getter(Element.prototype, 'scrollHeight');
  var clientHeight = getter(Element.prototype, 'clientHeight');
  var qsa = Element.prototype.querySelectorAll;
  var rect = Element.prototype.getBoundingClientRect;
  var getAttr = Element.prototype.getAttribute;
  var setAttr = Element.prototype.setAttribute;
  var removeAttr = Element.prototype.removeAttribute;
  var removeEl = Element.prototype.remove;
  var parentEl = getter(Node.prototype, 'parentElement');
  var htmlStyle = getter(HTMLElement.prototype, 'style');
  var attrsOf = getter(Element.prototype, 'attributes');
  var firstChildEl = getter(Element.prototype, 'firstElementChild');
  var hasOwn = Object.prototype.hasOwnProperty;

  function all(root, selector) {
    return Array.prototype.slice.call(qsa.call(root, selector));
  }
  // all() plus the root itself: <html style> and <html background> are
  // attributes like any other (39).
  var matches = Element.prototype.matches;
  function allSelf(root, selector) {
    return (matches.call(root, selector) ? [root] : []).concat(all(root, selector));
  }

  function cspFor(origin, remoteImages, partBase) {
    // Only this message's parts, not the whole origin: a same-origin GET to
    // anything else is a request the message has no business making. No
    // http: even after click-to-load: the app origin, loopback and the LAN
    // are plain http, and the message's CSS would reach every one of them.
    var img = 'img-src ' + origin + partBase + ' data:' + (remoteImages ? ' https:' : '');
    return "default-src 'none'; " + img + "; style-src 'unsafe-inline'; font-src data:";
  }

  // opts.partBase must be an app path under /part/ ending in '/', already
  // percent-encoded (callers escape the account and Message-ID), and stable
  // under URL normalisation, so a Message-ID of '..' can't turn the prefix
  // into something else. Anything else falls back to the whole part
  // endpoint. It lands in a CSP source expression, so ';', ',', quotes and
  // whitespace are refused outright.
  var PART_BASE_OK = /^\/part\/[A-Za-z0-9\-._~%!$&()*+=:@\/]*\/$/;
  function partBaseFor(raw, origin) {
    if (typeof raw !== 'string' || !PART_BASE_OK.test(raw)) return '/part/';
    try {
      if (new URL(raw, origin).pathname !== raw) return '/part/';
    } catch (e) { return '/part/'; }
    return raw;
  }

  // Hosts no mail image may name: IP literals of any range, and localhost.
  // The URL parser has already canonicalised 0x7f.1, 2130706433 and friends
  // to dotted quads, and IPv6 to [..].
  function localHost(h) {
    h = String(h).toLowerCase().replace(/\.$/, '');
    return h === 'localhost' || /\.localhost$/.test(h) || h.charAt(0) === '[' ||
      /^\d+\.\d+\.\d+\.\d+$/.test(h);
  }

  // Read-receipt tools: hosts that serve nothing but open pixels, so every
  // image from them goes (38) and none of it is ever content. Bulk senders'
  // hosts (list-manage.com, sendgrid.net, HubSpot's marketing CDN) are left
  // off on purpose: the same hosts carry the logos, and blocking them breaks
  // the mail. So are tools whose pixel sits on a path of their main domain
  // (Mailtrack's mailtrack.io/trace, Saleshandy, Polymail, ContactMonkey,
  // NetHunt): a host rule would take their signature logos too. A match is
  // the host itself or any subdomain of it.
  //
  // Sources, fetched 2026-09-24: OneClickLab/ugly-email-trackers list.txt
  // (last changed 2022-05-24), leavemealone-app/email-trackers trackers.txt
  // (2021-07-23), trockerapp/trocker chrome/lists.js (2026-01-26).
  var RECEIPT_HOSTS = [
    'mltrk.io',                       // Mailtrack
    'r.superhuman.com',               // Superhuman
    't.yesware.com',                  // Yesware
    'mailfoogae.appspot.com',         // Streak
    'bl-1.com',                       // Bananatag
    'track.mixmax.com', 'email.mixmax.com', // Mixmax
    'tracking.cirrusinsight.com',     // Cirrus Insight
    'email81.com',                    // GetNotify
    'outrch.com',                     // Outreach
    'salesloftlinks.com',             // Salesloft
    'closeml.com',                    // Close
    'mailstat.us',                    // Boomerang
    'yamm-track.appspot.com',         // Yet Another Mail Merge
    'track.gmass.co', 'gmtrack.net',  // GMass
    't.mailpgn.com',                  // Pigeon
    'signaldomn.online',              // Snov.io
    'infinite-stream-5194.herokuapp.com', // PersistIQ
    'tracking.vocus.io',              // Vocus
    'pixel.watch',                    // Trocker lists it as "CM"
    'track.getsidekick.com', 't.hubspotemail.net', // HubSpot Sales (Sidekick)
  ];
  // HubSpot Sales also rotates through numbered and spelled-out families of
  // t.* hosts (Trocker's list, 'SK').
  (function () {
    function pad(i) { return (i < 10 ? '0' : '') + i; }
    var i;
    for (i = 1; i <= 12; i++) RECEIPT_HOSTS.push('t.sidekickopen' + pad(i) + '.com');
    for (i = 1; i <= 5; i++) RECEIPT_HOSTS.push('t.sigopn' + pad(i) + '.com');
    for (i = 1; i <= 13; i++) RECEIPT_HOSTS.push('t.strk' + pad(i) + '.email');
    ['uno', 'dos', 'tres', 'quatro', 'cinco'].forEach(function (n) { RECEIPT_HOSTS.push('t.senal' + n + '.com'); });
    ['una', 'due', 'tre', 'quattro', 'cinque'].forEach(function (n) { RECEIPT_HOSTS.push('t.signale' + n + '.com'); });
    ['un', 'deux', 'trois', 'quatre', 'cinq', 'six', 'sept', 'huit', 'neuf', 'dix'].forEach(function (n) {
      RECEIPT_HOSTS.push('t.signaux' + n + '.com');
    });
  })();

  function receiptHost(h) {
    h = String(h).toLowerCase().replace(/\.$/, '');
    for (var i = 0; i < RECEIPT_HOSTS.length; i++) {
      var d = RECEIPT_HOSTS[i];
      if (h === d || h.slice(-d.length - 1) === '.' + d) return true;
    }
    return false;
  }

  // cid:X → part URL, null for a cid with no part, undefined for not-a-cid.
  // The scheme is case-insensitive (15's CID:); the id is RFC 2392
  // URL-encoded, so try it raw and decoded.
  function cidTarget(raw, cids) {
    var m = /^\s*cid:\s*<?([^>\s]*)>?\s*$/i.exec(raw);
    if (!m) return undefined;
    var id = m[1], dec = id;
    try { dec = decodeURIComponent(id); } catch (e) { /* keep raw */ }
    if (hasOwn.call(cids, id)) return cids[id];
    if (hasOwn.call(cids, dec)) return cids[dec];
    return null;
  }

  // Classifies an image-ish URL: returns the URL to keep, or null to drop it.
  //   cid:        → mapped part URL, or drop
  //   data:       → keep (img-src data:; 29)
  //   same origin → drop unless it is one of this message's part URLs, under
  //                 partBase. Mail has no business loading app URLs, so
  //                 /status or /events would otherwise fire as GETs (30, 35).
  //                 Relative URLs resolve against the app page (about:srcdoc
  //                 inherits its base), so they land here too (07).
  //   loopback, LAN, localhost → drop (36, 37): IP literals of any range,
  //                 localhost and *.localhost (pneu on other ports too).
  //   receipt tracker host → drop (38), in every place a URL is checked:
  //                 src, srcset, poster, background, CSS url().
  //   remote      → keep; CSP blocks it unless remoteImages (14). srcset
  //                 candidates are handled by the caller.
  function imageURL(raw, ctx) {
    var t = cidTarget(raw, ctx.cids);
    if (t === null) return null;
    var v = t !== undefined ? String(t) : String(raw).trim();
    if (t === undefined && /^data:/i.test(v)) return v;
    var u;
    try { u = new URL(v, ctx.base); } catch (e) { return null; }
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
    if (u.origin === ctx.origin) {
      var mine = t !== undefined || ctx.parts[u.href];
      return mine && u.pathname.indexOf(ctx.partBase) === 0 ? v : null;
    }
    if (localHost(u.hostname)) return null;
    if (receiptHost(u.hostname)) return null;
    return v;
  }

  function isRemote(url, ctx) {
    if (!url || /^(data|cid):/i.test(url)) return false;
    try { return new URL(url, ctx.base).origin !== ctx.origin; } catch (e) { return false; }
  }

  // Minimal srcset tokenizer: a candidate is a URL (a run of non-space,
  // data: URLs included) plus descriptors up to the next comma.
  function parseSrcset(s) {
    var out = [], i = 0, n = s.length;
    while (i < n) {
      while (i < n && /[\s,]/.test(s[i])) i++;
      if (i >= n) break;
      var start = i;
      while (i < n && !/\s/.test(s[i])) i++;
      var url = s.slice(start, i), desc = '';
      if (/,+$/.test(url)) {
        url = url.replace(/,+$/, '');
      } else {
        var d = i;
        while (i < n && s[i] !== ',') i++;
        desc = s.slice(d, i).trim();
      }
      out.push({ url: url, desc: desc });
    }
    return out;
  }

  // cid: inside CSS url() — rewrite to the part, or to none. Cheap regex; a
  // miss only means a broken image (CSP never allows cid:).
  var CSS_CID = /url\(\s*(['"]?)\s*cid:([^'")\s]*)\s*\1\s*\)/gi;
  function rewriteCssCids(css, ctx) {
    return css.replace(CSS_CID, function (_, q, id) {
      var t = cidTarget('cid:' + id, ctx.cids);
      return t ? 'url("' + String(t).replace(/["\\\n]/g, '') + '")' : 'none';
    });
  }
  // Anything in CSS that could fetch from outside: url() that isn't data:/cid:,
  // image-set() with bare strings, @import.
  var CSS_REMOTE = /url\(\s*['"]?\s*(?!data:|cid:)[^\s'")]|image-set\(|@import/i;

  // CSS URL filter (35, 36). DOMPurify does not parse CSS and a regex can't
  // see through escapes (\75 rl(), so the browser's own CSS parser does it:
  // style attributes through the inert element's CSSStyleDeclaration, <style>
  // blocks through a constructed CSSStyleSheet (which never fetches and
  // ignores @import). Each declaration whose value fetches is checked with
  // imageURL; a failing one is removed whole.
  //
  // Values come back serialised: url("…") always quoted, escapes decoded.
  // Except where a substitution is involved: a value using var()/attr()/
  // env() is stored as raw tokens, escapes and fallbacks included (so a
  // backslash marks one too), and its URL is only assembled at
  // computed-value time. Such a value goes if it looks like it could fetch
  // at all (39).
  var CSS_FETCHES = /url\(|image-set\(|image\(|src\(|cross-fade\(/i;
  var CSS_OPAQUE = /var\(|attr\(|env\(|\\/i;
  var CSS_STRING = /"((?:[^"\\]|\\[\s\S])*)"|'((?:[^'\\]|\\[\s\S])*)'/g;
  // A custom property keeps its value as raw tokens too, and fetches only
  // once var() puts it somewhere, where this filter no longer sees a URL
  // (39). Both go on this broader test.
  var CSS_MAY_FETCH = /url|image|src|fade|\\/i;
  function cssValueOK(value, ctx) {
    if (CSS_OPAQUE.test(value)) return !CSS_MAY_FETCH.test(value);
    if (!CSS_FETCHES.test(value)) return true;
    if (/url\(\s*[^"\s]/i.test(value)) return false; // unquoted: custom-property token soup
    CSS_STRING.lastIndex = 0;
    for (var m; (m = CSS_STRING.exec(value));) {
      var s = (m[1] !== undefined ? m[1] : m[2]).replace(/\\([0-9a-f]{1,6})\s?/gi, function (_, hex) {
        var cp = parseInt(hex, 16);
        return cp > 0 && cp <= 0x10ffff ? String.fromCodePoint(cp) : '�';
      }).replace(/\\([\s\S])/g, '$1');
      if (imageURL(s, ctx) === null) return false;
    }
    return true;
  }
  // Body colors (SPEC.md, Thread page): a message that sets a background or
  // a text color anywhere was designed against its own palette, so it keeps
  // a light sheet; one that sets neither renders in the app's colors. Read
  // off the filtered declarations, so a background whose url() the filter
  // dropped doesn't count. A shorthand leaves its unset longhands at
  // 'initial', and CSS-wide keywords set nothing, so neither counts.
  var COLOR_PROPS = ['color', 'background-color', 'background-image'];
  var NO_COLOR = /^(?:initial|inherit|unset|revert|revert-layer|none|transparent|currentcolor|rgba\(0, 0, 0, 0\))$/i;
  function declaresColor(style) {
    return COLOR_PROPS.some(function (p) {
      var v = style.getPropertyValue(p).trim();
      return v !== '' && !NO_COLOR.test(v);
    });
  }
  // A shorthand holding var() leaves every longhand pending substitution:
  // listed, but reading '' (39). Its value is nowhere this filter can read —
  // cssText drops it too once a longhand overrides part of the shorthand —
  // so every one goes. var() inside a shorthand never survives; mail rarely
  // has it.
  function filterDeclarations(style, ctx) {
    var changed = false;
    for (var i = style.length - 1; i >= 0; i--) {
      var p = style.item(i), v = style.getPropertyValue(p);
      if (v === '' || (p.slice(0, 2) === '--' ? CSS_MAY_FETCH.test(v) : !cssValueOK(v, ctx))) {
        style.removeProperty(p);
        changed = true;
      }
    }
    return changed;
  }
  // Sans text renders in MAIL_SANS. The frame's font-src is data: only, so a
  // sender's web font never loads and nearly every stack ends at Arial,
  // Helvetica or sans-serif, which fontconfig answers with Liberation Sans:
  // hinted for Windows, soft at a fractional device scale. MAIL_SANS goes in
  // ahead of the first family in SANS_STAND_INS, so a font the sender named
  // earlier in the list still wins, and nothing is lost when it isn't
  // installed. Serif and monospace stacks are left as sent.
  var MAIL_SANS = 'Inter';
  var SANS_STAND_INS = ['sans-serif', 'system-ui', 'ui-sans-serif', '-apple-system',
    'blinkmacsystemfont', 'arial', 'helvetica', 'helvetica neue', 'helveticaneue',
    'liberation sans', 'verdana', 'tahoma', 'segoe ui', 'roboto', 'calibri',
    'trebuchet ms', 'open sans', 'lato'];
  function substituteSans(families) {
    var list = families.match(/"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^,]+/g) || [];
    for (var i = 0; i < list.length; i++) {
      var name = list[i].trim().replace(/^["']|["']$/g, '').toLowerCase();
      if (name === MAIL_SANS.toLowerCase()) return families;
      if (SANS_STAND_INS.indexOf(name) >= 0) {
        list.splice(i, 0, MAIL_SANS);
        return list.map(function (f) { return f.trim(); }).join(', ');
      }
    }
    return families;
  }
  function substituteFonts(style) {
    var v = style.getPropertyValue('font-family');
    if (v === '') return false;
    var next = substituteSans(v);
    if (next === v) return false;
    style.setProperty('font-family', next, style.getPropertyPriority('font-family'));
    return true;
  }

  // Rules the walk below can see all of: their values live in .style or
  // their children in .cssRules. Anything else goes whole, because it can
  // hold a value outside both — @property's initial-value, @function's
  // parameter defaults (39) — and so can whatever at-rule ships next.
  // @import and @namespace never get here (replaceSync drops @import).
  var RULE_TYPES = ['CSSStyleRule', 'CSSMediaRule', 'CSSSupportsRule',
    'CSSContainerRule', 'CSSLayerBlockRule', 'CSSLayerStatementRule',
    'CSSKeyframesRule', 'CSSKeyframeRule', 'CSSFontFaceRule', 'CSSPageRule',
    'CSSScopeRule', 'CSSStartingStyleRule', 'CSSNestedDeclarations']
    .map(function (n) { return window[n]; })
    .filter(function (c) { return typeof c === 'function'; });
  function knownRule(r) {
    return RULE_TYPES.some(function (c) { return r instanceof c; });
  }
  function filterRules(owner, ctx) {
    var changed = false, rules = owner.cssRules;
    for (var i = rules.length - 1; i >= 0; i--) {
      var r = rules[i];
      if (!knownRule(r)) {
        owner.deleteRule(i);
        changed = true;
        continue;
      }
      if (r.style && filterDeclarations(r.style, ctx)) changed = true;
      if (r.style && declaresColor(r.style)) ctx.colors = true;
      // An @font-face's font-family names the face it defines, not a stack.
      if (r.style && !(r instanceof CSSFontFaceRule) && substituteFonts(r.style)) changed = true;
      if (r.cssRules && filterRules(r, ctx)) changed = true;
    }
    return changed;
  }
  // The CSS text to keep: the original when nothing was removed. A rebuilt
  // sheet carries decoded escapes, so '<' is re-escaped: the text must never
  // close its <style> element when the srcdoc is parsed.
  function filterSheetText(css, ctx, rebuild) {
    var sheet = new CSSStyleSheet();
    try { sheet.replaceSync(css); } catch (e) { return ''; }
    if (!filterRules(sheet, ctx) && !rebuild) return css;
    return Array.prototype.map.call(sheet.cssRules, function (r) { return r.cssText; })
      .join('\n').replace(/</g, '\\3c ');
  }

  // Invisible remote images (38): an <img> no reader sees is a beacon.
  // Tiny is every declared dimension 3px or less (inline style over the
  // attribute), with at least one declared: a 600x1 divider stays, a lone
  // width=1 goes. Hidden is display:none, visibility:hidden, opacity:0 or a
  // zero width/height/max-width/max-height on the image or any ancestor's
  // inline style. Only inline styles are read: rules in <style> blocks
  // (a class that hides the pixel) would need the cascade, which an inert
  // document doesn't compute, so those pixels load.
  var TINY = 3;
  var LEN_ATTR = /^\s*(\d*\.?\d+)\s*(?:px)?\s*$/i;
  var LEN_CSS = /^(\d*\.?\d+)px$/; // CSSOM serialises 0 as 0px
  // A declared length in px, or null when absent or not a plain length.
  function length(v, re) {
    var m = re.exec(v || '');
    return m ? parseFloat(m[1]) : null;
  }
  function zero(v) {
    return v !== '' && parseFloat(v) === 0;
  }
  function hiddenBy(decl) {
    return decl.getPropertyValue('display') === 'none' ||
      decl.getPropertyValue('visibility') === 'hidden' ||
      zero(decl.getPropertyValue('opacity')) ||
      ['width', 'height', 'max-width', 'max-height'].some(function (p) {
        return zero(decl.getPropertyValue(p));
      });
  }
  function invisible(img, root) {
    var decl = htmlStyle.call(img);
    var dims = ['width', 'height'].map(function (p) {
      var css = length(decl.getPropertyValue(p), LEN_CSS);
      return css !== null ? css : length(getAttr.call(img, p), LEN_ATTR);
    }).filter(function (d) { return d !== null; });
    if (dims.length && dims.every(function (d) { return d <= TINY; })) return true;
    for (var el = img; el; el = el === root ? null : parentEl.call(el)) {
      if (el instanceof HTMLElement && hiddenBy(htmlStyle.call(el))) return true;
    }
    return false;
  }

  // Every other attribute (40): SVG presentation attributes (mask, fill,
  // filter, clip-path, marker-*, cursor…) are CSS values that fetch, and
  // element.style never shows them. One that could fetch goes, unless its
  // only references are same-document url(#id).
  var URL_ATTRS = { src: 1, poster: 1, background: 1, href: 1, 'xlink:href': 1, srcset: 1, style: 1 };
  var FRAGMENT_REF = /url\(\s*(['"]?)#[^'"()\\\s]*\1\s*\)/gi;
  function attrValueOK(v) {
    if (/\\/.test(v)) return !CSS_MAY_FETCH.test(v);
    v = v.replace(FRAGMENT_REF, '');
    return !/url\s*\(/i.test(v) && !CSS_FETCHES.test(v);
  }

  var LINK_OK = /^(?:https?:\/\/|mailto:)/i;

  function postProcess(root, ctx) {
    var remote = false;

    // Receipt pixels by shape, before anything counts them as remote. The
    // src and srcset are classified exactly as below; data:, cid: and part
    // images are never removed.
    all(root, 'img').forEach(function (el) {
      var src = getAttr.call(el, 'src'), srcset = getAttr.call(el, 'srcset');
      var urls = srcset ? parseSrcset(srcset).map(function (c) { return c.url; }) : [];
      if (src !== null) urls.push(src);
      var fetches = urls.some(function (u) {
        var v = imageURL(u, ctx);
        return v !== null && isRemote(v, ctx);
      });
      if (fetches && invisible(el, root)) removeEl.call(el);
    });

    // Images and everything else that fetches as an image under img-src.
    var urlAttrs = ['src', 'poster', 'background', 'href', 'xlink:href'];
    allSelf(root, '[src], [poster], [background], [href], [*|href]').forEach(function (el) {
      urlAttrs.forEach(function (name) {
        if ((name === 'href' || name === 'xlink:href') && /^(a|area)$/i.test(el.localName)) return;
        var raw = getAttr.call(el, name);
        if (raw === null) return;
        var v = imageURL(raw, ctx);
        if (v === null) removeAttr.call(el, name);
        else {
          if (v !== raw) setAttr.call(el, name, v);
          if (isRemote(v, ctx)) remote = true;
        }
      });
    });

    allSelf(root, '*').forEach(function (el) {
      Array.prototype.slice.call(attrsOf.call(el)).forEach(function (a) {
        if (!hasOwn.call(URL_ATTRS, a.name.toLowerCase()) && !attrValueOK(a.value)) removeAttr.call(el, a.name);
      });
    });

    // srcset: a data: src does not stop the browser from choosing a remote 2x
    // candidate, which CSP then blocks, leaving a broken image (24). Until
    // click-to-load, keep only local candidates, so the src shows.
    all(root, '[srcset]').forEach(function (el) {
      var kept = [];
      parseSrcset(getAttr.call(el, 'srcset')).forEach(function (c) {
        var v = imageURL(c.url, ctx);
        if (v === null) return;
        if (isRemote(v, ctx)) {
          remote = true;
          if (!ctx.remoteImages) return;
        }
        kept.push(c.desc ? v + ' ' + c.desc : v);
      });
      if (kept.length) setAttr.call(el, 'srcset', kept.join(', '));
      else removeAttr.call(el, 'srcset');
    });

    // CSS: cid rewrite, the URL filter, and a note that there is remote CSS
    // to unblock.
    allSelf(root, '[style]').forEach(function (el) {
      var css = getAttr.call(el, 'style');
      if (CSS_REMOTE.test(css)) remote = true;
      var next = rewriteCssCids(css, ctx);
      if (next !== css) setAttr.call(el, 'style', next);
      var decl = el.style;
      if (!decl || typeof decl.removeProperty !== 'function') {
        // No CSSOM for this element: keep nothing that could fetch.
        if (/url|image|src\(|\\/i.test(next)) removeAttr.call(el, 'style');
        return;
      }
      filterDeclarations(decl, ctx);
      if (declaresColor(decl)) ctx.colors = true;
      substituteFonts(decl);
    });
    all(root, 'font[face]').forEach(function (el) {
      var face = getAttr.call(el, 'face'), next = substituteSans(face);
      if (next !== face) setAttr.call(el, 'face', next);
    });
    all(root, 'style').forEach(function (el) {
      var css = el.textContent;
      // An SVG <style> can hold elements, and its sheet is built from its own
      // text nodes only, while textContent joins theirs in: a comment opened
      // in one child hides a rule from this filter, not from the browser
      // (40). Flatten, so the text checked is the text applied, and rebuild
      // it, which drops the comments the split was hiding behind.
      var split = !!firstChildEl.call(el);
      if (split) el.textContent = css;
      if (CSS_REMOTE.test(css)) remote = true;
      var next = filterSheetText(rewriteCssCids(css, ctx), ctx, split);
      if (next !== css) el.textContent = next;
    });

    // Links: only explicit http(s)/mailto survive, all open in a new tab
    // without opener or referrer (17). Relative and fragment hrefs are
    // dropped: they resolve against the app page (07, 30), and a fragment
    // can't scroll a frame that is sized to its content anyway.
    all(root, 'a, area').forEach(function (el) {
      ['href', 'xlink:href'].forEach(function (name) {
        var raw = getAttr.call(el, name);
        if (raw !== null && !LINK_OK.test(raw.trim())) removeAttr.call(el, name);
      });
      setAttr.call(el, 'target', '_blank');
      setAttr.call(el, 'rel', 'noopener noreferrer');
      removeAttr.call(el, 'ping');
      removeAttr.call(el, 'attributionsrc');
    });

    // Legacy color attributes, as they survived the filters above:
    // bgcolor/text on body, table and td, color on font, and a background
    // image, which assumes its own text color just as much.
    allSelf(root, '[bgcolor], [text], [color], [background]').forEach(function (el) {
      ['bgcolor', 'text', 'color', 'background'].forEach(function (name) {
        var v = getAttr.call(el, name);
        if (v !== null && v.trim() !== '') ctx.colors = true;
      });
    });

    return remote;
  }

  // The frame's html{} rule, one of two. Sheet: the mail's own palette on
  // an off-white page, the way it was designed. Themed: the app's colors,
  // which come from our stylesheet (app.js reads --bg/--fg/--accent and
  // color-scheme), never from mail; they are still held to a strict shape
  // before reaching CSS text, and anything else falls back to the sheet.
  var SHEET_RULE = 'html{background:#f2f2ef;color:#000;color-scheme:light;overflow-y:hidden}';
  var HEX = /^#[0-9a-fA-F]{6}$/;
  var SCHEME = /^(light|dark)$/;
  function themedRule(t) {
    if (!t || !HEX.test(t.bg) || !HEX.test(t.fg) || !HEX.test(t.accent) || !SCHEME.test(t.scheme)) return null;
    return 'html{background:' + t.bg + ';color:' + t.fg + ';color-scheme:' + t.scheme + ';overflow-y:hidden}' +
      'a{color:' + t.accent + '}';
  }
  var FRAME_STYLE =
    'body{margin:8px;font:14px/1.45 ' + MAIL_SANS + ',sans-serif;overflow-wrap:break-word}' +
    'img{max-width:100%;height:auto}' +
    'pre{white-space:pre-wrap}';

  // Builds the srcdoc string. Exposed for tests; callers want renderMailFrame.
  function assemble(html, cids, origin, opts) {
    opts = opts || {};
    cids = cids || {};
    var base = opts.base || document.baseURI;
    var parts = Object.create(null);
    Object.keys(cids).forEach(function (k) {
      try { parts[new URL(cids[k], base).href] = true; } catch (e) { /* skip */ }
    });
    var ctx = {
      cids: cids, origin: origin, base: base, parts: parts,
      partBase: partBaseFor(opts.partBase, origin),
      remoteImages: !!opts.remoteImages,
      colors: false, // set by postProcess: the mail declares its own colors
    };

    var root = DOMPurify.sanitize(String(html), PURIFY_CONFIG);
    var doc = root.ownerDocument;
    var remote = postProcess(root, ctx);
    var themed = ctx.colors ? null : themedRule(opts.theme);

    var head = all(root, 'head')[0];
    if (!head) {
      head = doc.createElement('head');
      root.insertBefore(head, root.firstChild);
    }
    var csp = cspFor(origin, ctx.remoteImages, ctx.partBase);
    function meta(attr, name, content) {
      var m = doc.createElement('meta');
      setAttr.call(m, attr, name);
      setAttr.call(m, 'content', content);
      return m;
    }
    var style = doc.createElement('style');
    style.textContent = (themed || SHEET_RULE) + FRAME_STYLE;
    var baseEl = doc.createElement('base');
    setAttr.call(baseEl, 'target', '_blank');
    // Inserted in reverse so the CSP meta ends up the first child of <head>,
    // before anything that could fetch. Our style comes before the mail's so
    // the mail's own rules win.
    [style, baseEl,
     meta('http-equiv', 'x-dns-prefetch-control', 'off'),
     meta('name', 'referrer', 'no-referrer'),
     meta('http-equiv', 'Content-Security-Policy', csp)].forEach(function (el) {
      head.insertBefore(el, head.firstChild);
    });

    return {
      srcdoc: '<!DOCTYPE html>' + root.outerHTML, csp: csp, remote: remote,
      colors: ctx.colors, sheet: !themed,
    };
  }

  function sizeFrame(frame) {
    var doc = frame.contentDocument;
    if (!doc) return;
    var html = docElement.call(doc);
    if (!html) return;
    // scrollHeight is the content extent (absolutely positioned overflow
    // included, 27) but never less than the viewport. If it exceeds the
    // viewport the content grew and it is exact. Otherwise the content may
    // have shrunk: measure against a zero-height viewport. That also means
    // vh/100%-height layouts and inset:0 overlays (27) measure as nothing
    // instead of feeding back into the frame height. The parent's min-height
    // holds the app's layout still while the frame is momentarily 0 tall.
    // The sheet's border sits inside a border-box height; margins sit
    // outside it and need nothing.
    var cs = getComputedStyle(frame), edge = 0;
    if (cs.boxSizing === 'border-box') {
      edge = ['borderTopWidth', 'borderBottomWidth', 'paddingTop', 'paddingBottom']
        .reduce(function (n, p) { return n + (parseFloat(cs[p]) || 0); }, 0);
    }
    var viewport = frame.clientHeight;
    var h = scrollHeight.call(html);
    if (h <= viewport) {
      var holder = frame.parentNode, held = holder && holder.style.minHeight;
      if (holder) holder.style.minHeight = holder.getBoundingClientRect().height + 'px';
      frame.style.height = edge + 'px';
      h = scrollHeight.call(html);
      if (holder) holder.style.minHeight = held;
    }
    h = Math.min(Math.max(h, 1), MAX_HEIGHT) + edge;
    frame.style.height = h + 'px';
    // A mail too wide for the column scrolls sideways inside the frame (28);
    // give the scrollbar its own room so it doesn't cover the last line.
    var bar = frame.clientHeight - clientHeight.call(html);
    if (bar > 0) frame.style.height = Math.min(h + bar, MAX_HEIGHT) + 'px';
  }

  function attach(frame) {
    var ro = null;
    frame.addEventListener('load', function () {
      var doc = frame.contentDocument;
      if (!doc) return;
      if (ro) ro.disconnect();
      ro = new ResizeObserver(function () { sizeFrame(frame); });
      var body = docBody.call(doc);
      if (body) ro.observe(body);
      ro.observe(docElement.call(doc));
      sizeFrame(frame);
      // Keystrokes land in the frame once it has focus; hand them to the app.
      // The listener lives in the parent's realm, so the sandbox's disabled
      // scripting doesn't apply to it.
      if (frame.__pneuKeys) doc.addEventListener('keydown', frame.__pneuKeys);
      if (frame.__pneuKeyup) doc.addEventListener('keyup', frame.__pneuKeyup);
    });
  }

  // renderMailFrame(html, cids, origin, opts) → { frame, remote }
  //   html    raw message HTML (untrusted)
  //   cids    { "<content-id without brackets>": "/part/…/n" }
  //   origin  the app origin, for img-src and the same-origin checks
  //   opts.remoteImages  relax img-src to https: (the default in app.js;
  //                      spam and trash only after a click)
  //   opts.partBase      this message's part prefix, e.g.
  //                      '/part/personal/' + encodeURIComponent(msgid) + '/'
  //                      (matching the server's url.PathEscape). Narrows
  //                      img-src and the same-origin filter; default /part/.
  //   opts.frame         re-render into this existing iframe
  //   opts.onKeydown     receives keydown events from inside the frame
  //   opts.onKeyup       receives keyup events from inside the frame
  //   opts.base          base URL relative URLs resolve against (default: document.baseURI)
  //   opts.theme         { bg, fg, accent: '#rrggbb', scheme: 'light'|'dark' }:
  //                      the app's colors, for a mail that declares none.
  //                      Missing or malformed: the light sheet.
  // remote is true when the message references remote images or CSS; colors
  // when it declares its own background or text color; sheet when the frame
  // renders as the light sheet (colors, or no usable theme), which is also
  // the frame's class.
  Pneu.renderMailFrame = function (html, cids, origin, opts) {
    opts = opts || {};
    var built = assemble(html, cids, origin, opts);
    var frame = opts.frame;
    if (!frame) {
      frame = document.createElement('iframe');
      frame.className = 'mail';
      frame.title = 'Message body';
      attach(frame);
    }
    if (opts.onKeydown) frame.__pneuKeys = opts.onKeydown;
    if (opts.onKeyup) frame.__pneuKeyup = opts.onKeyup;
    // Attributes before srcdoc: sandbox flags are captured at navigation.
    frame.setAttribute('sandbox', SANDBOX);
    frame.setAttribute('referrerpolicy', 'no-referrer');
    frame.classList.toggle('sheet', built.sheet);
    // The only unsafe sink: a DOM property, never string-built markup (34).
    frame.srcdoc = built.srcdoc;
    return { frame: frame, remote: built.remote, colors: built.colors, sheet: built.sheet };
  };

  Pneu.assembleMailDocument = assemble;
  Pneu.MAILFRAME_SANDBOX = SANDBOX;
})();
