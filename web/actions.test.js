// node --test web/actions.test.js — the pure parts of message actions
// (web/static/actions.js). Lives outside web/static so the embed doesn't serve it.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const A = require('./static/actions.js');

const APP = ['http://pneu.localhost:8025', 'http://127.0.0.1:8025'];

test('visible: bidi and format controls drawn as escapes', () => {
  assert.equal(A.visible('evil\u202Egpj.exe'), 'evil\\u{202E}gpj.exe');
  assert.equal(A.visible('a\u200Bb\u200D\uFEFF\u00AD'), 'a\\u{200B}b\\u{200D}\\u{FEFF}\\u{00AD}');
  assert.equal(A.visible('\u2066x\u2069\u061C'), '\\u{2066}x\\u{2069}\\u{061C}');
  // Tag characters (Cf, astral) and a lone surrogate.
  assert.equal(A.visible('x\u{E0041}'), 'x\\u{E0041}');
  assert.equal(A.visible('x\uD800y'), 'x\\u{D800}y');
  // Line breaks and separators on a one-line field; kept in a multiline one.
  assert.equal(A.visible('a\nb\r\u2028\u0085'), 'a\\u{000A}b\\u{000D}\\u{2028}\\u{0085}');
  assert.equal(A.visible('a\nb\tc\rd', true), 'a\nb\tc\\u{000D}d');
  // Ordinary non-ASCII text is left alone.
  assert.equal(A.visible('Zoë — 東京 مرحبا'), 'Zoë — 東京 مرحبا');
});

test('segments: plain runs and controls apart', () => {
  assert.deepEqual(A.segments('ab\u202Ecd'), [{ t: 'ab' }, { c: '\\u{202E}' }, { t: 'cd' }]);
  assert.deepEqual(A.segments(''), []);
  assert.deepEqual(A.segments(null), []);
});

test('cap: by code points, marked', () => {
  assert.deepEqual(A.cap('abc', 3), { text: 'abc', cut: false });
  assert.deepEqual(A.cap('abcd', 3), { text: 'abc…', cut: true });
  assert.deepEqual(A.cap('😀😀😀', 2), { text: '😀😀…', cut: true });
});

test('browserCheck: ordinary web addresses pass, normalized', () => {
  assert.deepEqual(A.browserCheck('https://news.example.com/unsub?u=1', APP), { ok: true, url: 'https://news.example.com/unsub?u=1' });
  assert.equal(A.browserCheck('http://example.org', APP).url, 'http://example.org/');
  assert.equal(A.browserCheck('https://bücher.example/x', APP).url, 'https://xn--bcher-kva.example/x');
});

test('browserCheck: refusals', () => {
  const no = (u) => assert.equal(A.browserCheck(u, APP).ok, false, u);
  no('javascript:alert(1)');
  no('data:text/html,hi');
  no('mailto:a@example.com');
  no('file:///etc/passwd');
  no('ftp://example.com/');
  no('not a url');
  no('http://pneu.localhost:8025/tag');
  no('http://127.0.0.1:8025/');
  no('http://127.0.0.1/');
  no('http://10.0.0.1/');
  no('http://2130706433/');
  no('http://0x7f.1/');
  no('http://1.2.3/');
  no('http://[::1]/');
  no('https://[2001:db8::1]:8443/');
  no('http://localhost:3000/');
  no('http://LOCALHOST./');
  no('http://router.localhost/');
  no('http://router/');
  no('http://intranet./');
});

test('destination: scheme, host, port unless default', () => {
  assert.equal(A.destination('https://list.example.com/u?t=1'), 'https://list.example.com');
  assert.equal(A.destination('https://list.example.com:443/u'), 'https://list.example.com');
  assert.equal(A.destination('https://list.example.com:8443/u'), 'https://list.example.com:8443');
  assert.equal(A.destination('https://bücher.example/'), 'https://xn--bcher-kva.example');
  assert.equal(A.destination('mailto:x@y'), null);
  assert.equal(A.destination('%%'), null);
});

test('Arming: drawn and the initiating key released', () => {
  const X = { code: 'KeyX', key: 'X' };
  const a = new A.Arming(X);
  assert.equal(a.accept({ key: 'y' }), false);
  a.rendered();
  assert.equal(a.accept({ key: 'y' }), false, 'X still down');
  a.keyup({ code: 'KeyY', key: 'y' });
  assert.equal(a.armed(), false, 'another key up is not X');
  a.keyup({ code: 'KeyX', key: 'x' }); // shift let go first: still X
  assert.equal(a.accept({ key: 'y' }), true);
  assert.equal(a.accept({ key: 'y', repeat: true }), false, 'repeats never');
});

test('Arming: released before drawn still waits for the draw', () => {
  const a = new A.Arming({ code: 'KeyX', key: 'X' });
  a.keyup({ code: 'KeyX', key: 'X' });
  assert.equal(a.armed(), false);
  a.rendered();
  assert.equal(a.armed(), true);
});

test('Arming: no code falls back to the key; a click needs only the draw', () => {
  const a = new A.Arming({ code: '', key: 'X' });
  a.rendered();
  a.keyup({ code: '', key: 'x' });
  assert.equal(a.armed(), true);
  const c = new A.Arming(null);
  assert.equal(c.armed(), false);
  c.rendered();
  assert.equal(c.armed(), true);
});

