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
