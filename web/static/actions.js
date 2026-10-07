// actions.js — message actions that act on what the sender wrote
// (docs/actions.md): the unsubscribe confirmation (X), the declared
// primary link (o: JSON-LD, shown as a chip), link hints (L), and the
// pieces they share: the browser-open check and the arming rule for
// confirming controls. The pure half also runs under node --test
// (web/actions.test.js); the DOM half only in the browser.
//
// The sender is the adversary. Every sender-supplied string reaches this
// document through textContent, each in its own <bdi>, with format and bidi
// controls drawn visibly, and capped. Nothing runs that the user didn't see
// in full and confirm.
(function (root) {
  'use strict';

  // ---- pure -----------------------------------------------------------------

  var CF = /[\p{Cf}\p{Cs}\u2028\u2029]/u;

  // shown reports whether code point ch is drawn as an escape: format and
  // bidi controls, lone surrogates, line/paragraph separators, and C0/C1
  // controls (a multiline field keeps its newlines and tabs).
  function shown(ch, multiline) {
    var c = ch.codePointAt(0);
    if (c < 0x20 || (c >= 0x7f && c <= 0x9f)) return !(multiline && (c === 0x0a || c === 0x09));
    return CF.test(ch);
  }

  function escapeCp(ch) {
    var h = ch.codePointAt(0).toString(16).toUpperCase();
    while (h.length < 4) h = '0' + h;
    return '\\u{' + h + '}';
  }

  // segments splits s into plain runs {t} and escaped controls {c}.
  function segments(s, multiline) {
    var out = [], run = '';
    for (var ch of String(s == null ? '' : s)) {
      if (shown(ch, multiline)) {
        if (run) { out.push({ t: run }); run = ''; }
        out.push({ c: escapeCp(ch) });
      } else {
        run += ch;
      }
    }
    if (run) out.push({ t: run });
    return out;
  }

  // visible is segments as one string, as the page draws it.
  function visible(s, multiline) {
    return segments(s, multiline).map(function (g) { return g.t != null ? g.t : g.c; }).join('');
  }

  // cap cuts s to n code points (before escaping), marking a cut with '…'.
  function cap(s, n) {
    var a = Array.from(String(s == null ? '' : s));
    if (a.length <= n) return { text: a.join(''), cut: false };
    return { text: a.slice(0, n).join('') + '…', cut: true };
  }

  // Caps in code points. Labels are cut with a visible '…'. Fields that say
  // what will be sent or opened (url, subject, body) are never cut: past
  // their cap the confirmation isn't offered, since a cut would hide part of
  // what runs.
  // The recipient is never cut either: the server allows at most 254
  // octets, and the dialog wraps it in full.
  var CAPS = { context: 200, account: 100, signedBy: 253, from: 320, destination: 300 };
  var WHOLE = { url: 4096, subject: 2048, body: 16384, to: 254 };

  var IPV4_LAST = /^(?:\d+|0x[0-9a-f]*)$/i;

  // browserCheck is the one check every browser-opened destination passes
  // (unsubscribe open-in-tab, o, link hints) at the moment of opening:
  // http(s) only, not the app's origin, not an IP literal, not localhost,
  // *.localhost or a single-label host. ok carries the normalized URL, which
  // is what is shown and what opens.
  function browserCheck(raw, appOrigins) {
    var u;
    try { u = new URL(String(raw)); } catch (e) { return { ok: false, reason: 'not a web address' }; }
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return { ok: false, reason: 'not a web address' };
    var origins = [].concat(appOrigins || []);
    for (var i = 0; i < origins.length; i++) {
      if (origins[i] && u.origin === origins[i]) return { ok: false, reason: 'points at pneu itself' };
    }
    var h = u.hostname.toLowerCase().replace(/\.+$/, '');
    if (!h) return { ok: false, reason: 'no host' };
    // The URL parser has already turned any numeric host (0x7f.1, 2130706433)
    // into dotted IPv4; a last label that is a number is one regardless.
    if (h.charAt(0) === '[' || IPV4_LAST.test(h.split('.').pop())) return { ok: false, reason: 'an IP address' };
    if (h === 'localhost' || /\.localhost$/.test(h)) return { ok: false, reason: 'a local address' };
    if (h.indexOf('.') < 0) return { ok: false, reason: 'a single-label host' };
    // https://github.com@evil.example/ reads as the first name and goes to
    // the second.
    if (u.username || u.password) return { ok: false, reason: 'a user name in the address' };
    return { ok: true, url: u.href };
  }

  // destination is what the one-click line shows, derived from the URL that
  // will be POSTed: scheme and host, the port only when it isn't 443 (URL
  // drops default ports), punycode as the parser leaves it.
  function destination(raw) {
    try {
      var u = new URL(String(raw));
      if (u.protocol !== 'https:' && u.protocol !== 'http:') return null;
      return u.protocol + '//' + u.host;
    } catch (e) { return null; }
  }

  // Arming: a confirming control acts only once its dialog has rendered and
  // the key that brought it up has been released. Until then every key and
  // click is dropped; repeats always are. init is that key's event ({code,
  // key}), or null when a click brought it up.
  function Arming(init) {
    this.code = (init && init.code) || '';
    this.key = (init && init.key) || '';
    this.released = !init;
    this.drawn = false;
  }
  Arming.prototype.rendered = function () { this.drawn = true; };
  Arming.prototype.keyup = function (e) {
    if (this.released || !e) return;
    if (this.code ? e.code === this.code : String(e.key).toLowerCase() === this.key.toLowerCase()) this.released = true;
  };
  Arming.prototype.armed = function () { return this.drawn && this.released; };
  Arming.prototype.accept = function (e) { return !(e && e.repeat) && this.armed(); };

  // Keys that inspect the armed dialog natively: Tab moves between the
  // disclosure and the scrollable fields; the rest scroll the focused one.
  var SCROLL = { ArrowUp: 1, ArrowDown: 1, ArrowLeft: 1, ArrowRight: 1, PageUp: 1, PageDown: 1, Home: 1, End: 1, ' ': 1 };

  // dialogKey decides a keydown inside the dialog (which never lets one
  // reach the app's map). armed: the dialog's arming (Arming.armed());
  // keys: the phase's key map; target: 'button', 'summary' or 'other', what
  // has focus. Returns
  //   'act'    run keys[e.key]
  //   'press'  Enter/Space on a focused button: run that button's action
  //   'native' leave the browser's default (focus, toggle, scroll)
  //   'drop'   nothing
  // Nothing but 'drop' before arming; acting never on a repeat.
  function dialogKey(e, armed, keys, target) {
    if (!armed || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return 'drop';
    var k = e.key;
    if (keys && Object.prototype.hasOwnProperty.call(keys, k) && typeof keys[k] === 'function') return e.repeat ? 'drop' : 'act';
    if (k === 'Tab') return 'native'; // from any focus, a button's included
    var press = k === 'Enter' || k === ' ';
    if (target === 'button') return press && !e.repeat ? 'press' : 'drop';
    if (target === 'summary' && press) return e.repeat ? 'drop' : 'native';
    if (SCROLL[k] === 1) return 'native';
    return 'drop';
  }

  // previewURL is the GET for msgid (as data-msgid carries it: already
  // url.PathEscape'd by the server), and the option after index.
  function previewURL(account, msgid, after) {
    return '/unsubscribe/' + encodeURIComponent(account) + '/' + msgid +
      (after != null ? '?after=' + encodeURIComponent(after) : '');
  }

  // ---- declared action (o) ------------------------------------------------
  // schema.org JSON-LD, as mailframe.js captured it from DOMPurify's own
  // parse: the text of each <script type="application/ld+json"> in the
  // document tree. The sender's suggestion, never evidence: the chip shows
  // where it goes, derived from the URL that opens.

  var LD = { blocks: 4, bytes: 65536, nodes: 2000, depth: 6 };
  // The action types that name a page to open, with a label for one that
  // has no name of its own.
  var ACTION_TYPES = { ViewAction: 'View', TrackAction: 'Track', ConfirmAction: 'Confirm', SaveAction: 'Save', RsvpAction: 'RSVP' };
  var NAME_CAP = 60;
  var own = Object.prototype.hasOwnProperty;

  function utf8Length(s) {
    if (s.length * 3 <= LD.bytes) return s.length; // short enough either way
    var n = 0;
    for (var i = 0; i < s.length; i++) {
      var c = s.charCodeAt(i);
      if (c < 0x80) n += 1;
      else if (c < 0x800) n += 2;
      else if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length && (s.charCodeAt(i + 1) & 0xfc00) === 0xdc00) { n += 4; i++; }
      else n += 3;
    }
    return n;
  }

  // actionType is t's fallback label if t (an @type: a string or a list of
  // them) names one of ACTION_TYPES, bare or as a schema.org IRI.
  function actionType(t) {
    var list = Array.isArray(t) ? t : [t];
    for (var i = 0; i < list.length; i++) {
      if (typeof list[i] !== 'string') continue;
      var n = list[i].replace(/^https?:\/\/schema\.org\//, '').replace(/^schema:/, '');
      if (own.call(ACTION_TYPES, n)) return ACTION_TYPES[n];
    }
    return null;
  }

  // ldURL is raw as an absolute http(s) URL that passes browserCheck, or
  // null. Stricter than the URL parser, which takes https:host and
  // backslashes, and drops tabs and newlines from inside a URL; with no
  // base, a relative URL is refused outright. No whitespace of any kind
  // (\s is Unicode's, U+0085 and U+180E besides), no C0 or C1 control, no
  // format character (Cf) and no lone surrogate: none of them belongs in
  // an address, and each can make one read as another.
  var LD_URL_BAD = /[\u0000-\u0020\u007f-\u009f\\\s\u180e\p{Cf}\p{Cs}]/u;
  function ldURL(raw, appOrigins) {
    if (typeof raw !== 'string' || !/^https?:\/\/[^\/]/i.test(raw) || LD_URL_BAD.test(raw)) return null;
    if (Array.from(raw).length > WHOLE.url) return null;
    var c = browserCheck(raw, appOrigins);
    return c.ok ? c.url : null;
  }

  // ldAction resolves one qualifying action to {url, name}, or null: url,
  // or target as a string or {url}; both given, they must agree. A
  // urlTemplate refuses it: that's a form to fill, not a link.
  function ldAction(a, appOrigins) {
    if (own.call(a, 'urlTemplate')) return null;
    var urls = [];
    if (own.call(a, 'url')) urls.push(a.url);
    if (own.call(a, 'target')) {
      var t = a.target;
      if (t && typeof t === 'object' && !Array.isArray(t)) {
        if (own.call(t, 'urlTemplate') || !own.call(t, 'url')) return null;
        urls.push(t.url);
      } else {
        urls.push(t);
      }
    }
    if (!urls.length) return null;
    var url = null;
    for (var i = 0; i < urls.length; i++) {
      var u = ldURL(urls[i], appOrigins);
      if (!u || (url && u !== url)) return null;
      url = u;
    }
    var name = typeof a.name === 'string' ? a.name.replace(/[\t\n\f\r ]+/g, ' ').trim() : '';
    if (!name) name = actionType(a['@type']);
    return { url: url, name: name }; // the full name: duplicates compare on it
  }

  // declaredAction is the message's one declared action, {url, name}, or
  // null. blocks: the captured script texts in document order (anything but
  // a string is a block the capture refused). Past any bound (4 blocks,
  // 64 KiB each, 2000 JSON nodes, depth 6) there is no action at all: what
  // the walk didn't see might have been a second one. potentialAction is
  // read on any object; exactly one qualifying action (identical duplicates
  // count once), else none. A block that isn't JSON contributes nothing.
  function declaredAction(blocks, appOrigins) {
    if (!Array.isArray(blocks) || blocks.length > LD.blocks) return null;
    var found = [], nodes = 0, OVER = {};
    function walk(v, depth) {
      if (++nodes > LD.nodes || depth > LD.depth) throw OVER;
      if (v === null || typeof v !== 'object') return;
      if (Array.isArray(v)) {
        for (var i = 0; i < v.length; i++) walk(v[i], depth + 1);
        return;
      }
      var keys = Object.keys(v);
      for (var k = 0; k < keys.length; k++) walk(v[keys[k]], depth + 1);
      if (!own.call(v, 'potentialAction')) return;
      [].concat(v.potentialAction).forEach(function (a) {
        if (a && typeof a === 'object' && !Array.isArray(a) && actionType(a['@type'])) found.push(a);
      });
    }
    try {
      for (var b = 0; b < blocks.length; b++) {
        if (typeof blocks[b] !== 'string' || utf8Length(blocks[b]) > LD.bytes) return null;
        var v;
        try { v = JSON.parse(blocks[b]); } catch (e) { continue; }
        walk(v, 1);
      }
    } catch (e) {
      if (e === OVER) return null;
      throw e;
    }
    // Identical duplicates count once, compared before the name is capped:
    // two names that differ only past the cap are two actions.
    var one = null;
    for (var i = 0; i < found.length; i++) {
      var r = ldAction(found[i], appOrigins);
      if (!r || (one && (r.url !== one.url || r.name !== one.name))) return null;
      one = r;
    }
    return one && { url: one.url, name: cap(one.name, NAME_CAP).text };
  }

  // Click-tracking redirectors known to sit in front of the real page. Any
  // host may redirect; these are the ones the chip says so for.
  var REDIRECTORS = ['list-manage.com', 'sendgrid.net', 'mandrillapp.com', 'mailchi.mp'];
  function redirector(raw) {
    var h;
    try { h = new URL(String(raw)).hostname.toLowerCase().replace(/\.+$/, ''); } catch (e) { return false; }
    return REDIRECTORS.some(function (d) { return h === d || h.slice(-d.length - 1) === '.' + d; });
  }

  // chipParts is what the chip draws for action: the name (capped by
  // declaredAction), the destination (scheme, host, port unless default)
  // and whether the host is a known redirector; null if it can't be shown.
  function chipParts(action) {
    if (!action || typeof action.url !== 'string') return null;
    var dest = destination(action.url);
    if (!dest) return null;
    return { name: String(action.name || ''), dest: cap(dest, CAPS.destination).text, redirect: redirector(action.url) };
  }

  // ---- button heuristic (o, tier 2) -----------------------------------------
  // With no declared action, a guess from the rendered body: the one link
  // that looks like the message's call to action. A sender can manufacture
  // any score, so the guess is only ever a suggestion shown as one (the chip
  // says "guess", its destination is the href's), made only among links the
  // user can see, and re-validated when o opens it (docs/actions.md).
  //
  // Every number the pick depends on is in CTA, so an evaluation over real
  // mail (scripts/ctaeval) can tune it in one place. A feature is a boolean
  // (counted 1) or a fraction; the score is the weighted sum.
  var CTA = {
    max: 500,          // anchors considered, in document order
    minFont: 9,        // px: smaller text is hidden text
    minContrast: 1.25, // text/background contrast ratio (WCAG) below which text is hidden
    threshold: 6,      // the pick's score must reach this, its text must lead with a verb,
    margin: 2,         // and beat the best other choice by this; within it, two verb-led
                       // candidates both over threshold go to the earlier (the primary
                       // button comes first: measured on real mail, 2026-09-30)
    budget: {          // one guess's work; past any of it, no pick for the message
      nodes: 20000,    // elements and text nodes visited (ancestors, subtrees, the overlay scan)
      ranges: 2000,    // text ranges measured
      styles: 20000,   // computed styles looked up (each element's once per guess)
    },
    weights: {
      styled: 3,     // a background of its own, or the sole content of a cell with one
      padding: 1,    // padded like a button (its own, or that cell's)
      radius: 1,     // rounded corners
      border: 0.5,   // a border all round (an outline button)
      verb: 3,       // its text starts with an action verb (CTA_VERBS)
      short: 1,      // 1-5 words
      long: -2,      // more than 8 words: a sentence, not a button
      early: 2,      // × how early it sits (1 at the top, 0 at the bottom)
      area: 2,       // × its area over the largest candidate's
      footer: -8,    // footer text (CTA_FOOTER): sinks even a styled, early verb
      imageOnly: -3, // no drawn text (an image; its alt is never a label): never picked
      tail: -3,      // in the last quarter of the document
      rawURL: -3,    // its text is an address
    },
  };

  var VERBS = [
    'view', 'confirm', 'verify', 'track', 'pay', 'reset', 'sign in', 'log in', 'login', 'accept', 'join',
    'review', 'download', 'open', 'get started', 'activate', 'complete', 'continue', 'shop', 'read', 'reply',
    'see', 'start', 'claim', 'schedule', 'book', 'rsvp', 'check', 'go to', 'retrieve', 'solve', 'apply',
    'share', 'print', 'buy', 'take', 'register', 'update (?:your )?(?:card|payment|billing)', 'manage (?:your )?(?:order|booking|reservation|trip|account)',
  ].join('|');
  var CTA_VERBS = new RegExp('^(?:' + VERBS + ')(?![\\p{L}\\p{N}])', 'u');
  // A verb anywhere in running text, at a word's start (a text body's line).
  var CTA_VERB_IN = new RegExp('(?:^|[^\\p{L}\\p{N}])((?:' + VERBS + ')(?![\\p{L}\\p{N}]))', 'iu');
  // "manage" alone is a footer word; managing an order or a trip is not.
  var CTA_FOOTER = new RegExp('(?:^|[^\\p{L}\\p{N}])(?:' + [
    'unsubscribe', 'opt[ -]?out', 'preferences', 'privacy', 'terms', 'legal',
    'view (?:it |this |the )?(?:e-?mail |message |newsletter )?(?:in (?:your |a )?(?:web )?browser|online|as a web ?page)',
    'web version', 'download (?:the |our )?(?:[\\p{L}]+ )?app', 'download to track', 'get the app',
    'manage(?! (?:your )?(?:order|booking|reservation|trip|account))', 'help', 'contact', 'support', 'faq',
    'facebook', 'twitter', 'instagram', 'linkedin', 'youtube', 'tiktok', 'pinterest', 'threads', 'bluesky',
    'mastodon', 'app store', 'google play', 'download on the', 'get it on', 'forward to a friend',
    'update (?:your )?(?:e-?mail )?(?:preferences|profile|settings)', 'careers', 'about us',
  ].join('|') + ')(?![\\p{L}\\p{N}])', 'u');
  var CTA_RAW_URL = /^(?:https?:\/\/|www\.)\S*$|^[\w-]+(?:\.[\w-]+)+(?:\/\S*)?$/i;

  // ctaText classifies a link's visible text (whitespace runs collapsed):
  // its word count, a leading action verb (after any leading symbols: an
  // arrow or an emoji), footer words anywhere, and whether it is an address.
  function ctaText(s) {
    var t = String(s == null ? '' : s).replace(/\s+/g, ' ').trim().toLowerCase();
    var words = t ? t.split(' ').length : 0;
    var lead = t.replace(/^[^\p{L}\p{N}]+/u, '');
    return {
      words: words,
      verb: CTA_VERBS.test(lead),
      footer: CTA_FOOTER.test(t) || t === 'x',
      rawURL: CTA_RAW_URL.test(t),
    };
  }

  // ---- text bodies (o, tier 3) ----------------------------------------------
  // A text/plain body has no styling to score, so its guess reads the words
  // around each link instead (docs/actions.md, Primary link). lines is the
  // body as rendered (textLines): [{text, quoted, links: [{url, start, end,
  // ...}]}], start and end offsets into text. Only the sender's own lines
  // count: not quoted ones, nothing from a signature ("-- ") or a forwarded
  // message on. A link's context is its line less every link on it, or,
  // when that leaves no letter, the nearest non-blank line above, if that
  // line has no link of its own. Links whose context has footer words go;
  // links to one URL are one choice (the one with a verb in its context
  // stands for it). The pick is the only choice left, or else the only one
  // whose context has an action verb; anything else is no pick. Its name
  // is the context from that verb to the end of its clause, its first
  // letter capitalized, or "Open link" when there is none, capped like a
  // declared action's. Returns {url,
  // name, link} (link the lines' own entry) or null.
  var TEXT_STOP = /^(?:--|-- |-{3,} ?(?:forwarded message|original message) ?-{3,})$/i;
  function textPick(lines) {
    var choices = [], byURL = Object.create(null);
    var stripped = function (l) {
      var t = '', at = 0;
      (l.links || []).forEach(function (k) { t += l.text.slice(at, k.start) + ' '; at = k.end; });
      return (t + l.text.slice(at)).replace(/\s+/g, ' ').trim();
    };
    for (var i = 0; i < lines.length; i++) {
      var l = lines[i];
      if (l.quoted) continue;
      if (TEXT_STOP.test(l.text.trim())) break;
      if (!l.links || !l.links.length) continue;
      var ctx = stripped(l);
      if (!/\p{L}/u.test(ctx)) {
        ctx = '';
        for (var j = i - 1; j >= 0; j--) {
          if (lines[j].quoted) break;
          if (!/\S/.test(lines[j].text)) continue;
          if (!lines[j].links || !lines[j].links.length) ctx = stripped(lines[j]);
          break;
        }
      }
      var t = ctaText(ctx), verb = CTA_VERB_IN.exec(ctx);
      for (var k = 0; k < l.links.length; k++) {
        var c = { url: l.links[k].url, link: l.links[k], footer: t.footer, verb: verb ? ctx.slice(verb.index + verb[0].length - verb[1].length) : null };
        var had = byURL[c.url];
        if (!had) { byURL[c.url] = c; choices.push(c); continue; }
        if (c.footer) had.footer = true; // one URL in a footer anywhere is a footer link
        if (!had.verb && c.verb) { had.verb = c.verb; had.link = c.link; }
      }
    }
    var left = choices.filter(function (c) { return !c.footer; });
    var pick = left.length === 1 ? left[0] : null;
    if (!pick) {
      var verbs = left.filter(function (c) { return c.verb; });
      if (verbs.length === 1) pick = verbs[0];
    }
    if (!pick) return null;
    var name = pick.verb ? pick.verb.split(/[.:;!?](?:\s|$)/)[0].trim() : '';
    name = name.charAt(0).toUpperCase() + name.slice(1);
    return { url: pick.url, name: cap(name || 'Open link', NAME_CAP).text, link: pick.link };
  }

  // ctaScore is features' weighted sum: {score, parts: {feature: points}}.
  function ctaScore(f, weights) {
    weights = weights || CTA.weights;
    var score = 0, parts = {};
    Object.keys(weights).forEach(function (k) {
      var v = f[k];
      v = typeof v === 'number' ? v : v ? 1 : 0;
      if (!v) return;
      parts[k] = weights[k] * v;
      score += parts[k];
    });
    return { score: Math.round(score * 1000) / 1000, parts: parts };
  }

  // ctaRank scores candidates ({url, text, f: features with area in px²,
  // anything else carried through}) and picks one. area becomes a fraction
  // of the largest. Candidates are one choice when they share a destination
  // (a button and its text link) or a label on one host (the same "Track
  // package" twice with different tracking parameters), transitively: the
  // best-scoring stands for them, so a CTA isn't its own runner-up. One
  // label on two hosts is two candidates, so a link elsewhere can't absorb
  // the real one; they compete on the margin. The pick is the best if it
  // leads with a verb, has text (never an image alone), reaches the
  // threshold and beats the best other choice by the margin; within the
  // margin of another such, the earlier in the document; else none. opts
  // overrides CTA's threshold, margin and weights.
  function ctaRank(cands, opts) {
    opts = opts || {};
    var threshold = opts.threshold != null ? opts.threshold : CTA.threshold;
    var margin = opts.margin != null ? opts.margin : CTA.margin;
    var weights = opts.weights || CTA.weights;
    var maxArea = 0;
    cands.forEach(function (c) { if (c.f.area > maxArea) maxArea = c.f.area; });
    var rs = cands.map(function (c, i) {
      var f = {};
      Object.keys(c.f).forEach(function (k) { f[k] = c.f[k]; });
      f.area = maxArea > 0 ? c.f.area / maxArea : 0;
      var s = ctaScore(f, weights);
      var r = {};
      Object.keys(c).forEach(function (k) { r[k] = c[k]; });
      r.f = f; r.score = s.score; r.parts = s.parts; r.index = i;
      return r;
    });
    // Union-find over the candidates, joined by destination and by
    // (host, label); a group's best-scoring member (the earlier on a tie)
    // stands for it.
    var up = rs.map(function (_, i) { return i; });
    var find = function (i) { while (up[i] !== i) { up[i] = up[up[i]]; i = up[i]; } return i; };
    var owner = Object.create(null);
    rs.forEach(function (r, i) {
      var keys = ['u\n' + r.url];
      var label = own.call(r, 'text') && r.text ? String(r.text).replace(/\s+/g, ' ').trim().toLowerCase() : '';
      var host = hostOf(r.url);
      if (label && host) keys.push('l\n' + host + '\n' + label);
      keys.forEach(function (k) {
        if (!(k in owner)) { owner[k] = i; return; }
        var x = find(i), y = find(owner[k]);
        if (x !== y) up[Math.max(x, y)] = Math.min(x, y);
      });
    });
    var best = Object.create(null), roots = [];
    rs.forEach(function (r, i) {
      var k = find(i);
      if (!(k in best)) { best[k] = r; roots.push(k); } else if (r.score > best[k].score) best[k] = r;
    });
    var ranked = roots.map(function (k) { return best[k]; });
    ranked.sort(function (a, b) { return b.score - a.score || a.index - b.index; });
    var top = ranked[0] || null, next = ranked[1] || null;
    var ok = function (r) { return r.score >= threshold && r.f.verb && !r.f.imageOnly && /\S/.test(r.text || ''); };
    var pick = null;
    if (top && ok(top)) {
      if (!next || top.score - next.score >= margin) pick = top;
      else if (ok(next)) pick = next.index < top.index ? next : top;
    }
    return { ranked: ranked, pick: pick, runnerUp: pick === next ? top : next };
  }

  function hostOf(u) {
    try { return new URL(u).hostname.toLowerCase(); } catch (e) { return ''; }
  }

  // contrast is the WCAG contrast ratio of two opaque [r, g, b] colors.
  function contrast(a, b) {
    function lum(c) {
      var l = c.slice(0, 3).map(function (v) {
        v /= 255;
        return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
      });
      return 0.2126 * l[0] + 0.7152 * l[1] + 0.0722 * l[2];
    }
    var x = lum(a), y = lum(b);
    return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
  }

  // over composites [r, g, b, a] (a 0-1) onto an opaque [r, g, b].
  function over(top, base) {
    var a = top[3];
    return [0, 1, 2].map(function (i) { return top[i] * a + base[i] * (1 - a); });
  }

  // ---- link hints (L) -----------------------------------------------------

  var HINT_ALPHABET = 'asdfghjkl';
  var HINT_MAX = 200;

  // hintLabels is n labels over alphabet, all the same length (so none is
  // a prefix of another), the shortest that fits.
  function hintLabels(n, alphabet) {
    alphabet = alphabet || HINT_ALPHABET;
    var k = alphabet.length, len = 1, room = k;
    while (room < n) { len++; room *= k; }
    var out = [];
    for (var i = 0; i < n; i++) {
      var s = '', x = i;
      for (var j = 0; j < len; j++) { s = alphabet.charAt(x % k) + s; x = Math.floor(x / k); }
      out.push(s);
    }
    return out;
  }

  // intersect is the overlap of two rects ({left, top, right, bottom}), or
  // null when it has no area.
  function intersect(a, b) {
    if (!a || !b) return null;
    var r = {
      left: Math.max(a.left, b.left), top: Math.max(a.top, b.top),
      right: Math.min(a.right, b.right), bottom: Math.min(a.bottom, b.bottom),
    };
    return r.right > r.left && r.bottom > r.top ? r : null;
  }

  // hintURL is a link's destination as a hint shows and opens it: http(s)
  // as browserCheck normalizes it ({kind: 'web', url}, or with refused: the
  // reason, shown and never opened), or a mailto ({kind: 'mailto', url});
  // null for anything else, which gets no label.
  function hintURL(raw, appOrigins) {
    if (typeof raw !== 'string') return null;
    var s = raw.trim();
    if (/^mailto:/i.test(s)) {
      try { new URL(s); } catch (e) { return null; }
      // Labelled and refused, as an overlong http(s) link is: it can't be
      // shown in full, and a link that just went unlabelled would look
      // like no link at all.
      if (Array.from(s).length > WHOLE.url) return { kind: 'mailto', refused: 'too long to show in full', url: s };
      return { kind: 'mailto', url: s };
    }
    if (!/^https?:\/\//i.test(s)) return null;
    var c = browserCheck(s, appOrigins);
    if (!c.ok) return { kind: 'web', refused: c.reason, url: s };
    if (Array.from(c.url).length > WHOLE.url) return { kind: 'web', refused: 'too long to show in full', url: c.url };
    return { kind: 'web', url: c.url };
  }

  // The keys that inspect a long destination on the status line.
  var HINT_SCROLL = {
    ArrowDown: ['line', 1], ArrowUp: ['line', -1], PageDown: ['page', 1], PageUp: ['page', -1],
    End: ['end', 1], Home: ['end', -1],
  };

  // placeLabel is where a label of size w × h goes for a link whose visible
  // box is box, in coordinates relative to clip's top-left: at the box's
  // top-left, moved in until the whole label lies inside clip, so a link
  // with a sliver showing still gets a label that can be read. null when
  // the label can't fit inside clip at all.
  function placeLabel(box, clip, w, h) {
    var cw = clip.right - clip.left, ch = clip.bottom - clip.top;
    if (!(w > 0 && h > 0) || w > cw || h > ch) return null;
    return {
      left: Math.min(Math.max(box.left - clip.left, 0), cw - w),
      top: Math.min(Math.max(box.top - clip.top, 0), ch - h),
    };
  }

  // inside reports whether rect r (with area) lies wholly within b, to half
  // a pixel (layout rounds).
  function inside(r, b) {
    if (!r || !b || !(r.right > r.left && r.bottom > r.top)) return false;
    var E = 0.5;
    return r.left >= b.left - E && r.top >= b.top - E && r.right <= b.right + E && r.bottom <= b.bottom + E;
  }

  // usable is the part of pane (a rect) that shows: pane ∩ view, less the
  // app's own sticky or fixed chrome (covers: rects) that overlaps it, each
  // taken off the edge it is anchored to: a bar across the top (the narrow
  // layout's header, the split's sticky title) lowers the top, one along
  // the bottom (the key bar) raises the bottom. The result may have no
  // area; inside() then fails for everything.
  function usable(pane, view, covers) {
    var b = {
      left: Math.max(pane.left, view.left), top: Math.max(pane.top, view.top),
      right: Math.min(pane.right, view.right), bottom: Math.min(pane.bottom, view.bottom),
    };
    (covers || []).forEach(function (c) {
      if (!c || !(c.right > b.left && c.left < b.right && c.bottom > b.top && c.top < b.bottom)) return;
      if (c.top <= b.top) b.top = Math.max(b.top, c.bottom);
      else if (c.bottom >= b.bottom) b.bottom = Math.min(b.bottom, c.top);
      else if (c.top - b.top < b.bottom - c.bottom) b.top = Math.max(b.top, c.bottom);
      else b.bottom = Math.min(b.bottom, c.top);
    });
    return b;
  }

  // primaryKey decides an o on a message's chip (docs/actions.md, Primary
  // link): a repeat does nothing; while the chip (rect, in the same
  // coordinates as bounds: the thread pane ∩ the viewport, above the key
  // bar and below the sticky title) isn't wholly in view it is 'reveal' —
  // scroll the chip into view and mark it — and only an o with the chip in
  // view is 'open'. So the destination the chip previews has always been
  // on screen when o opens it, from the pane or from inside a frame.
  function primaryKey(e, rect, bounds) {
    if (e && e.repeat) return 'none';
    return inside(rect, bounds) ? 'open' : 'reveal';
  }

  // hintKey is the hint session's key state machine. st: {typed, selected}
  // (selected: a label, or null); labels: the frozen list. Every key is the
  // session's; act says what to do with it:
  //   close   Esc
  //   open    Enter with a selection
  //   select  the typed label is complete: st.selected is it
  //   narrow  typed is a prefix of some labels (Backspace included)
  //   scroll  an inspection key: scroll the status line's destination by
  //           {by: 'line'|'page'|'end', dir: 1|-1}, never the page
  //   none    dropped: repeats, modifiers (Shift included: Shift+Enter
  //           opens nothing), keys that match nothing
  function hintKey(st, e, labels) {
    var next = { typed: st.typed, selected: st.selected };
    if (e.repeat || e.isComposing) return { st: next, act: 'none' };
    if (e.key === 'Escape') return { st: next, act: 'close' };
    if (e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return { st: next, act: 'none' };
    if (own.call(HINT_SCROLL, e.key)) return { st: next, act: 'scroll', by: HINT_SCROLL[e.key][0], dir: HINT_SCROLL[e.key][1] };
    if (e.key === 'Enter') return { st: next, act: next.selected ? 'open' : 'none' };
    if (e.key === 'Backspace') {
      if (!next.typed) return { st: next, act: 'none' };
      return { st: { typed: next.typed.slice(0, -1), selected: null }, act: 'narrow' };
    }
    if (typeof e.key !== 'string' || e.key.length !== 1 || HINT_ALPHABET.indexOf(e.key) < 0) return { st: next, act: 'none' };
    // A letter after a selection starts a new label.
    var typed = (next.selected ? '' : next.typed) + e.key;
    if (labels.indexOf(typed) >= 0) return { st: { typed: typed, selected: typed }, act: 'select' };
    if (labels.some(function (l) { return l.indexOf(typed) === 0; })) return { st: { typed: typed, selected: null }, act: 'narrow' };
    return { st: next, act: 'none' };
  }

  var A = {
    segments: segments, visible: visible, cap: cap, CAPS: CAPS, WHOLE: WHOLE,
    browserCheck: browserCheck, destination: destination, Arming: Arming, previewURL: previewURL,
    dialogKey: dialogKey,
    LD: LD, declaredAction: declaredAction, redirector: redirector, chipParts: chipParts,
    CTA: CTA, ctaText: ctaText, textPick: textPick, ctaScore: ctaScore, ctaRank: ctaRank, contrast: contrast, over: over,
    HINT_ALPHABET: HINT_ALPHABET, HINT_MAX: HINT_MAX, hintLabels: hintLabels, intersect: intersect,
    hintURL: hintURL, hintKey: hintKey, placeLabel: placeLabel, inside: inside, primaryKey: primaryKey, usable: usable,
  };
  if (typeof module === 'object' && module.exports) {
    module.exports = A;
    return;
  }

  var Pneu = (root.Pneu = root.Pneu || {});
  Pneu.actions = A;

  // ---- DOM ------------------------------------------------------------------

  function appOrigins() {
    var o = [location.origin];
    if (document.body && document.body.dataset.origin) o.push(document.body.dataset.origin);
    return o;
  }

  // openInBrowser opens url in the default browser's tab if browserCheck
  // passes; returns the check.
  function openInBrowser(url) {
    var c = browserCheck(url, appOrigins());
    if (c.ok) window.open(c.url, '_blank', 'noopener,noreferrer');
    return c;
  }
  A.openInBrowser = openInBrowser;

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  // field is one untrusted string: its own isolated <bdi>, controls drawn as
  // escapes. n caps a label; whole (a limit) refuses rather than cuts.
  function field(s, opts) {
    opts = opts || {};
    var b = el('bdi', 'untrusted' + (opts.cls ? ' ' + opts.cls : ''));
    var text = String(s == null ? '' : s);
    if (opts.cap) text = cap(text, opts.cap).text;
    segments(text, opts.multiline).forEach(function (g) {
      if (g.t != null) b.appendChild(document.createTextNode(g.t));
      else b.appendChild(el('span', 'ctl', g.c));
    });
    return b;
  }

  function tooLong(s, limit) { return Array.from(String(s == null ? '' : s)).length > limit; }

  // ---- the session ------------------------------------------------------
  // One unsubscribe at a time, bound to the message identity taken when X
  // was pressed ({account, msgid, article, root}); ctx (app.js):
  //   flash(text, kind)  the status line
  //   archive(ident)     archive ident's thread (the thread pane's e)
  //   onClose()          the dialog has closed

  var S = null;

  function unsubscribe(ident, e, ctx) {
    if (S || !ident || (e && e.repeat)) return;
    var s = S = { ident: ident, ctx: ctx, arm: new Arming(e ? { code: e.code, key: e.key } : null), dlg: null, after: null };
    // Leaving the window before arming (the keyup may never come here):
    // nothing has been confirmed, so the session just goes.
    s.onBlur = function () { if (live(s) && !s.arm.armed()) cancel(); };
    window.addEventListener('blur', s.onBlur);
    preview(null);
  }

  // keyup feeds every keyup (the app document and mail frames) to the
  // current arming.
  function keyup(e) { if (S && S.arm) S.arm.keyup(e); }

  // pendingPreview: X was pressed and the preview hasn't answered yet.
  function pendingPreview() { return !!(S && !S.dlg); }

  // pendingKey takes every keydown while the preview loads, or while link
  // hints are up, before the app's key map (app.js asks first), so nothing
  // falls through to archive or reply: Esc cancels, the rest are dropped
  // (or, for hints, typed). Returns whether it took e.
  function pendingKey(e) {
    if (H) { hintKeydown(e); return true; }
    if (!pendingPreview()) return false;
    e.preventDefault();
    if (e.stopPropagation) e.stopPropagation();
    if (e.key === 'Escape' && !e.repeat) cancel();
    return true;
  }

  // cancel drops the session: a preview in flight is aborted and the dialog
  // closes. An unsubscribe already POSTed runs on; its outcome goes to the
  // status line.
  function cancel() {
    closeHints();
    if (!S) return;
    var s = S;
    if (s.abort) s.abort.abort();
    finish(s);
  }

  // finish ends session s: the dialog closes and goes, synchronously (the
  // close event comes a task later, and a new X may come first).
  function finish(s) {
    if (live(s)) S = null;
    if (s.finished) return;
    s.finished = true;
    window.removeEventListener('blur', s.onBlur);
    if (!s.dlg) return;
    if (s.dlg.open) s.dlg.close();
    s.dlg.remove();
    if (s.ctx.onClose) s.ctx.onClose();
  }

  // cancelFor cancels a session bound to article (its frame was replaced).
  function cancelFor(article) {
    if (H && H.ident.article === article) closeHints();
    if (S && S.ident.article === article) cancel();
  }

  A.unsubscribe = unsubscribe;
  A.keyup = keyup;
  A.cancel = cancel;
  A.cancelFor = cancelFor;
  A.pendingPreview = pendingPreview;
  A.pendingKey = pendingKey;
  A.active = function () { return !!S || !!H; };

  function live(s) { return s === S; }

  function preview(after) {
    var s = S;
    s.after = after;
    if (s.abort) s.abort.abort();
    s.abort = new AbortController();
    fetch(previewURL(s.ident.account, s.ident.msgid, after), {
      credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' }, signal: s.abort.signal,
    }).then(function (res) {
      return res.json().catch(function () { return null; }).then(function (data) {
        if (!res.ok || !data || typeof data.method !== 'string') {
          var err = new Error((data && (data.reason || data.error)) || 'HTTP ' + res.status);
          err.link = linkOutcome(res, data);
          throw err;
        }
        return data;
      });
    }).then(function (data) {
      if (!live(s)) return;
      s.abort = null;
      showPreview(data);
    }).catch(function (err) {
      if (!live(s) || (s.abort && s.abort.signal.aborted)) return;
      s.abort = null;
      var ctx = s.ctx;
      cancel();
      // On a client, a server it can't reach says so (link.js).
      var lt = err && err.link && ctx.linkText ? ctx.linkText(err.link, false) : null;
      ctx.flash(lt ? 'Unsubscribe: ' + lt : 'Unsubscribe failed: ' + (err && err.message || 'network error'), 'error');
    });
  }

  // ---- the dialog -------------------------------------------------------
  // All keys stop here (nothing reaches the thread's map); dialogKey decides
  // the rest, and before arming it drops everything. Once armed, Tab, the
  // disclosure's Enter/Space and the scrolling keys work natively on the
  // disclosure and the scrollable fields (tabindex 0). Buttons stay out of
  // the tab order; one focused by a click acts on Enter/Space only through
  // dialogKey, so only armed, and a click counts only when the pointer went
  // down while armed, so a key-made click never does.

  function ensureDialog() {
    var s = S;
    if (s.dlg) return s.dlg;
    var d = el('dialog');
    d.id = 'unsub';
    d.tabIndex = -1;
    d.setAttribute('aria-label', 'Unsubscribe');
    d.addEventListener('keydown', function (e) {
      e.stopPropagation();
      s.downArmed = false; // a click this key makes is not a pointer's
      var t = e.target && e.target.closest ? e.target : null;
      var btn = t && t.closest('button');
      var target = btn ? 'button' : t && t.closest('summary') ? 'summary' : 'other';
      var what = live(s) ? dialogKey(e, s.arm.armed(), s.keys, target) : 'drop';
      if (what === 'native') return;
      e.preventDefault();
      if (what === 'act') s.keys[e.key](e);
      else if (what === 'press' && typeof btn.pneuAct === 'function') btn.pneuAct(e);
    });
    d.addEventListener('keyup', function (e) {
      e.stopPropagation();
      // Space activates a button on keyup: never here.
      if ((e.key === ' ' || e.key === 'Enter') && e.target && e.target.closest && e.target.closest('button')) e.preventDefault();
      keyup(e);
    });
    // Esc's close request: the keydown handler decides.
    d.addEventListener('cancel', function (e) { e.preventDefault(); });
    d.addEventListener('pointerdown', function () { s.downArmed = live(s) && s.arm.armed(); }, true);
    d.addEventListener('click', function (e) {
      if (e.target === d && s.downArmed) close(); // the backdrop
    });
    d.addEventListener('close', function () { finish(s); });
    s.dlg = d;
    document.body.appendChild(d);
    return d;
  }

  function close() { cancel(); }

  function button(key, label, fn) {
    var b = el('button');
    b.type = 'button';
    b.tabIndex = -1;
    b.appendChild(el('kbd', '', key));
    b.appendChild(document.createTextNode(' ' + label));
    var s = S;
    b.addEventListener('click', function (e) {
      e.stopPropagation();
      var ok = live(s) && s.downArmed && s.arm.armed();
      s.downArmed = false;
      if (ok) fn(null);
    });
    // Enter/Space on the focused button, as dialogKey allows it (armed).
    b.pneuAct = function (e) { if (live(s) && s.arm.armed()) fn(e); };
    return b;
  }

  // rearm starts a new arming for the key event (or null: a click, or an
  // answer arriving) that moved the dialog on. A key the previous arming was
  // still waiting on stays required.
  function rearm(s, trigger) {
    var prev = s.arm;
    var a = new Arming(trigger ? { code: trigger.code, key: trigger.key } : null);
    if (!trigger && prev && !prev.released) { a.code = prev.code; a.key = prev.key; a.released = false; }
    s.arm = a;
    return a;
  }

  // phase replaces the dialog's content. parts: {title, body: [nodes],
  // actions: [[key, label, fn, keys?]]}. trigger is what moved here (see
  // rearm); undefined keeps the current arming (set when the move began).
  // The new content arms once drawn and that key is up.
  function phase(parts, trigger) {
    var s = S;
    var d = ensureDialog();
    if (trigger !== undefined) rearm(s, trigger);
    s.downArmed = false;
    var kids = [el('h2', null, parts.title)];
    (parts.body || []).forEach(function (n) { kids.push(n); });
    var bar = el('div', 'actions');
    var keys = {};
    (parts.actions || []).forEach(function (a) {
      bar.appendChild(button(a[0], a[1], a[2]));
      (a[3] || [a[0]]).forEach(function (k) { keys[k] = a[2]; });
    });
    kids.push(bar);
    s.keys = keys;
    d.replaceChildren.apply(d, kids);
    if (!d.open) d.showModal();
    d.focus({ preventScroll: true });
    var arm = s.arm;
    // Rendered: two frames on, the content has been laid out and painted.
    requestAnimationFrame(function () { requestAnimationFrame(function () { arm.rendered(); }); });
  }

  var CLOSE = ['Esc', 'Close', function () { close(); }, ['Escape', 'n', 'q']];
  var CANCEL = ['n', 'Cancel', function () { close(); }, ['n', 'Escape']];

  function row(dl, label, node) {
    dl.appendChild(el('dt', null, label));
    var dd = el('dd');
    dd.appendChild(node);
    dl.appendChild(dd);
    return dd;
  }

  function note(text, cls) { return el('p', 'note' + (cls ? ' ' + cls : ''), text); }

  // inspectable puts the disclosure, or a field that may scroll, in the tab
  // order, for the armed dialog's native keys.
  function inspectable(node) {
    node.tabIndex = 0;
    return node;
  }

  // destLine is the destination on its own line above everything else, in
  // space reserved for it, so no label above or beside can push it out.
  function destLine(label, value, cls) {
    var p = el('p', 'dest');
    p.appendChild(el('span', 'label', label));
    var f = field(value, { cls: cls });
    if (/\burl\b/.test(cls || '')) inspectable(f);
    p.appendChild(f);
    return p;
  }

  function showPreview(data) {
    var s = S;
    var trig; // the arming set when this preview was asked for (X, or y)
    if (data.method === 'none') {
      phase({
        title: 'Nothing to unsubscribe',
        body: [note('This message offers no unsubscribe in its headers.'), reasonLine(data.reason)],
        actions: [CLOSE],
      }, trig);
      return;
    }
    var body = [];
    var dl = el('dl');
    var confirm = null, blocked = null;
    var title;
    if (data.method === 'one-click') {
      title = 'One-click unsubscribe';
      var dest = destination(data.url);
      if (!dest || tooLong(data.url, WHOLE.url)) blocked = 'The unsubscribe address can’t be shown in full.';
      body.push(destLine('POST to', dest ? cap(dest, CAPS.destination).text : '(unreadable address)', 'destination'));
      row(dl, 'signed by', field('d=' + (data.signedBy || ''), { cap: CAPS.signedBy }));
      var det = el('details');
      det.appendChild(inspectable(el('summary', null, 'Full URL'))); // Tab, then Enter/Space toggles
      det.appendChild(inspectable(field(data.url, { cls: 'url' })));
      body.push(dl, det);
      confirm = ['y', 'Unsubscribe', execute];
    } else if (data.method === 'mailto') {
      title = 'Unsubscribe by email';
      if (tooLong(data.subject, WHOLE.subject) || tooLong(data.body, WHOLE.body)) blocked = 'The message can’t be shown in full.';
      // The recipient in full, wrapped; past the server's own limit it
      // can't be, and nothing is offered.
      if (tooLong(data.to, WHOLE.to)) blocked = 'The recipient can’t be shown in full.';
      body.push(destLine('To', tooLong(data.to, WHOLE.to) ? cap(data.to, WHOLE.to).text : data.to, 'destination'));
      var from = field(data.from, { cap: CAPS.from });
      var acct = el('span');
      acct.appendChild(from);
      acct.appendChild(document.createTextNode(' · account '));
      acct.appendChild(field(data.account, { cap: CAPS.account }));
      row(dl, 'From', acct);
      inspectable(row(dl, 'Subject', field(data.subject)));
      body.push(dl);
      body.push(el('p', 'label', 'The complete message:'));
      var pre = inspectable(el('pre', 'mailbody'));
      if (data.body) pre.appendChild(field(data.body, { multiline: true }));
      else pre.appendChild(el('span', 'empty', '(empty body)'));
      body.push(pre);
      confirm = ['y', 'Send', execute];
    } else if (data.method === 'open') {
      title = 'Open the unsubscribe page';
      var chk = browserCheck(data.url, appOrigins());
      if (!chk.ok) blocked = 'This address was refused for safety: ' + chk.reason + '.';
      else if (tooLong(chk.url, WHOLE.url)) blocked = 'The address can’t be shown in full.';
      body.push(destLine('Opens', chk.ok ? chk.url : data.url, 'destination url'));
      body.push(note('opens in your browser'));
      confirm = ['y', 'Open', openPage];
    } else {
      phase({ title: 'Unsubscribe', body: [note('pneu doesn’t know this unsubscribe method.')], actions: [CLOSE] }, trig);
      return;
    }
    if (data.context) {
      var cdl = el('dl', 'context');
      row(cdl, 'Context', field(data.context, { cap: CAPS.context }));
      body.push(cdl);
    }
    if (!blocked && data.method !== 'open' && !data.token) {
      blocked = data.method === 'mailto'
        ? 'This account can’t send until its first download finishes.'
        : 'No confirmation token came with this preview.';
    }
    s.preview = data;
    if (blocked) {
      body.push(note(blocked, 'bad'));
      phase({ title: title, body: body, actions: [CLOSE] }, trig);
      return;
    }
    phase({ title: title, body: body, actions: [confirm, CANCEL] }, trig);
  }

  function reasonLine(reason) {
    var p = el('p', 'note');
    if (reason) p.appendChild(field(reason, { cap: 300 }));
    return p;
  }

  // ---- execute ----------------------------------------------------------

  function openPage(e) {
    var s = S;
    var c = openInBrowser(s.preview.url);
    if (!c.ok) {
      phase({ title: 'Refused', body: [note('This address was refused for safety: ' + c.reason + '. Nothing was opened.', 'bad')], actions: [CLOSE] }, e);
      return;
    }
    succeeded(e, 'Opened the unsubscribe page in your browser.');
  }

  function execute(e) {
    var s = S;
    var token = s.preview.token;
    // One POST per token: the phase below has no keys until it answers.
    phase({ title: 'Unsubscribing…', body: [note('Waiting for the answer.')], actions: [] }, e);
    var headers = { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' };
    fetch('/unsubscribe', {
      method: 'POST',
      credentials: 'same-origin',
      headers: Pneu.writeHeaders ? Pneu.writeHeaders(headers) : headers, // app.js: the window id
      body: new URLSearchParams({ token: token }).toString(),
    }).then(function (res) {
      return res.json().catch(function () { return null; }).then(function (data) {
        if (res.status === 409 && data && data.state === 'expired') return data;
        // A client daemon that couldn't reach the server says whether the
        // POST left this machine (Pneu-Link, link.js).
        var link = linkOutcome(res, data);
        if (link) return { linkFail: link };
        if (!res.ok || !data || typeof data.state !== 'string') return null; // no answer we can read
        return data;
      });
    }, function () { return null; }).then(function (data) {
      if (data && data.linkFail === 'not-sent') notSent(s);
      else if (data && data.linkFail) unknown(s, token, 0, 0, s.ctx.linkText ? s.ctx.linkText('unknown', true) : null);
      else if (data) outcome(s, data);
      else unknown(s, token, 0);
    });
  }

  // linkOutcome is a failed answer's Pneu-Link from a client daemon
  // ('not-sent' or 'unknown'), or '' (the server answered, or a server).
  function linkOutcome(res, data) {
    var L = root.Pneu && root.Pneu.link;
    return L && !res.ok ? L.outcome(res.headers, data) : '';
  }

  // notSent: the POST never left this machine (the link was down), so
  // nothing ran and the token is unused. Safe to try again, from a fresh
  // preview.
  function notSent(s) {
    var text = (s.ctx.linkText && s.ctx.linkText('not-sent', true)) || 'Not sent: can’t reach the server.';
    if (!live(s)) { s.ctx.flash(text, 'error'); return; }
    phase({
      title: 'Not sent',
      body: [note(text + ' Nothing was sent.', 'bad')],
      actions: [['y', 'Preview again', function (e) { rearm(s, e); preview(s.after); }], CLOSE],
    }, null);
  }

  // Delays before each ask for the stored result: two for a lost answer,
  // then every 3s while the server says the action is still running (a send
  // can outlast the POST's answer), for a minute at most.
  var POLL = [1500, 5000];
  var PENDING = { every: 3000, max: 20 };

  // unknown: the POST's answer was lost. It may have run; it is never sent
  // again. The stored result is asked for instead.
  // why, on the first call, says why the answer was lost (a client's link).
  function unknown(s, token, n, pending, why) {
    pending = pending || 0;
    if (n === 0 && !pending && live(s)) {
      phase({ title: 'Outcome unknown', body: [note(why || 'The answer was lost; checking whether it went through…')], actions: [CLOSE] }, null);
    }
    if (n >= POLL.length || pending > PENDING.max) {
      report(s, 'Unsubscribe outcome unknown. It may have gone through; it will not be retried.', 'error',
        { title: 'Outcome unknown', text: 'It may have gone through. pneu won’t send it again.' });
      return;
    }
    setTimeout(function () {
      fetch('/unsubscribe-result/' + encodeURIComponent(token), {
        credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' },
      }).then(function (res) {
        if (!res.ok) return null;
        return res.json().catch(function () { return null; });
      }, function () { return null; }).then(function (data) {
        var state = data && typeof data.state === 'string' ? data.state : '';
        if (state === 'pending') unknown(s, token, n, pending + 1);
        else if (state && state !== 'expired') outcome(s, data);
        else unknown(s, token, n + 1, pending);
      });
    }, pending ? PENDING.every : POLL[n]);
  }

  // report shows an end state in the dialog if it's still up, else on the
  // status line.
  function report(s, flashText, kind, dialog) {
    if (live(s)) phase({ title: dialog.title, body: [note(dialog.text, kind === 'error' ? 'bad' : '')], actions: [CLOSE] }, null);
    else s.ctx.flash(flashText, kind);
  }

  function outcome(s, data) {
    var method = s.preview.method;
    switch (data.state) {
      case 'ok':
        var what = method === 'mailto' ? 'Unsubscribe email sent.' : 'Unsubscribed.';
        if (live(s)) succeeded(null, what);
        else s.ctx.flash(what);
        return;
      case 'maybe-sent':
        // The send log's unknown: never sent again, from any preview.
        report(s, 'Unsubscribe email may have been sent: check Sent.', 'error',
          { title: 'May have been sent', text: 'The unsubscribe email may have been sent: check Sent in Gmail. pneu won’t send it again.' });
        return;
      case 'refused':
        report(s, 'Unsubscribe refused for safety; nothing was sent.', 'error',
          { title: 'Refused', text: 'The destination was refused for safety. Nothing was sent.' });
        return;
      case 'expired':
        if (!live(s)) { s.ctx.flash('Unsubscribe expired; nothing was sent.', 'error'); return; }
        phase({
          title: 'Expired',
          body: [note('The confirmation expired before it ran. Nothing was sent.')],
          actions: [['y', 'Preview again', function (e) { rearm(s, e); preview(s.after); }], CLOSE],
        }, null);
        return;
      case 'failed':
        var cat = data.category ? ' (' + data.category + ')' : '';
        if (!live(s)) { s.ctx.flash('Unsubscribe failed' + cat, 'error'); return; }
        var acts = [CLOSE];
        var body = [note('The unsubscribe request failed' + cat + '.', 'bad')];
        if (data.fallback && typeof s.preview.index === 'number') {
          var idx = s.preview.index;
          body.push(note('The sender lists another way to unsubscribe.'));
          acts = [['y', 'Preview the next option', function (e) { rearm(s, e); preview(idx); }], CLOSE];
        }
        phase({ title: 'Failed', body: body, actions: acts }, null);
        return;
      default:
        report(s, 'Unsubscribe outcome unknown.', 'error', { title: 'Outcome unknown', text: 'The server gave an answer pneu doesn’t know.' });
    }
  }

  function succeeded(e, text) {
    var s = S;
    phase({
      title: 'Done',
      body: [note(text)],
      actions: [['e', 'Archive the thread', function () {
        var ident = s.ident, ctx = s.ctx;
        close();
        ctx.archive(ident);
      }], CLOSE],
    }, e);
  }

  // ---- the chip and o ---------------------------------------------------
  // The chip is the preview: `o  <name> → <destination>`, each sender
  // string its own isolated <bdi> with controls drawn. The destination is
  // derived from the URL that opens and never gives way to the name (CSS:
  // the name shrinks, the destination wraps). There is no confirmation
  // after it; o and a click open, through browserCheck again.

  // chip is the header chip for action (declaredAction's), or null if its
  // URL can't be shown. open runs on a click.
  function chip(action, open) {
    var p = chipParts(action);
    if (!p) return null;
    var b = el('button', 'cta');
    b.type = 'button';
    b.tabIndex = -1;
    b.appendChild(el('kbd', null, 'o'));
    if (action.guess) {
      // A guess from the body's links (tier 2), marked as one.
      b.className += ' guess';
      b.appendChild(el('span', 'tag', 'guess'));
    }
    b.appendChild(field(p.name, { cls: 'name' }));
    b.appendChild(el('span', 'arrow', '→'));
    var d = el('span', 'dest');
    d.appendChild(field(p.dest, { cls: 'destination' }));
    if (p.redirect) d.appendChild(el('span', 'redirect', '(redirect)'));
    b.appendChild(d);
    b.pneuAction = { url: action.url, name: p.name };
    if (action.guess) b.pneuAction.guess = action.guess;
    b.addEventListener('click', function (e) {
      e.stopPropagation(); // not a header click: no fold
      b.blur(); // a later Enter is the app's again
      open();
    });
    return b;
  }

  // openAction opens the action a chip carries, re-checked now; returns
  // the check (ok, url | reason).
  // A guess is re-validated first (stillPicked): the same link, connected,
  // visible and pointing where it did when picked, else nothing opens and
  // the check says why (stale).
  function openAction(chipEl) {
    var a = chipEl && chipEl.pneuAction;
    if (!a) return { ok: false, reason: 'no link' };
    if (a.guess) {
      var why = stillPicked(a.guess);
      if (why) return { ok: false, stale: true, reason: why };
    }
    return openInBrowser(a.url);
  }

  // primary handles an o on chipEl (primaryKey): with the chip wholly
  // inside bounds and nothing over it, it opens; otherwise the chip is
  // scrolled into view and marked for a moment, and nothing opens until a
  // later, separate o finds it in view. Returns {act: 'none' | 'reveal' |
  // 'open', why: 'bounds' | 'covered' (for a reveal), check: openAction's
  // (when it opened)}.
  function primary(chipEl, e, bounds) {
    var r = chipEl.getBoundingClientRect();
    var act = primaryKey(e, r, bounds), why = act === 'reveal' ? 'bounds' : null;
    if (act === 'open' && covered(chipEl, r)) { act = 'reveal'; why = 'covered'; }
    if (act === 'none') return { act: act };
    if (act === 'reveal') {
      chipEl.scrollIntoView({ block: 'center', inline: 'nearest' });
      chipEl.classList.add('reveal');
      clearTimeout(chipEl.pneuReveal);
      chipEl.pneuReveal = setTimeout(function () { chipEl.classList.remove('reveal'); }, 1500);
      return { act: act, why: why };
    }
    return { act: act, check: openAction(chipEl) };
  }

  // covered reports whether anything but the chip shows at its centre, its
  // top and bottom rows, or any line of its name and destination (a
  // wrapped destination is several): whatever is in front hides it.
  function covered(chipEl, r) {
    var cx = (r.left + r.right) / 2;
    var pts = [[cx, (r.top + r.bottom) / 2], [cx, r.top + 2], [cx, r.bottom - 2]];
    Array.prototype.forEach.call(chipEl.querySelectorAll('bdi'), function (b) {
      Array.prototype.forEach.call(b.getClientRects(), function (l) {
        if (!(l.width > 0 && l.height > 0)) return;
        var y = (l.top + l.bottom) / 2;
        pts.push([l.left + 1, y], [(l.left + l.right) / 2, y], [l.right - 1, y]);
      });
    });
    return pts.some(function (p) {
      var hit = document.elementFromPoint(p[0], p[1]);
      return !hit || !chipEl.contains(hit);
    });
  }

  A.chip = chip;
  A.openAction = openAction;
  A.primary = primary;

  // ---- the guess (o, tier 2) -------------------------------------------------
  // Read off the frame's live document after it has laid out; the frame runs
  // no script, so nothing in it changes but by our hand, a re-render, or
  // the sender's CSS animating it (which refuses a link: moving).
  // Everything is read through the prototypes (docRoot's reason), in the
  // frame's own coordinates: its viewport is the whole document (mailframe.js
  // sizes it to its content), so a link below the fold is still hit-tested.
  //
  // None of this is a security boundary: a sender can already name any
  // destination for o, invisibly, with JSON-LD. The boundary is the chip,
  // which shows the real destination (the href that opens), and o opening
  // only what the chip shows. These checks make a guess plausible (a link
  // the reader can see, saying what it says), and they refuse
  // conservatively rather than model CSS paint exactly.

  var P = null;
  function dom() {
    if (P) return P;
    var get = function (proto, n) { return Object.getOwnPropertyDescriptor(proto, n).get; };
    P = {
      rects: Element.prototype.getClientRects, bcr: Element.prototype.getBoundingClientRect,
      attr: Element.prototype.getAttribute, qsa: Element.prototype.querySelectorAll,
      animations: Element.prototype.getAnimations,
      contains: Node.prototype.contains, fromPoint: Document.prototype.elementFromPoint,
      range: Document.prototype.createRange, walker: Document.prototype.createTreeWalker,
      parent: get(Node.prototype, 'parentElement'), connected: get(Node.prototype, 'isConnected'),
      owner: get(Node.prototype, 'ownerDocument'), text: get(Node.prototype, 'textContent'),
      type: get(Node.prototype, 'nodeType'),
      ns: get(Element.prototype, 'namespaceURI'), local: get(Element.prototype, 'localName'),
      clientW: get(Element.prototype, 'clientWidth'), clientH: get(Element.prototype, 'clientHeight'),
      scrollH: get(Element.prototype, 'scrollHeight'),
      sheets: get(Document.prototype, 'styleSheets'),
    };
    return P;
  }

  // The work one guess may do (CTA.budget): past any of it, spend throws
  // OVER and the message gets no pick. A pass (frameEnv) counts its nodes
  // visited, text ranges measured and computed styles looked up; each
  // element's style is looked up once per pass.
  var OVER = { over: true };
  function spend(env, kind, n) {
    env.spent[kind] += n == null ? 1 : n;
    if (env.spent[kind] > CTA.budget[kind]) throw OVER;
  }
  function style(env, el, pseudo) {
    var m = env.cs[pseudo || ''] || (env.cs[pseudo || ''] = new Map());
    var cs = m.get(el);
    if (!cs) {
      spend(env, 'styles');
      cs = getComputedStyle(el, pseudo || null);
      m.set(el, cs);
    }
    return cs;
  }

  function areaOf(r) { return r ? (r.right - r.left) * (r.bottom - r.top) : 0; }

  // largestRect is el's client rect with the most area, or null.
  function largestRect(el) {
    var rs = dom().rects.call(el), best = null, ba = 0;
    for (var i = 0; i < rs.length; i++) {
      var r = rs[i], ar = r.width * r.height;
      if (ar > ba) { ba = ar; best = r; }
    }
    return best && { left: best.left, top: best.top, right: best.right, bottom: best.bottom };
  }

  // rgba is a CSS color as [r, g, b, a (0-1)], or null. Computed colors are
  // rgb()/rgba() but for the newer spaces (oklab, color(), ...), which a
  // 1×1 canvas converts.
  var colorCtx = null, colors = Object.create(null);
  function rgba(css) {
    css = String(css || '').trim();
    if (own.call(colors, css)) return colors[css];
    var out = null;
    var m = /^rgba?\(\s*([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)(?:\s*[,/]\s*([\d.]+)(%?))?\s*\)$/.exec(css);
    if (m) {
      out = [+m[1], +m[2], +m[3], m[4] == null ? 1 : m[5] ? m[4] / 100 : +m[4]];
    } else if (css) {
      try {
        if (!colorCtx) {
          var cv = document.createElement('canvas');
          cv.width = cv.height = 1;
          colorCtx = cv.getContext('2d', { willReadFrequently: true });
        }
        colorCtx.clearRect(0, 0, 1, 1);
        colorCtx.fillStyle = 'rgba(1, 2, 3, 0)';
        colorCtx.fillStyle = css;
        colorCtx.fillRect(0, 0, 1, 1);
        var d = colorCtx.getImageData(0, 0, 1, 1).data;
        out = [d[0], d[1], d[2], d[3] / 255];
      } catch (e) { out = null; }
    }
    colors[css] = out;
    return out;
  }

  function painted(c) { return !!(c && c[3] > 0); }

  // backdrop is the opaque color el's content sits on: its own background
  // and its ancestors', composited from the first opaque one up (white
  // under a transparent root). Background images never get here: a link
  // with one on it, inside it or behind it is refused (seen).
  function backdrop(el, env) {
    var chain = [];
    for (var n = el; n; n = n === env.root ? null : dom().parent.call(n)) {
      spend(env, 'nodes');
      var c = rgba(style(env, n).backgroundColor);
      if (painted(c)) chain.push(c);
      if (c && c[3] >= 0.999) break;
    }
    var b = [255, 255, 255];
    for (var i = chain.length - 1; i >= 0; i--) b = over(chain[i], b);
    return b;
  }

  // surface is el's own background, when it has one that shows (a color at
  // least half opaque), as it paints over its container: [r, g, b].
  function surface(el, env) {
    var c = rgba(style(env, el).backgroundColor);
    if (!(c && c[3] >= 0.5)) return null;
    var p = dom().parent.call(el);
    return over(c, p && el !== env.root ? backdrop(p, env) : [255, 255, 255]);
  }

  function distinct(a, b) {
    return Math.abs(a[0] - b[0]) + Math.abs(a[1] - b[1]) + Math.abs(a[2] - b[2]) > 24;
  }

  // legible: text drawn in el's style is not near-invisible against its
  // backdrop (same-color and transparent text).
  function legible(el, cs, env) {
    var fg = rgba(cs.getPropertyValue('-webkit-text-fill-color') || cs.color);
    if (!fg) return false;
    var b = backdrop(el, env);
    return contrast(over(fg, b), b) >= CTA.minContrast;
  }

  // clipped reports whether cs hides its box by other means than size and
  // opacity: a clip, a mask, a filter or a blend mode, any of which can
  // make a link vanish while every rect says it's there. Conservative.
  function clipped(cs) {
    var mask = cs.getPropertyValue('mask-image') || cs.getPropertyValue('-webkit-mask-image') || 'none';
    return cs.clipPath !== 'none' || (cs.clip && cs.clip !== 'auto') || cs.filter !== 'none' ||
      mask !== 'none' || (cs.mixBlendMode && cs.mixBlendMode !== 'normal');
  }

  // moving: a CSS animation or transition runs on el (or, subtree, on
  // anything inside it): what shows now may not in a moment.
  function moving(el, subtree) {
    var f = dom().animations;
    return !!f && f.call(el, subtree ? { subtree: true } : undefined).length > 0;
  }

  // pseudos reports whether the frame's style sheets name a pseudo-element
  // (::before, ::after, ::first-line, ::first-letter) anywhere, read off the
  // CSSOM so escapes are already undone. Without one only a <q> has one
  // (the UA's quotes), and the per-element pseudo lookups are skipped.
  var PSEUDO_SEL = /:(?:before|after|first-line|first-letter)\b/i;
  function pseudos(env) {
    if (env.pseudos != null) return env.pseudos;
    var found = false;
    var walk = function (rules) {
      for (var i = 0; rules && i < rules.length && !found; i++) {
        spend(env, 'nodes');
        var r = rules[i];
        if (typeof r.selectorText === 'string' && PSEUDO_SEL.test(r.selectorText)) found = true;
        else if (r.cssRules) walk(r.cssRules);
      }
    };
    var sheets = dom().sheets.call(env.doc);
    for (var i = 0; i < sheets.length && !found; i++) {
      var rules = null;
      try { rules = sheets[i].cssRules; } catch (e) { found = true; }
      walk(rules);
    }
    env.pseudos = found;
    return found;
  }
  function pseudoCheck(env, el) { return pseudos(env) || dom().local.call(el) === 'q'; }

  // pseudoTrick: el draws something that isn't in the DOM text, or draws
  // its text other than its computed style says. A ::before/::after with
  // content (other than none, normal or ""), or an empty one that paints a
  // background; a ::first-line or ::first-letter whose color, fill, size
  // or background differs from el's own. Conservative: it refuses elements
  // where the rule doesn't apply too.
  function pseudoTrick(env, el) {
    if (!pseudoCheck(env, el)) return false;
    var cs = style(env, el);
    for (var i = 0; i < 2; i++) {
      var ps = style(env, el, i ? '::after' : '::before'), ct = ps.content;
      if (ct === 'none' || ct === 'normal') continue;
      if (ct !== '""' || painted(rgba(ps.backgroundColor)) || ps.backgroundImage !== 'none') return true;
    }
    for (var j = 0; j < 2; j++) {
      var fs = style(env, el, j ? '::first-letter' : '::first-line');
      if (fs.color !== cs.color || fs.fontSize !== cs.fontSize ||
        fs.getPropertyValue('-webkit-text-fill-color') !== cs.getPropertyValue('-webkit-text-fill-color') ||
        painted(rgba(fs.backgroundColor)) || fs.backgroundImage !== 'none') return true;
    }
    return false;
  }

  // overlays are what may paint over a link without taking the hit test,
  // found by one bounded scan of the document rather than by modelling
  // paint order: every element with pointer-events: none and a position
  // other than static, or positioned fixed, absolute or sticky with a
  // background or any opacity, and everything inside one (it paints in the
  // same layer); and every positioned ::before/::after that paints, which
  // has no rect of its own to read and so counts as covering the whole
  // document. [{el, r, pseudo}], once per pass.
  function suspect(cs) {
    var pos = cs.position;
    if (cs.pointerEvents === 'none' && pos !== 'static') return true;
    if (pos !== 'fixed' && pos !== 'absolute' && pos !== 'sticky') return false;
    return painted(rgba(cs.backgroundColor)) || cs.backgroundImage !== 'none' || parseFloat(cs.opacity) > 0;
  }
  function overlays(env) {
    if (env.overlays) return env.overlays;
    var D = dom(), all = D.qsa.call(env.root, '*'), out = [], open = [];
    spend(env, 'nodes', all.length);
    var whole = { left: -Infinity, top: -Infinity, right: Infinity, bottom: Infinity };
    for (var i = 0; i < all.length; i++) {
      var e = all[i];
      while (open.length && !D.contains.call(open[open.length - 1], e)) open.pop();
      var inside = open.length > 0;
      if (inside || suspect(style(env, e))) {
        if (!inside) open.push(e);
        out.push({ el: e, r: D.bcr.call(e), pseudo: false });
      }
      if (pseudoCheck(env, e)) {
        for (var k = 0; k < 2; k++) {
          var ps = style(env, e, k ? '::after' : '::before');
          if (ps.content === 'none' || ps.content === 'normal') continue;
          if (ps.position !== 'static' && (inside || suspect(ps))) out.push({ el: e, r: whole, pseudo: true });
        }
      }
    }
    env.overlays = out;
    return out;
  }

  // overlaid: something from overlays that isn't a, inside a or around it
  // (a pseudo-element counts as its element's only when a holds it) meets
  // a's box.
  function overlaid(a, env) {
    var D = dom(), box = D.bcr.call(a), list = overlays(env);
    spend(env, 'nodes', list.length);
    for (var i = 0; i < list.length; i++) {
      var o = list[i];
      if (D.contains.call(a, o.el)) continue;
      if (!o.pseudo && D.contains.call(o.el, a)) continue;
      if (intersect(o.r, box)) return true;
    }
    return false;
  }

  // Elements that paint an opaque box of their own content.
  var REPLACED = /^(?:img|picture|video|canvas|object|embed|iframe|input|select|textarea|meter|progress)$/;
  var HTML_NS = 'http://www.w3.org/1999/xhtml';

  // seen is what of anchor a shows, or null if it doesn't show (or throws
  // OVER past the budget). The box: its largest client rect, half of it
  // at least inside the frame's viewport and inside every ancestor that
  // clips; no ancestor under 0.5 opacity (the product), hidden, clipped,
  // masked, filtered or blended, painting a background image, or animated;
  // the element at the centre of what shows is a or inside it; nothing
  // from overlays meets it. Inside it: HTML only (an <svg> refuses it), no
  // background image, no pseudo-element trick (pseudoTrick), nothing
  // animated; every text node that draws (a box with area, visibility
  // visible) at least CTA.minFont, legible, under 0.5 opacity nowhere, and
  // drawn inside the visible box where it hits the link; and no opaque box
  // inside it (a background, an image) over a quarter or more of its text.
  // One piece that fails refuses the link (hidden-text tricks).
  // {box, text, imageOnly}: text is the drawn text; imageOnly when there is
  // none (an image's alt is never a label).
  function seen(a, env) {
    var D = dom();
    var full = largestRect(a);
    if (!full) return null;
    var half = areaOf(full) / 2;
    var vis = intersect(full, { left: 0, top: 0, right: env.vw, bottom: env.vh });
    if (!vis || areaOf(vis) < half) return null;
    var o = 1;
    for (var n = a; n; n = n === env.root ? null : D.parent.call(n)) {
      spend(env, 'nodes');
      var cs = style(env, n);
      var v = parseFloat(cs.opacity);
      if (v >= 0 && v <= 1) o *= v;
      if (o < 0.5 || clipped(cs) || cs.backgroundImage !== 'none') return null;
      if (moving(n, n === a)) return null;
      if (n === a && cs.visibility !== 'visible') return null;
      if (n !== a && (cs.overflowX !== 'visible' || cs.overflowY !== 'visible')) {
        vis = intersect(vis, D.bcr.call(n));
        if (!vis || areaOf(vis) < half) return null;
      }
    }
    if (vis.right - vis.left < 8 || vis.bottom - vis.top < 8) return null;
    var hit = D.fromPoint.call(env.doc, (vis.left + vis.right) / 2, (vis.top + vis.bottom) / 2);
    if (!hit || !D.contains.call(a, hit)) return null;
    if (overlaid(a, env)) return null;
    if (pseudoTrick(env, a)) return null;

    var parts = [], opaque = [];
    var w = D.walker.call(env.doc, a, 5 /* NodeFilter.SHOW_ELEMENT | SHOW_TEXT */);
    for (var t = w.nextNode(); t; t = w.nextNode()) {
      spend(env, 'nodes');
      if (D.type.call(t) === 1) {
        if (D.ns.call(t) !== HTML_NS) return null; // <svg>, <math>
        var ecs = style(env, t);
        if (ecs.backgroundImage !== 'none' || pseudoTrick(env, t)) return null;
        var bc = rgba(ecs.backgroundColor);
        if ((bc && bc[3] >= 0.5) || REPLACED.test(D.local.call(t))) opaque.push(t);
        continue;
      }
      var data = D.text.call(t);
      if (!/\S/.test(data)) continue;
      var pe = D.parent.call(t);
      var pcs = style(env, pe);
      if (pcs.visibility !== 'visible') continue; // not drawn
      spend(env, 'ranges');
      var range = D.range.call(env.doc);
      range.selectNodeContents(t);
      var r = range.getBoundingClientRect();
      if (!(r.width > 0 && r.height > 0)) continue; // not drawn
      if (parseFloat(pcs.fontSize) < CTA.minFont) return null;
      var po = o;
      for (var m = pe; m && m !== a; m = D.parent.call(m)) {
        spend(env, 'nodes');
        var mcs = style(env, m), mv = parseFloat(mcs.opacity);
        if (mv >= 0 && mv <= 1) po *= mv;
        if (clipped(mcs)) return null;
      }
      if (po < 0.5 || !legible(pe, pcs, env)) return null;
      var tr = { left: r.left, top: r.top, right: r.right, bottom: r.bottom };
      var tv = intersect(tr, vis);
      if (!tv || areaOf(tv) < areaOf(tr) / 2) return null;
      var th = D.fromPoint.call(env.doc, (tv.left + tv.right) / 2, (tv.top + tv.bottom) / 2);
      if (!th || !D.contains.call(a, th)) return null;
      parts.push({ s: data, r: tr, node: t });
    }
    // An opaque box inside the link that doesn't hold a piece of text may
    // paint over it (an inline-block, an image, a positioned child): a
    // quarter of the text's area under such boxes refuses the link.
    var total = 0, covered = 0;
    parts.forEach(function (p) { total += areaOf(p.r); });
    opaque.forEach(function (el) {
      var er = D.bcr.call(el);
      spend(env, 'nodes', parts.length);
      parts.forEach(function (p) {
        if (!D.contains.call(el, p.node)) covered += areaOf(intersect(er, p.r));
      });
    });
    if (total > 0 && covered >= total / 4) return null;
    // Pieces on one line, touching, run together ("Ver<b>ify</b>"); any
    // other break is a space.
    var text = '';
    parts.forEach(function (p, i) {
      var q = i ? parts[i - 1].r : null;
      if (q && !(Math.abs(p.r.top - q.top) < Math.max(p.r.bottom - p.r.top, q.bottom - q.top) / 2 && p.r.left - q.right < 4)) text += ' ';
      text += p.s;
    });
    text = text.replace(/\s+/g, ' ').trim();
    return { box: vis, text: text, imageOnly: !text };
  }

  // styling is how button-like a looks: a background of its own that
  // differs from its container's, or, for the table-cell buttons email
  // uses, an ancestor (up to 4) whose only content is this link with such a
  // background; padding, rounded corners and a border on either. area is
  // the visual button's (that cell's, if any) visible area.
  function styling(a, env, box) {
    var D = dom();
    var srcs = [a], styled = false, cellBox = null;
    var mine = surface(a, env), p = D.parent.call(a);
    if (mine && p && distinct(mine, backdrop(p, env))) styled = true;
    var norm = function (s) { return String(s || '').replace(/\s+/g, ' ').trim(); };
    var text = norm(D.text.call(a));
    for (var n = p, i = 0; n && n !== env.root && i < 4; n = D.parent.call(n), i++) {
      var nt = D.text.call(n);
      spend(env, 'nodes', 1 + Math.ceil(nt.length / 256));
      if (norm(nt) !== text) break;
      var links = D.qsa.call(n, 'a[href]');
      spend(env, 'nodes', links.length);
      if (links.length !== 1) break;
      srcs.push(n);
      var s = surface(n, env), np = D.parent.call(n);
      if (s && np && distinct(s, backdrop(np, env))) {
        styled = true;
        cellBox = intersect(D.bcr.call(n), { left: 0, top: 0, right: env.vw, bottom: env.vh });
        break;
      }
    }
    var padding = false, radius = false, border = false;
    srcs.forEach(function (e) {
      var cs = style(env, e), px = function (k) { return parseFloat(cs[k]) || 0; };
      if (px('paddingTop') + px('paddingBottom') >= 4 && px('paddingLeft') + px('paddingRight') >= 8) padding = true;
      if (px('borderTopLeftRadius') >= 2 || px('borderBottomRightRadius') >= 2) radius = true;
      if (['Top', 'Right', 'Bottom', 'Left'].every(function (side) {
        var c = rgba(cs['border' + side + 'Color']);
        return px('border' + side + 'Width') >= 1 && !/^(none|hidden)$/.test(cs['border' + side + 'Style']) && c && c[3] > 0;
      })) border = true;
    });
    return { styled: styled, padding: padding, radius: radius, border: border, area: areaOf(cellBox || box) };
  }

  // candidate is anchor a as a scored candidate's features, or null: an
  // absolute http(s) href that passes the browser-open check (never a
  // mailto), a link that shows (seen). Throws OVER past the budget.
  function candidate(a, env) {
    var raw = dom().attr.call(a, 'href');
    if (raw == null || !/^\s*https?:\/\//i.test(raw)) return null;
    var c = browserCheck(raw.trim(), env.origins);
    if (!c.ok || Array.from(c.url).length > WHOLE.url) return null;
    var s = seen(a, env);
    if (!s) return null;
    var st = styling(a, env, s.box);
    var info = ctaText(s.text);
    var cy = (s.box.top + s.box.bottom) / 2;
    var at = env.docH > 0 ? Math.min(Math.max(cy / env.docH, 0), 1) : 0;
    return {
      url: c.url, href: raw, text: s.text, label: cap(s.text, NAME_CAP).text, anchor: a, box: s.box,
      f: {
        styled: st.styled, padding: st.padding, radius: st.radius, border: st.border,
        verb: info.verb, short: info.words >= 1 && info.words <= 5, long: info.words > 8,
        early: 1 - at, area: st.area, footer: info.footer, imageOnly: s.imageOnly, tail: at > 0.75,
        rawURL: info.rawURL,
      },
    };
  }

  // frameEnv is one pass over doc: its geometry, and the pass's budget
  // counters and style cache.
  function frameEnv(doc, origins) {
    var root = docRoot(doc);
    if (!root) return null;
    var D = dom();
    return {
      doc: doc, root: root, vw: D.clientW.call(root), vh: D.clientH.call(root), docH: D.scrollH.call(root),
      origins: origins || appOrigins(),
      spent: { nodes: 0, ranges: 0, styles: 0 }, cs: {}, overlays: null, pseudos: null,
    };
  }

  // guess runs the heuristic on frame's document, which must have laid out
  // (layout: false otherwise; try again once it shows). {pick, runnerUp,
  // ranked (ctaRank's, with each candidate's anchor, features and score),
  // candidates (how many links showed), layout, over (the budget ran out:
  // no pick), error (something threw: no pick), action (the pick as the
  // chip carries it)}. It never throws. Only for a
  // message with no declared action; the caller decides that.
  function guess(frame, origins) {
    var r = { pick: null, runnerUp: null, ranked: [], candidates: 0, layout: false, over: false, error: false, action: null };
    var doc, env;
    try {
      doc = frame && frame.contentDocument;
      env = doc && frameEnv(doc, origins);
    } catch (e) { env = null; }
    if (!env || !(env.vw > 0 && env.vh > 0)) return r;
    var cands = [];
    try {
      var anchors = dom().qsa.call(env.root, 'a[href]');
      for (var i = 0; i < anchors.length && i < CTA.max; i++) {
        var c = candidate(anchors[i], env);
        if (c) cands.push(c);
      }
      var got = ctaRank(cands);
      got.candidates = cands.length;
      got.layout = true;
      got.over = false;
      got.error = false;
      got.action = null;
      if (got.pick) {
        var p = got.pick;
        got.action = {
          url: p.url, name: p.label || 'Link',
          guess: { frame: frame, doc: doc, anchor: p.anchor, href: p.href, url: p.url, label: p.label },
        };
      }
      return got;
    } catch (e) {
      // Past the budget, or anything unexpected (the sender's document is
      // odd in a way nothing here foresaw): no pick, nothing half-built.
      r.layout = true;
      if (e === OVER) r.over = true; else r.error = true;
      return r;
    }
  }

  // textLines reads a text body's <pre> (textbody.go RenderText) as lines
  // for textPick: its text, top-level links ({url: the href, start, end,
  // anchor}), and a quoted entry for each quote (blockquote.q, or the
  // folded trailing quote, details.qfold) in its place.
  function textLines(pre) {
    var lines = [], cur = { text: '', links: [] };
    var flush = function () { lines.push(cur); cur = { text: '', links: [] }; };
    Array.prototype.forEach.call(pre.childNodes, function (n) {
      if (n.nodeType === 3) {
        var parts = n.data.split('\n');
        parts.forEach(function (t, i) { if (i) flush(); cur.text += t; });
      } else if (n.nodeType === 1 && n.localName === 'a' && n.hasAttribute('href')) {
        var start = cur.text.length;
        cur.text += n.textContent;
        cur.links.push({ url: n.getAttribute('href'), start: start, end: cur.text.length, anchor: n });
      } else if (n.nodeType === 1 && (n.localName === 'blockquote' || n.localName === 'details')) {
        // A folded trailing quote (details.qfold) is quoted as a whole.
        if (cur.text || cur.links.length) flush();
        lines.push({ text: '', links: [], quoted: true });
      } else if (n.nodeType === 1) {
        cur.text += n.textContent;
      }
    });
    if (cur.text || cur.links.length) flush();
    return lines;
  }

  // textGuess is a text body's guess (textPick over its <pre>), as the
  // chip's action {url, name, guess}, or null. The URL is the anchor's href
  // (RenderText writes the address it shows); the guess keeps the anchor
  // for stillPicked.
  function textGuess(pre) {
    var p = pre ? textPick(textLines(pre)) : null;
    if (!p) return null;
    var a = p.link.anchor;
    return { url: p.url, name: p.name, guess: { text: true, anchor: a, href: a.getAttribute('href') } };
  }

  // stillPicked is null while the guess g still stands, else why not: the
  // frame holds the same document, the anchor is in it, its href is the
  // one picked, and it still shows (seen) with the same text.
  function stillPicked(g) {
    try { return standing(g); } catch (e) { return 'the link no longer shows'; }
  }
  function standing(g) {
    var D = dom();
    if (g.text) {
      // A text body's pick (textGuess): server-rendered markup in this
      // document, which only a re-render replaces.
      if (!D.connected.call(g.anchor)) return 'the link is gone';
      if (D.attr.call(g.anchor, 'href') !== g.href) return 'the link changed after it was picked';
      return null;
    }
    if (!g.frame || g.frame.contentDocument !== g.doc) return 'the message was redrawn';
    if (!D.connected.call(g.anchor) || D.owner.call(g.anchor) !== g.doc) return 'the link is gone';
    if (D.attr.call(g.anchor, 'href') !== g.href) return 'the link changed after it was picked';
    var env = frameEnv(g.doc);
    var c = env && candidate(g.anchor, env);
    if (!c) return 'the link no longer shows';
    if (c.url !== g.url || c.label !== g.label) return 'the link changed after it was picked';
    return null;
  }

  // mark highlights a guess's link from this document, never by styling
  // inside the frame (the sender's CSS comes after ours there): a layer the
  // size of the frame's viewport in holder (the frame's positioned
  // container), so the frame's box clips it, it scrolls with the pane, and
  // the app's sticky chrome, the key bar and dialogs stack above it. It
  // follows the link as the frame resizes or scrolls sideways, re-checking
  // the guess g (stillPicked) each time, and hides when the link has no
  // box or the guess no longer stands. While it shows it is re-validated
  // every 250ms (a timer, which runs only while the layer is drawn and
  // displayed): if the link moved with no resize or scroll to follow, or
  // the guess no longer stands, it hides until the next of those. The
  // caller calls sync() when the layer may have started or stopped
  // showing (the cursor moved, the message expanded or collapsed): it
  // stops the timer, or re-validates and starts it. Anything that throws
  // hides it and schedules nothing. {el, update(), sync(), stop(),
  // timing() (whether the timer is pending)}.
  function mark(holder, frame, anchor, g) {
    var layer = el('div', 'cta-mark');
    layer.setAttribute('aria-hidden', 'true');
    var box = el('span');
    layer.appendChild(box);
    holder.appendChild(layer);
    var doc = frame.contentDocument, stopped = false, drawn = null, timer = 0;
    function stop() {
      if (stopped) return;
      stopped = true;
      ro.disconnect();
      rest();
      window.removeEventListener('resize', update);
      if (doc) doc.removeEventListener('scroll', update, true);
      layer.remove();
    }
    function rest() { if (timer) { clearTimeout(timer); timer = 0; } }
    function where() {
      return frame.contentDocument === doc && dom().connected.call(anchor) ? largestRect(anchor) : null;
    }
    function stands() { return !g || stillPicked(g) === null; }
    // showing: drawn and displayed (the cursor message, expanded).
    function showing() { return !layer.hidden && layer.getClientRects().length > 0; }
    // arm keeps the re-validation timer running exactly while it shows.
    function arm() {
      if (!stopped && showing()) { if (!timer) timer = setTimeout(tick, 250); } else rest();
    }
    function update() {
      if (stopped) return;
      if (!layer.isConnected) { stop(); return; } // the thread went
      try {
        var r = where();
        if (r && !stands()) r = null;
        drawn = r;
        layer.hidden = !r;
        if (r) {
          layer.style.left = (frame.offsetLeft + frame.clientLeft) + 'px';
          layer.style.top = (frame.offsetTop + frame.clientTop) + 'px';
          layer.style.width = frame.clientWidth + 'px';
          layer.style.height = frame.clientHeight + 'px';
          box.style.left = r.left + 'px';
          box.style.top = r.top + 'px';
          box.style.width = (r.right - r.left) + 'px';
          box.style.height = (r.bottom - r.top) + 'px';
        }
      } catch (e) {
        drawn = null; // anything unexpected: no highlight
        layer.hidden = true;
      }
      arm();
    }
    // tick re-validates what shows; hidden or not displayed, the timer
    // stops until update or sync starts it again. A throw hides it, and
    // nothing is rescheduled.
    function tick() {
      timer = 0;
      if (stopped) return;
      if (!layer.isConnected) { stop(); return; }
      if (!showing()) return;
      try {
        var r = where();
        var moved = !r || !drawn || ['left', 'top', 'right', 'bottom'].some(function (k) { return Math.abs(r[k] - drawn[k]) > 0.5; });
        if (moved || !stands()) layer.hidden = true;
      } catch (e) {
        layer.hidden = true;
      }
      arm();
    }
    // sync: the layer may have started or stopped showing (the cursor
    // moved, the message expanded or collapsed): stop the timer, or
    // re-validate and start it.
    function sync() {
      if (stopped) return;
      if (!layer.isConnected) { stop(); return; }
      if (holder.getClientRects().length) update();
      else rest();
    }
    var ro = new ResizeObserver(update);
    update();
    ro.observe(frame);
    window.addEventListener('resize', update);
    if (doc) doc.addEventListener('scroll', update, true);
    return { el: layer, update: update, sync: sync, stop: stop, timing: function () { return !!timer; } };
  }

  A.guess = guess;
  A.textGuess = textGuess;
  A.stillPicked = stillPicked;
  A.mark = mark;

  // ---- link hints -------------------------------------------------------
  // Labels are drawn in this document, over the frame, never inside it (the
  // sender's CSS comes after ours there). One session at a time, bound to
  // the message identity taken when L was pressed; the label → URL map is
  // frozen when it opens, and a resize, a scroll (the pane's, the window's
  // or the frame's), the frame resizing or being replaced, or the cursor
  // moving (cancel/cancelFor) closes it. It takes every key (pendingKey).

  var H = null;

  // The frame's document is the sender's, same-origin: read it through the
  // prototypes, so a named element can't stand in for a method.
  function docRoot(doc) {
    return Object.getOwnPropertyDescriptor(Document.prototype, 'documentElement').get.call(doc);
  }

  // hintItems collects the labelable links of frame, clipped to bounds (app
  // coordinates): anchors with an http(s) or mailto href, a client rect with
  // area that survives frame ∩ bounds, whose centre hits the anchor (or
  // something inside it) in the frame and hits the frame in this document,
  // and not faded under 0.5 opacity. At most HINT_MAX, in document order.
  function hintItems(frame, bounds, origins) {
    var doc = frame.contentDocument;
    if (!doc) return { items: [], clip: null };
    var fr = frame.getBoundingClientRect();
    var ox = fr.left + frame.clientLeft, oy = fr.top + frame.clientTop;
    var clip = intersect({ left: ox, top: oy, right: ox + frame.clientWidth, bottom: oy + frame.clientHeight }, bounds);
    if (!clip) return { items: [], clip: null };
    var root = docRoot(doc);
    if (!root) return { items: [], clip: clip };
    var anchors = Element.prototype.querySelectorAll.call(root, 'a[href]');
    var getAttr = Element.prototype.getAttribute, rects = Element.prototype.getClientRects;
    var contains = Node.prototype.contains, fromPoint = Document.prototype.elementFromPoint;
    var items = [];
    for (var i = 0; i < anchors.length && i < 2000 && items.length < HINT_MAX; i++) {
      var a = anchors[i];
      var dest = hintURL(getAttr.call(a, 'href'), origins);
      if (!dest) continue;
      var rs = rects.call(a), box = null;
      for (var j = 0; j < rs.length && !box; j++) {
        var r = rs[j];
        if (!(r.width > 0 && r.height > 0)) continue;
        box = intersect({ left: r.left + ox, top: r.top + oy, right: r.right + ox, bottom: r.bottom + oy }, clip);
      }
      if (!box) continue;
      var cx = (box.left + box.right) / 2, cy = (box.top + box.bottom) / 2;
      var hit = fromPoint.call(doc, cx - ox, cy - oy);
      if (!hit || !contains.call(a, hit)) continue;
      if (document.elementFromPoint(cx, cy) !== frame) continue;
      if (faded(a, root)) continue;
      items.push({ dest: dest, box: box });
    }
    return { items: items, clip: clip };
  }

  function faded(a, root) {
    var o = 1;
    for (var n = a; n && o >= 0.5; n = n === root ? null : n.parentElement) {
      var v = parseFloat(getComputedStyle(n).opacity);
      if (v >= 0 && v <= 1) o *= v;
    }
    return o < 0.5;
  }

  // hints opens link hints on ident's message. ctx (app.js):
  //   frame      the message's iframe.mail
  //   bounds     {left, top, right, bottom}: the thread pane ∩ the viewport
  //              above the key bar, in this document's coordinates
  //   status(n)  show node n on the status line until status(null)
  //   statusBox()  the status line's element: its own scrolling (a long
  //              destination) doesn't close hints, and the inspection keys
  //              scroll it
  //   flash(text, kind)
  //   onClose()  the session has ended
  function hints(ident, e, ctx) {
    if (H || S || !ident || (e && e.repeat)) return;
    var got = ctx.frame ? hintItems(ctx.frame, ctx.bounds, appOrigins()) : { items: [] };
    if (!got.items.length) { ctx.flash('No links in view to label'); return; }
    var labels = hintLabels(got.items.length);
    var ov = el('div');
    ov.id = 'hints';
    ov.setAttribute('aria-hidden', 'true');
    var c = got.clip;
    ov.style.left = c.left + 'px';
    ov.style.top = c.top + 'px';
    ov.style.width = (c.right - c.left) + 'px';
    ov.style.height = (c.bottom - c.top) + 'px';
    var labs = got.items.map(function (it, i) {
      var lab = el('span', 'hint', labels[i]);
      ov.appendChild(lab);
      return lab;
    });
    // Each label's whole box goes inside the clip (placeLabel): measured
    // once laid out, all reads before any write. One that can't fit gets
    // no label, and its link none.
    document.body.appendChild(ov);
    var sizes = labs.map(function (lab) { return [lab.offsetWidth, lab.offsetHeight]; });
    var map = {}, kept = [];
    got.items.forEach(function (it, i) {
      var at = placeLabel(it.box, c, sizes[i][0], sizes[i][1]);
      if (!at) { labs[i].remove(); return; }
      labs[i].style.left = at.left + 'px';
      labs[i].style.top = at.top + 'px';
      map[labels[i]] = { dest: it.dest, el: labs[i] };
      kept.push(labels[i]);
    });
    if (!kept.length) { ov.remove(); ctx.flash('No links in view to label'); return; }
    var h = H = { ident: ident, ctx: ctx, labels: kept, map: map, ov: ov, st: { typed: '', selected: null } };
    h.close = function () { if (H === h) closeHints(); };
    // Any scroll closes them but the status line's own: a long destination
    // scrolls there, and reading it mustn't end the session.
    h.onScroll = function (ev) {
      var box = ctx.statusBox && ctx.statusBox(), t = ev && ev.target;
      if (box && t && t.nodeType === 1 && box.contains(t)) return;
      h.close();
    };
    var fdoc = ctx.frame.contentDocument;
    window.addEventListener('resize', h.close);
    document.addEventListener('scroll', h.onScroll, true);
    if (fdoc) fdoc.addEventListener('scroll', h.close, true);
    h.fdoc = fdoc;
    // A new document in the frame (a re-render) is new links.
    h.frame = ctx.frame;
    h.frame.addEventListener('load', h.close);
    // The labels sit where the links were: the frame changing size (an
    // image arriving) moves them.
    var w = ctx.frame.offsetWidth, ht = ctx.frame.offsetHeight;
    h.ro = new ResizeObserver(function () {
      if (ctx.frame.offsetWidth !== w || ctx.frame.offsetHeight !== ht) h.close();
    });
    h.ro.observe(ctx.frame);
    ctx.status(hintPrompt(kept.length));
  }

  function hintPrompt(n) {
    var p = el('span');
    p.appendChild(document.createTextNode(n + ' link' + (n === 1 ? '' : 's') + ' · type a label · '));
    p.appendChild(el('kbd', null, 'Esc'));
    p.appendChild(document.createTextNode(' closes'));
    return p;
  }

  function closeHints() {
    if (!H) return;
    var h = H;
    H = null;
    window.removeEventListener('resize', h.close);
    document.removeEventListener('scroll', h.onScroll, true);
    if (h.fdoc) h.fdoc.removeEventListener('scroll', h.close, true);
    h.frame.removeEventListener('load', h.close);
    if (h.ro) h.ro.disconnect();
    h.ov.remove();
    h.ctx.status(null);
    if (h.ctx.onClose) h.ctx.onClose();
  }

  function hintKeydown(e) {
    var h = H;
    e.preventDefault();
    if (e.stopPropagation) e.stopPropagation();
    var r = hintKey(h.st, e, h.labels);
    h.st = r.st;
    if (r.act === 'close') { closeHints(); return; }
    if (r.act === 'open') { openHint(h); return; }
    if (r.act === 'scroll') { scrollStatus(h, r); return; }
    if (r.act === 'none') return;
    Object.keys(h.map).forEach(function (l) {
      var m = h.map[l];
      m.el.classList.toggle('off', l.indexOf(h.st.typed) !== 0);
      m.el.classList.toggle('on', l === h.st.selected);
    });
    h.ctx.status(h.st.selected ? selection(h.map[h.st.selected].dest) : hintPrompt(h.labels.length));
    shade(h);
  }

  // shade hides, while a label is selected, every label whose box meets
  // the status line: nothing may sit over the destination Enter opens (the
  // key bar stacks above the labels too, app.css).
  function shade(h) {
    var box = h.st.selected && h.ctx.statusBox && h.ctx.statusBox();
    var sr = box ? box.getBoundingClientRect() : null;
    Object.keys(h.map).forEach(function (l) {
      var m = h.map[l];
      m.el.classList.remove('under');
      if (sr && intersect(m.el.getBoundingClientRect(), sr)) m.el.classList.add('under');
    });
  }

  // scrollStatus scrolls the status line (a long destination) for an
  // inspection key; the page never moves (its scroll would close hints).
  function scrollStatus(h, r) {
    var box = h.ctx.statusBox && h.ctx.statusBox();
    if (!box) return;
    if (r.by === 'end') { box.scrollTop = r.dir > 0 ? box.scrollHeight : 0; return; }
    var line = parseFloat(getComputedStyle(box).lineHeight) || 18;
    box.scrollTop += r.dir * (r.by === 'page' ? Math.max(line, box.clientHeight - line) : line);
  }

  // selection is the status line for a selected label: the full
  // destination under the confirmation's rules, and what Enter will do.
  function selection(dest) {
    var p = el('span', 'hintsel');
    if (dest.refused) {
      // Nothing opens, so a cut can hide nothing that runs.
      p.appendChild(el('span', 'bad', 'Refused (' + dest.refused + '): '));
      p.appendChild(field(dest.url, { cls: 'url', cap: WHOLE.url }));
      return p;
    }
    p.appendChild(el('kbd', null, '⏎'));
    p.appendChild(document.createTextNode(dest.kind === 'mailto' ? ' writes to ' : ' opens '));
    p.appendChild(field(dest.url, { cls: 'url' }));
    if (dest.kind === 'web' && redirector(dest.url)) p.appendChild(el('span', 'redirect', ' (redirect)'));
    return p;
  }

  // openHint opens the selection: http(s) through the browser-open check
  // again; mailto as a body click opens it, in a new window, so the
  // browser hands it to its mail handler.
  function openHint(h) {
    var dest = h.map[h.st.selected].dest, ctx = h.ctx;
    closeHints();
    if (dest.refused) { ctx.flash('Refused for safety: ' + dest.refused, 'error'); return; }
    if (dest.kind === 'mailto') {
      window.open(dest.url, '_blank', 'noopener,noreferrer');
      ctx.flash('Opened the mail link');
      return;
    }
    var c = openInBrowser(dest.url);
    if (c.ok) ctx.flash('Opened ' + destination(c.url));
    else ctx.flash('Refused for safety: ' + c.reason, 'error');
  }

  // yieldStatus is called before anything else writes the status line: a
  // hint session owns it while up, and a line that no longer shows the
  // selection must not leave Enter armed for it, so the session ends.
  function yieldStatus() { if (H) closeHints(); }

  A.hints = hints;
  A.closeHints = closeHints;
  A.yieldStatus = yieldStatus;
  A.hintsOpen = function () { return !!H; };
})(typeof window !== 'undefined' ? window : this);