test('previewURL: account escaped, msgid as the page carries it', () => {
  assert.equal(A.previewURL('work', 'abc%40x.example'), '/unsubscribe/work/abc%40x.example');
  assert.equal(A.previewURL('a/b', 'm%2Fn%40x', 2), '/unsubscribe/a%2Fb/m%2Fn%40x?after=2');
  assert.equal(A.previewURL('p', 'm', 0), '/unsubscribe/p/m?after=0');
});

test('dialogKey: nothing before arming', () => {
  const keys = { y: () => {}, Escape: () => {} };
  for (const key of ['y', 'Escape', 'Enter', ' ', 'Tab', 'ArrowDown', 'PageDown']) {
    for (const target of ['button', 'summary', 'other']) {
      assert.equal(A.dialogKey({ key }, false, keys, target), 'drop', key + ' on ' + target);
    }
  }
});

test('dialogKey: armed, the phase keys act; never on a repeat or with modifiers', () => {
  const keys = { y: () => {}, n: () => {}, Escape: () => {} };
  assert.equal(A.dialogKey({ key: 'y' }, true, keys, 'other'), 'act');
  assert.equal(A.dialogKey({ key: 'y' }, true, keys, 'summary'), 'act');
  assert.equal(A.dialogKey({ key: 'Escape' }, true, keys, 'button'), 'act');
  assert.equal(A.dialogKey({ key: 'y', repeat: true }, true, keys, 'other'), 'drop');
  assert.equal(A.dialogKey({ key: 'y', ctrlKey: true }, true, keys, 'other'), 'drop');
  assert.equal(A.dialogKey({ key: 'y', isComposing: true }, true, keys, 'other'), 'drop');
  assert.equal(A.dialogKey({ key: 'e' }, true, keys, 'other'), 'drop', 'not this phase\'s key');
  assert.equal(A.dialogKey({ key: 'toString' }, true, keys, 'other'), 'drop');
});

test('dialogKey: armed, native inspection', () => {
  const keys = { y: () => {} };
  assert.equal(A.dialogKey({ key: 'Tab' }, true, keys, 'other'), 'native');
  assert.equal(A.dialogKey({ key: 'Tab', shiftKey: true }, true, keys, 'summary'), 'native');
  assert.equal(A.dialogKey({ key: 'Enter' }, true, keys, 'summary'), 'native');
  assert.equal(A.dialogKey({ key: ' ' }, true, keys, 'summary'), 'native');
  assert.equal(A.dialogKey({ key: 'Enter', repeat: true }, true, keys, 'summary'), 'drop');
  for (const key of ['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' ']) {
    assert.equal(A.dialogKey({ key }, true, keys, 'other'), 'native', key);
    assert.equal(A.dialogKey({ key, repeat: true }, true, keys, 'other'), 'native', key + ' held');
  }
  assert.equal(A.dialogKey({ key: 'Enter' }, true, keys, 'other'), 'drop');
});

test('dialogKey: a focused button takes Enter/Space only armed, never natively', () => {
  const keys = { y: () => {} };
  assert.equal(A.dialogKey({ key: 'Enter' }, true, keys, 'button'), 'press');
  assert.equal(A.dialogKey({ key: ' ' }, true, keys, 'button'), 'press');
  assert.equal(A.dialogKey({ key: 'Enter' }, false, keys, 'button'), 'drop');
  assert.equal(A.dialogKey({ key: ' ', repeat: true }, true, keys, 'button'), 'drop');
  assert.equal(A.dialogKey({ key: 'ArrowDown' }, true, keys, 'button'), 'drop');
});

// ---- the DOM half, on a small fake document -----------------------------

const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');

class FakeEl {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.parent = null;
    this.listeners = {};
    this.className = '';
    this.tabIndex = -1;
    this.attrs = {};
    this.open = false;
    this._text = '';
  }
  set textContent(t) { this.children = []; this._text = String(t); }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  appendChild(c) { c.parent = this; this.children.push(c); return c; }
  replaceChildren(...kids) { this.children = []; kids.forEach((k) => this.appendChild(k)); }
  setAttribute(k, v) { this.attrs[k] = v; }
  addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); }
  closest(sel) {
    for (let n = this; n; n = n.parent) if (n.tagName && n.tagName.toLowerCase() === sel) return n;
    return null;
  }
  contains(n) { for (; n; n = n.parent) if (n === this) return true; return false; }
  focus() {}
  remove() { this.removed = true; }
  showModal() { this.open = true; }
  close() { this.open = false; }
  all(pred, out = []) { if (pred(this)) out.push(this); this.children.forEach((c) => c.all && c.all(pred, out)); return out; }
  dispatch(type, ev) {
    const e = Object.assign({ target: this, prevented: false, stopped: false }, ev);
    e.preventDefault = () => { e.prevented = true; };
    e.stopPropagation = () => { e.stopped = true; };
    for (let n = this; n && !e.stopped; n = n.parent) (n.listeners[type] || []).forEach((f) => f(e));
    return e;
  }
}

function loadDOM() {
  const calls = [];
  const pending = [];
  const frames = [];
  const body = new FakeEl('body');
  body.dataset = {};
  const document = {
    body,
    createElement: (t) => new FakeEl(t),
    createTextNode: (t) => ({ textContent: t }),
  };
  const win = { addEventListener() {}, removeEventListener() {}, open() {} };
  // fetch: each call waits for the test to answer it.
  const fetch = (url, opts) => new Promise((resolve, reject) => {
    calls.push({ url, opts });
    const signal = opts && opts.signal;
    if (signal) signal.addEventListener('abort', () => reject(new Error('aborted')));
    pending.push((data, status) => resolve({
      ok: (status || 200) < 400, status: status || 200, json: () => Promise.resolve(data),
    }));
  });
  const ctx = vm.createContext({
    window: win, document, fetch, location: { origin: 'http://pneu.localhost:8025' },
    AbortController, URL, URLSearchParams, setTimeout, clearTimeout,
    requestAnimationFrame: (cb) => frames.push(cb),
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/actions.js'), 'utf8'), ctx);
  const flushFrames = () => { while (frames.length) frames.shift()(); };
  return { A: win.Pneu.actions, calls, pending, body, flushFrames };
}

const tick = () => new Promise((r) => setImmediate(r));
const keyEv = (key, extra) => Object.assign({ key, code: key.length === 1 ? 'Key' + key.toUpperCase() : key }, extra);

test('pending preview: every key is taken, Escape cancels, a late answer shows nothing', async () => {
  const D = loadDOM();
  const flashes = [];
  const ctx = { flash: (t) => flashes.push(t), archive() {}, onClose() {} };
  D.A.unsubscribe({ account: 'p', msgid: 'm%40x' }, keyEv('X', { code: 'KeyX' }), ctx);
  assert.equal(D.calls.length, 1);
  assert.equal(D.A.pendingPreview(), true);
  for (const key of ['e', '#', 't', 'r', 'a', 'j', 'Enter', 'y', 'X']) {
    const ev = keyEv(key);
    let prevented = false;
    ev.preventDefault = () => { prevented = true; };
    assert.equal(D.A.pendingKey(ev), true, key + ' taken');
    assert.equal(prevented, true, key + ' prevented');
    assert.equal(D.A.pendingPreview(), true, key + ' leaves it pending');
  }
  // A repeated Escape (held from before) doesn't cancel; a fresh one does.
  assert.equal(D.A.pendingKey(keyEv('Escape', { repeat: true, preventDefault() {} })), true);
  assert.equal(D.A.pendingPreview(), true);
  assert.equal(D.A.pendingKey(keyEv('Escape', { preventDefault() {} })), true);
  assert.equal(D.A.pendingPreview(), false);
  assert.equal(D.A.active(), false);
  // Nothing pending: keys go on to the app.
  assert.equal(D.A.pendingKey(keyEv('e', { preventDefault() { throw new Error('taken'); } })), false);
  // The (aborted) answer arriving late opens no dialog and flashes nothing.
  D.pending[0]({ method: 'mailto', token: 't' });
  await tick();
  assert.equal(D.body.children.length, 0);
  assert.deepEqual(flashes, []);
});

async function openDialog(data) {
  const D = loadDOM();
  D.A.unsubscribe({ account: 'p', msgid: 'm%40x' }, keyEv('X', { code: 'KeyX' }), { flash() {}, archive() {}, onClose() {} });
  D.pending.shift()(data);
  await tick(); await tick();
  const dlg = D.body.children[0];
  assert.ok(dlg && dlg.open, 'dialog open');
  const posts = () => D.calls.filter((c) => c.url === '/unsubscribe').length;
  const arm = () => { D.flushFrames(); D.A.keyup({ code: 'KeyX', key: 'X' }); };
  return { D, dlg, posts, arm };
}

const ONECLICK = { method: 'one-click', url: 'https://list.example.com/u?t=1', signedBy: 'list.example.com', token: 'tok', index: 0 };

test('dialog: before arming every key is dropped, Enter on the confirm button included', async () => {
  const { dlg, posts } = await openDialog(ONECLICK);
  const confirm = dlg.all((n) => n.tagName === 'BUTTON')[0];
  for (const key of ['Enter', ' ', 'y', 'Tab']) {
    const e = confirm.dispatch('keydown', keyEv(key));
    assert.equal(e.prevented, true, key);
    assert.equal(e.stopped, true, key + ' stopped');
  }
  // A key-made click (no pointerdown) does nothing either.
  confirm.dispatch('click', {});
  assert.equal(posts(), 0);
  assert.equal(dlg.open, true);
});

test('dialog: armed, inspection keys are native and stay in the dialog; Enter on the confirm button confirms', async () => {
  const { dlg, posts, arm } = await openDialog(ONECLICK);
  arm();
  const summary = dlg.all((n) => n.tagName === 'SUMMARY')[0];
  const url = dlg.all((n) => n.tagName === 'BDI' && /\burl\b/.test(n.className))[0];
  assert.equal(url.tabIndex, 0, 'the full URL is focusable');
  assert.notEqual(summary.tabIndex, -1, 'the disclosure is focusable');
  for (const [node, key] of [[summary, 'Enter'], [summary, ' '], [dlg, 'Tab'], [url, 'ArrowDown'], [url, 'PageDown'], [url, 'End']]) {
    const e = node.dispatch('keydown', keyEv(key));
    assert.equal(e.prevented, false, key + ' native');
    assert.equal(e.stopped, true, key + ' kept from the app');
  }
  // A key the phase doesn't know is dropped.
  assert.equal(dlg.dispatch('keydown', keyEv('e')).prevented, true);
  assert.equal(posts(), 0);
  // A key-made click still does nothing; Enter on the button, armed, does.
  const confirm = dlg.all((n) => n.tagName === 'BUTTON')[0];
  confirm.dispatch('click', {});
  assert.equal(posts(), 0);
  const e = confirm.dispatch('keydown', keyEv('Enter'));
  assert.equal(e.prevented, true);
  assert.equal(posts(), 1);
});

test('dialog: y confirms only once armed', async () => {
  const { dlg, posts, arm } = await openDialog(ONECLICK);
  dlg.dispatch('keydown', keyEv('y'));
  assert.equal(posts(), 0);
  arm();
  dlg.dispatch('keydown', keyEv('y', { repeat: true }));
  assert.equal(posts(), 0);
  dlg.dispatch('keydown', keyEv('y'));
  assert.equal(posts(), 1);
});

test('dialog: a mailto recipient is shown whole, wrapped; one past 254 is not offered', async () => {
  const to = 'l'.repeat(64) + '@' + 'd'.repeat(180) + '.example';
  const mailto = { method: 'mailto', to, from: 'Robin <r@x.example>', account: 'p', subject: 's', body: 'b', token: 't', index: 0 };
  let { dlg } = await openDialog(mailto);
  assert.ok(dlg.textContent.includes(to), 'recipient in full');
  assert.equal(dlg.all((n) => n.tagName === 'BUTTON').length, 2, 'confirm offered');
  assert.equal(dlg.all((n) => n.tagName === 'PRE')[0].tabIndex, 0, 'body focusable');
  ({ dlg } = await openDialog(Object.assign({}, mailto, { to: 'l'.repeat(64) + '@' + 'd'.repeat(190) + '.example' })));
  const labels = dlg.all((n) => n.tagName === 'BUTTON').map((b) => b.textContent);
  assert.deepEqual(labels, ['Esc Close'], 'no confirm');
});

test('dialogKey: armed, Tab moves focus even from a button', () => {
  assert.equal(A.dialogKey({ key: 'Tab' }, true, {}, 'button'), 'native');
  assert.equal(A.dialogKey({ key: 'Tab', shiftKey: true }, true, {}, 'button'), 'native');
  assert.equal(A.dialogKey({ key: 'Tab' }, false, {}, 'button'), 'drop');
});

// ---- declared action (o) ------------------------------------------------

const ld = (o) => JSON.stringify(o);
const email = (action, extra) => Object.assign({ '@context': 'http://schema.org', '@type': 'EmailMessage', potentialAction: action }, extra);
const PR = 'https://github.com/northwind/app/pull/4821';

test('declaredAction: GitHub shape, as an array, with url and target', () => {
  const block = ld([email({ '@type': 'ViewAction', target: PR, url: PR, name: 'View Pull Request' },
    { description: 'View this Pull Request on GitHub', publisher: { '@type': 'Organization', name: 'GitHub', url: 'https://github.com' } })]);
  assert.deepEqual(A.declaredAction([block], APP), { url: PR, name: 'View Pull Request' });
});

test('declaredAction: url, target string, target {url}; normalized', () => {
  assert.deepEqual(A.declaredAction([ld(email({ '@type': 'ViewAction', url: PR, name: 'x' }))], APP), { url: PR, name: 'x' });
  assert.deepEqual(A.declaredAction([ld(email({ '@type': 'ViewAction', target: PR, name: 'x' }))], APP), { url: PR, name: 'x' });
  assert.deepEqual(A.declaredAction([ld(email({ '@type': 'TrackAction', target: { '@type': 'EntryPoint', url: PR }, name: 'x' }))], APP), { url: PR, name: 'x' });
  assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', url: 'HTTPS://GitHub.COM' }))], APP).url, 'https://github.com/');
});

test('declaredAction: the action types, bare or as schema.org IRIs; a missing name is the type', () => {
  for (const [t, label] of [['ViewAction', 'View'], ['TrackAction', 'Track'], ['ConfirmAction', 'Confirm'], ['SaveAction', 'Save'], ['RsvpAction', 'RSVP']]) {
    assert.equal(A.declaredAction([ld(email({ '@type': t, url: PR }))], APP).name, label);
  }
  assert.equal(A.declaredAction([ld(email({ '@type': 'http://schema.org/ViewAction', url: PR, name: 'n' }))], APP).name, 'n');
  assert.equal(A.declaredAction([ld(email({ '@type': ['Thing', 'ViewAction'], url: PR, name: 'n' }))], APP).name, 'n');
  assert.equal(A.declaredAction([ld(email({ '@type': 'ReplyAction', url: PR }))], APP), null);
  assert.equal(A.declaredAction([ld(email({ url: PR }))], APP), null, 'no type');
  assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', url: PR, name: '   ' }))], APP).name, 'View');
});

test('declaredAction: exactly one qualifying action; identical duplicates count once', () => {
  const a = { '@type': 'ViewAction', url: PR, name: 'View' };
  const b = { '@type': 'ViewAction', url: PR + '/files', name: 'Files' };
  assert.equal(A.declaredAction([ld(email([a, b]))], APP), null);
  assert.equal(A.declaredAction([ld(email(a)), ld(email(b))], APP), null, 'across blocks');
  assert.deepEqual(A.declaredAction([ld(email(a)), ld(email(a))], APP), { url: PR, name: 'View' });
  // A non-qualifying type beside it doesn't count.
  assert.deepEqual(A.declaredAction([ld(email([a, { '@type': 'ReplyAction', url: PR + '/r' }]))], APP), { url: PR, name: 'View' });
  // A second qualifying action with a bad URL still makes two.
  assert.equal(A.declaredAction([ld(email([a, { '@type': 'ViewAction', url: 'javascript:x' }]))], APP), null);
  // potentialAction anywhere: in @graph, nested.
  assert.deepEqual(A.declaredAction([ld({ '@graph': [{ '@type': 'Order', potentialAction: a }] })], APP), { url: PR, name: 'View' });
  assert.equal(A.declaredAction([], APP), null);
  assert.equal(A.declaredAction([ld(email(null))], APP), null);
});

test('declaredAction: urlTemplate refused; url and target must agree', () => {
  assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', url: PR, urlTemplate: PR + '{?q}' }))], APP), null);
  assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', target: { urlTemplate: PR + '{?q}' } }))], APP), null);
  assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', target: { url: PR, urlTemplate: PR } }))], APP), null);
  assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', url: PR, target: 'https://evil.example/' }))], APP), null);
  assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', target: [PR] }))], APP), null, 'a list');
  assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', url: { url: PR } }))], APP), null);
  assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', name: 'no url' }))], APP), null);
});

test('declaredAction: only absolute http(s) that passes the browser-open check', () => {
  const bad = [
    'javascript:alert(1)', 'data:text/html,<b>x</b>', '/status', 'status', '//github.com/x', 'https:github.com/x',
    'https:\\\\github.com\\x', 'https://git\nhub.com/', ' https://github.com/', 'https://127.0.0.1/', 'https://[::1]/',
    'http://2130706433/', 'https://localhost/', 'http://pneu.localhost:8025/status', 'https://intranet/',
    'https://github.com@evil.example/', 'ftp://github.com/', 'mailto:a@b.example', 'https://x.example/' + 'a'.repeat(5000),
  ];
  for (const url of bad) assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', url }))], APP), null, url);
});

test('declaredAction: bounds; past any, no action at all', () => {
  const good = ld(email({ '@type': 'ViewAction', url: PR, name: 'View' }));
  assert.ok(A.declaredAction([good, '{}', '{}', '{}'], APP), 'four blocks');
  assert.equal(A.declaredAction([good, '{}', '{}', '{}', '{}'], APP), null, 'five blocks');
  assert.equal(A.declaredAction([good, false], APP), null, 'a block the capture refused');
  const big = ld(email({ '@type': 'ViewAction', url: PR }, { description: 'x'.repeat(65536) }));
  assert.equal(A.declaredAction([big], APP), null, 'past 64 KiB');
  // 64 KiB counts bytes: 30000 three-byte characters are 90000 bytes.
  const wide = ld(email({ '@type': 'ViewAction', url: PR }, { description: '東'.repeat(30000) }));
  assert.equal(A.declaredAction([wide], APP), null, 'past 64 KiB in UTF-8');
  const nodes = ld(email({ '@type': 'ViewAction', url: PR }, { list: new Array(2000).fill(1) }));
  assert.equal(A.declaredAction([nodes], APP), null, 'past 2000 nodes');
  const fewer = ld(email({ '@type': 'ViewAction', url: PR }, { list: new Array(1980).fill(1) }));
  assert.ok(A.declaredAction([fewer], APP), 'under 2000 nodes');
  // Depth: [ {potentialAction: {target: {url: "…"}}} ] is 5 deep.
  assert.ok(A.declaredAction([ld([email({ '@type': 'ViewAction', target: { url: PR } })])], APP));
  assert.equal(A.declaredAction([ld([email({ '@type': 'ViewAction', url: PR }, { deep: { a: { b: { c: { d: 1 } } } } })])], APP), null, 'depth 7');
  assert.ok(A.declaredAction([ld([email({ '@type': 'ViewAction', url: PR }, { deep: { a: { b: { c: 1 } } } })])], APP), 'depth 6');
  // A block that isn't JSON contributes nothing.
  assert.deepEqual(A.declaredAction(['{"@type": "EmailMessage", "potentialAction": ', good], APP), { url: PR, name: 'View' });
});

test('declaredAction: the name collapsed, capped at 60 code points, controls kept for the chip to show', () => {
  const name = (n) => A.declaredAction([ld(email({ '@type': 'ViewAction', url: PR, name: n }))], APP).name;
  assert.equal(name('  View\n\tPull   Request '), 'View Pull Request');
  assert.equal(name('😀'.repeat(61)), '😀'.repeat(60) + '…');
  assert.equal(name('a‮b'), 'a‮b');
  assert.equal(A.visible(name('a‮b')), 'a\\u{202E}b');
  assert.equal(name('<img src=x onerror=alert(1)>'), '<img src=x onerror=alert(1)>', 'text, never markup');
});

test('declaredAction: __proto__ and friends are only keys', () => {
  const block = '{"__proto__": {"potentialAction": {"@type": "ViewAction", "url": "' + PR + '"}}, "@type": "EmailMessage"}';
  assert.deepEqual(A.declaredAction([block], APP), { url: PR, name: 'View' });
  assert.equal({}.potentialAction, undefined);
});

test('redirector: the explicit list, host or subdomain', () => {
  assert.equal(A.redirector('https://example.us1.list-manage.com/track/click?u=1'), true);
  assert.equal(A.redirector('https://u123.ct.sendgrid.net/ls/click?x'), true);
  assert.equal(A.redirector('https://mandrillapp.com/track/click/1'), true);
  assert.equal(A.redirector('https://mailchi.mp/x/y'), true);
  assert.equal(A.redirector('https://click.example.com/x'), false, 'a click.* host is not evidence');
  assert.equal(A.redirector('https://links.example.com/x'), false);
  assert.equal(A.redirector('https://notlist-manage.com/x'), false);
  assert.equal(A.redirector('https://list-manage.com.evil.example/x'), false);
  assert.equal(A.redirector('%%'), false);
});

test('chipParts: name, destination with a non-default port, redirect', () => {
  assert.deepEqual(A.chipParts({ url: PR, name: 'View Pull Request' }), { name: 'View Pull Request', dest: 'https://github.com', redirect: false });
  assert.deepEqual(A.chipParts({ url: 'https://shop.example:8443/o/1', name: 'Track' }), { name: 'Track', dest: 'https://shop.example:8443', redirect: false });
  assert.equal(A.chipParts({ url: 'https://x.us2.list-manage.com/c', name: 'n' }).redirect, true);
  assert.equal(A.chipParts({ url: 'javascript:x', name: 'n' }), null);
  assert.equal(A.chipParts(null), null);
});

test('browserCheck: a user name in the address is refused', () => {
  assert.equal(A.browserCheck('https://github.com@evil.example/', APP).ok, false);
  assert.equal(A.browserCheck('https://u:p@evil.example/', APP).ok, false);
  assert.equal(A.browserCheck('https://:p@evil.example/', APP).ok, false);
});

// ---- link hints (L) -------------------------------------------------------

test('hintLabels: home row, one length, prefix-free, the shortest that fits', () => {
  assert.deepEqual(A.hintLabels(0), []);
  assert.deepEqual(A.hintLabels(3), ['a', 's', 'd']);
  assert.equal(A.hintLabels(9).join(''), 'asdfghjkl');
  const ten = A.hintLabels(10);
  assert.deepEqual(ten.slice(0, 3), ['aa', 'as', 'ad']);
  assert.ok(ten.every((l) => l.length === 2));
  assert.ok(A.hintLabels(81).every((l) => l.length === 2));
  const max = A.hintLabels(A.HINT_MAX);
  assert.equal(max.length, 200);
  assert.equal(new Set(max).size, 200);
  assert.ok(max.every((l) => l.length === 3 && /^[asdfghjkl]+$/.test(l)));
  for (const l of max) assert.ok(!max.some((m) => m !== l && m.startsWith(l)), 'prefix-free');
});

test('intersect: overlap with area, or null', () => {
  const r = (left, top, right, bottom) => ({ left, top, right, bottom });
  assert.deepEqual(A.intersect(r(0, 0, 10, 10), r(5, 5, 20, 20)), r(5, 5, 10, 10));
  assert.equal(A.intersect(r(0, 0, 10, 10), r(10, 0, 20, 10)), null, 'touching edges');
  assert.equal(A.intersect(r(0, 0, 10, 10), r(20, 20, 30, 30)), null);
  assert.equal(A.intersect(r(0, 0, 10, 10), null), null);
  // Frame ∩ pane ∩ viewport, in any order.
  const frame = r(100, -200, 700, 2000), pane = r(90, 40, 800, 900), view = r(0, 0, 1280, 872);
  assert.deepEqual(A.intersect(A.intersect(frame, pane), view), r(100, 40, 700, 872));
});

test('hintURL: http(s) normalized or refused with a reason; mailto; nothing else', () => {
  assert.deepEqual(A.hintURL(' https://Example.COM/a ', APP), { kind: 'web', url: 'https://example.com/a' });
  assert.deepEqual(A.hintURL('mailto:a@b.example?subject=hi', APP), { kind: 'mailto', url: 'mailto:a@b.example?subject=hi' });
  assert.equal(A.hintURL('https://127.0.0.1/', APP).refused, 'an IP address');
  assert.equal(A.hintURL('http://pneu.localhost:8025/status', APP).refused, 'points at pneu itself');
  assert.equal(A.hintURL('https://x.example/' + 'a'.repeat(5000), APP).refused, 'too long to show in full');
  assert.equal(A.hintURL('javascript:x', APP), null);
  assert.equal(A.hintURL('#top', APP), null);
  assert.equal(A.hintURL('/status', APP), null);
  assert.equal(A.hintURL(null, APP), null);
});

test('hintKey: typing selects, Enter opens only a selection, Esc closes', () => {
  const labels = A.hintLabels(12); // aa as ad af ag ah aj ak al sa ss sd
  let st = { typed: '', selected: null };
  const k = (key, extra) => { const r = A.hintKey(st, Object.assign({ key }, extra), labels); st = r.st; return r.act; };
  assert.equal(k('Enter'), 'none', 'nothing selected');
  assert.equal(k('s'), 'narrow');
  assert.deepEqual(st, { typed: 's', selected: null });
  assert.equal(k('q'), 'none', 'not the alphabet');
  assert.equal(k('f'), 'none', 'sf is no label');
  assert.deepEqual(st, { typed: 's', selected: null });
  assert.equal(k('d'), 'select');
  assert.deepEqual(st, { typed: 'sd', selected: 'sd' });
  assert.equal(k('Enter', { repeat: true }), 'none', 'repeats never');
  assert.equal(k('a'), 'narrow', 'a letter after a selection starts over');
  assert.deepEqual(st, { typed: 'a', selected: null });
  assert.equal(k('j'), 'select');
  assert.equal(k('Backspace'), 'narrow');
  assert.deepEqual(st, { typed: 'a', selected: null });
  assert.equal(k('Backspace'), 'narrow');
  assert.equal(k('Backspace'), 'none');
  assert.equal(k('a'), 'narrow');
  assert.equal(k('s'), 'select');
  assert.equal(k('A'), 'none', 'labels are lower case');
  assert.equal(k('e', { ctrlKey: true }), 'none');
  assert.equal(k('e'), 'none', 'archive does nothing here');
  assert.deepEqual(st, { typed: 'as', selected: 'as' });
  assert.equal(k('Enter'), 'open');
  assert.equal(k('Escape', { repeat: true }), 'none');
  assert.equal(k('Escape'), 'close');
});

// ---- the chip (DOM half) ---------------------------------------------------

test('chip: o, the name and destination as isolated text, controls drawn, redirect marked', () => {
  const D = loadDOM();
  let opened = 0;
  const c = D.A.chip({ url: 'https://x.us2.list-manage.com:8443/c?u=1', name: 'Pay‮now <b>' }, () => opened++);
  assert.equal(c.tagName, 'BUTTON');
  assert.equal(c.textContent, 'oPay\\u{202E}now <b>→https://x.us2.list-manage.com:8443(redirect)');
  const bdis = c.all((n) => n.tagName === 'BDI');
  assert.deepEqual(bdis.map((b) => b.className), ['untrusted name', 'untrusted destination']);
  assert.deepEqual({ ...c.pneuAction }, { url: 'https://x.us2.list-manage.com:8443/c?u=1', name: 'Pay‮now <b>' });
  c.blur = () => {};
  const e = c.dispatch('click', {});
  assert.equal(e.stopped, true, 'not a header click');
  assert.equal(opened, 1);
  assert.equal(D.A.chip({ url: 'javascript:x', name: 'n' }, () => {}), null);
});

test('openAction: re-checked at open time', () => {
  const D = loadDOM();
  const c = D.A.chip({ url: PR, name: 'View' }, () => {});
  assert.deepEqual({ ...D.A.openAction(c) }, { ok: true, url: PR });
  c.pneuAction.url = 'http://pneu.localhost:8025/status';
  assert.equal(D.A.openAction(c).ok, false);
  assert.equal(D.A.openAction(null).ok, false);
});

// ---- review fixes (2026-09-30) ---------------------------------------------

test('primaryKey: o opens only with the chip wholly in view; otherwise reveals; repeats never', () => {
  const r = (left, top, right, bottom) => ({ left, top, right, bottom });
  const pane = r(300, 40, 1100, 840); // pane ∩ viewport, under the sticky title, above the key bar
  const chip = (top) => r(320, top, 700, top + 20);
  assert.equal(A.primaryKey({ key: 'o' }, chip(100), pane), 'open');
  assert.equal(A.primaryKey({ key: 'o', repeat: true }, chip(100), pane), 'none', 'a repeat does nothing, in view or not');
  assert.equal(A.primaryKey({ key: 'o', repeat: true }, chip(-400), pane), 'none');
  // Scrolled away above (a tall body being read), under the sticky title,
  // half under the key bar, below the fold, off to the side.
  assert.equal(A.primaryKey({ key: 'o' }, chip(-400), pane), 'reveal');
  assert.equal(A.primaryKey({ key: 'o' }, chip(30), pane), 'reveal', 'partly under the sticky title');
  assert.equal(A.primaryKey({ key: 'o' }, chip(830), pane), 'reveal', 'partly under the key bar');
  assert.equal(A.primaryKey({ key: 'o' }, chip(2000), pane), 'reveal');
  assert.equal(A.primaryKey({ key: 'o' }, r(1000, 100, 1200, 120), pane), 'reveal', 'past the right edge');
  assert.equal(A.primaryKey({ key: 'o' }, r(0, 0, 0, 0), pane), 'reveal', 'not laid out');
  // After the reveal scrolled it in, the next separate o opens.
  assert.equal(A.primaryKey({ key: 'o' }, chip(420), pane), 'open');
  // Layout rounding at the edges.
  assert.equal(A.primaryKey({ key: 'o' }, r(299.6, 39.6, 1100.4, 60), pane), 'open');
});

test('placeLabel: the whole label inside the clip at every edge; none if it cannot fit', () => {
  const clip = { left: 100, top: 50, right: 700, bottom: 450 }; // 600 × 400
  const w = 27, h = 16; // a 3-letter label
  const box = (left, top, right, bottom) => ({ left, top, right, bottom });
  const inClip = (at) => at.left >= 0 && at.top >= 0 && at.left + w <= 600 && at.top + h <= 400;
  // Top-left, a sliver at the top, bottom, left and right edges, a corner.
  const cases = [
    [box(100, 50, 400, 70), { left: 0, top: 0 }],
    [box(300, 50, 400, 52), { left: 200, top: 0 }],
    [box(300, 448, 400, 450), { left: 200, top: 384 }],
    [box(100, 200, 102, 220), { left: 0, top: 150 }],
    [box(698, 200, 700, 220), { left: 573, top: 150 }],
    [box(699, 449, 700, 450), { left: 573, top: 384 }],
  ];
  for (const [b, want] of cases) {
    const at = A.placeLabel(b, clip, w, h);
    assert.deepEqual(at, want, JSON.stringify(b));
    assert.ok(inClip(at));
  }
  assert.equal(A.placeLabel(box(100, 50, 101, 51), { left: 100, top: 50, right: 120, bottom: 60 }, w, h), null, 'no room');
  assert.equal(A.placeLabel(box(100, 50, 101, 51), clip, 0, h), null, 'not laid out');
});

test('declaredAction: no Unicode whitespace, C1 or format character in the URL', () => {
  for (const ch of [' ', ' ', ' ', ' ', ' ', ' ', ' ', '　', '\u0085', '᠎', '​', '‎', '‮', '⁦', '﻿', '­', '\u009b', '\ud800']) {
    const url = 'https://github.com/a' + ch + 'b';
    assert.equal(A.declaredAction([ld(email({ '@type': 'ViewAction', url }))], APP), null, 'U+' + ch.codePointAt(0).toString(16));
  }
  assert.ok(A.declaredAction([ld(email({ '@type': 'ViewAction', url: 'https://github.com/東京?q=é' }))], APP), 'other non-ASCII is fine');
});

test('declaredAction: duplicates compare on the full name, before the cap', () => {
  const long = 'x'.repeat(60);
  const a = { '@type': 'ViewAction', url: PR, name: long + ' one' };
  const b = { '@type': 'ViewAction', url: PR, name: long + ' two' };
  assert.equal(A.declaredAction([ld(email([a, b]))], APP), null, 'the same after the cap, different before');
  assert.deepEqual(A.declaredAction([ld(email([a, a]))], APP), { url: PR, name: long + '…' });
});

test('declaredAction: schema: and full-IRI @type values', () => {
  for (const t of ['https://schema.org/ViewAction', 'http://schema.org/ViewAction', 'schema:ViewAction']) {
    assert.deepEqual(A.declaredAction([ld(email({ '@type': t, url: PR }))], APP), { url: PR, name: 'View' }, t);
  }
  for (const t of ['https://schema.org.evil.example/ViewAction', 'https://example.com/ViewAction', 'Schema:ViewAction', 'viewaction']) {
    assert.equal(A.declaredAction([ld(email({ '@type': t, url: PR }))], APP), null, t);
  }
});

test('hintURL: an overlong mailto is labelled and refused, not dropped', () => {
  const long = 'mailto:a@b.example?body=' + 'x'.repeat(5000);
  assert.deepEqual(A.hintURL(long, APP), { kind: 'mailto', refused: 'too long to show in full', url: long });
});

test('hintKey: Shift+Enter and other modified keys drop; inspection keys scroll the status line', () => {
  const labels = A.hintLabels(3);
  const st = { typed: 'a', selected: 'a' };
  assert.equal(A.hintKey(st, { key: 'Enter', shiftKey: true }, labels).act, 'none');
  assert.equal(A.hintKey(st, { key: 'Enter' }, labels).act, 'open');
  assert.equal(A.hintKey(st, { key: 'Escape', shiftKey: true }, labels).act, 'close', 'Esc always closes');
  const scroll = (key, extra) => { const r = A.hintKey(st, Object.assign({ key }, extra), labels); return [r.act, r.by, r.dir]; };
  assert.deepEqual(scroll('ArrowDown'), ['scroll', 'line', 1]);
  assert.deepEqual(scroll('ArrowUp'), ['scroll', 'line', -1]);
  assert.deepEqual(scroll('PageDown'), ['scroll', 'page', 1]);
  assert.deepEqual(scroll('PageUp'), ['scroll', 'page', -1]);
  assert.deepEqual(scroll('End'), ['scroll', 'end', 1]);
  assert.deepEqual(scroll('Home'), ['scroll', 'end', -1]);
  assert.equal(scroll('ArrowDown', { repeat: true })[0], 'none');
  assert.equal(scroll('PageDown', { shiftKey: true })[0], 'none');
  // Scrolling leaves the selection armed.
  assert.deepEqual(A.hintKey(st, { key: 'ArrowDown' }, labels).st, st);
});

test('usable: the pane less the sticky and fixed chrome over it, in both layouts', () => {
  const r = (left, top, right, bottom) => ({ left, top, right, bottom });
  const view = r(0, 0, 800, 600);
  // Narrow: the window scrolls; the sticky header covers the top, the key bar the bottom.
  const header = r(0, 0, 800, 36), keybar = r(0, 572, 800, 600);
  assert.deepEqual(A.usable(r(0, -900, 800, 3000), view, [header, keybar]), r(0, 36, 800, 572));
  // Split: the thread pane on the right under the page header; its sticky
  // title covers its top; the list's title, beside it, covers nothing of it.
  const pane = r(560, 37, 1400, 872), title = r(560, 37, 1400, 60), listTitle = r(0, 37, 559, 60);
  assert.deepEqual(A.usable(pane, r(0, 0, 1400, 900), [r(0, 0, 1400, 37), title, listTitle, r(0, 872, 1400, 900)]), r(560, 60, 1400, 872));
  // A wrapped chip whose top row is behind the header, its centre below it: not in view.
  const chip = r(20, 20, 400, 60);
  assert.equal(A.primaryKey({ key: 'o' }, chip, A.usable(r(0, -900, 800, 3000), view, [header, keybar])), 'reveal');
  assert.equal(A.primaryKey({ key: 'o' }, chip, A.usable(r(0, -900, 800, 3000), view, [keybar])), 'open', 'without the header, it would open');
  // Nothing left: nothing is inside.
  assert.equal(A.inside(r(10, 10, 20, 20), A.usable(r(0, 0, 100, 30), view, [r(0, 0, 100, 40)])), false);
});
