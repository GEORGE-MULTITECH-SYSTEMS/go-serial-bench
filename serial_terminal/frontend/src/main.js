// Go Serial Bench — pure JS UI. No framework, no build-time deps beyond
// Vite bundling. Talks to Go via the Wails bridge:
//   window.go.main.App.<Method>(...)  -> Promise
//   window.runtime.EventsOn(name, fn)  <- pushed events
import './style.css';

const $ = (id) => document.getElementById(id);
const App = () => window.go.main.App;
const RT = () => window.runtime;

// ---------- state ----------
let isOpen = false;
let paused = false;
let pausedBuf = [];
let rxTotal = 0, txTotal = 0;
let repeatTimer = null, repeatN = 0;
const MAX_LINES = 3000;

// ---------- helpers ----------
function fmtBytes(n) {
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KiB';
  return (n / 1048576).toFixed(2) + ' MiB';
}
function setHealth(cls, label, sub) {
  const h = $('health');
  h.className = 'health ' + cls;
  $('healthLabel').textContent = label;
  $('healthSub').textContent = sub || '';
}
function toast(msg, isErr) {
  addLine({ ts: new Date().toTimeString().slice(0, 8) + '.000', kind: isErr ? 'err' : 'info', text: msg });
}

// ---------- monitor ----------
const mon = $('monitor');
function lineVisible(kind) {
  if (kind === 'rx') return $('showRx').checked;
  if (kind === 'tx') return $('showTx').checked;
  return $('showInfo').checked;
}
function addLine(l) {
  if (paused) { pausedBuf.push(l); if (pausedBuf.length > MAX_LINES) pausedBuf.shift(); return; }
  renderLine(l);
}
function renderLine(l) {
  const div = document.createElement('div');
  div.className = 'ln ' + l.kind + (lineVisible(l.kind) ? '' : ' hidden');
  div.dataset.kind = l.kind;
  const dir = l.kind === 'rx' ? 'RX' : l.kind === 'tx' ? 'TX' : l.kind === 'err' ? 'ERR' : '··';
  let body = '';
  if (l.kind === 'rx' || l.kind === 'tx') {
    const hex = $('showHex').checked ? `<span class="hex">${esc(l.hex)}</span>` : '';
    const asc = $('showAscii').checked ? `<span class="asc">${esc(l.text)}</span>` : '';
    body = `${hex}${asc}<span class="n">(${l.n})</span>`;
    div.dataset.hex = l.hex; div.dataset.text = l.text; div.dataset.n = l.n;
  } else {
    body = esc(l.text || '');
  }
  div.innerHTML = `<span class="ts">${l.ts}</span><span class="dir">${dir}</span><span class="body">${body}</span>`;
  mon.appendChild(div);
  while (mon.childElementCount > MAX_LINES) mon.removeChild(mon.firstChild);
  if ($('autoscroll').checked) mon.scrollTop = mon.scrollHeight;
}
function esc(s) { return String(s).replace(/[&<>]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' }[c])); }
function reflowVisibility() {
  for (const el of mon.children) {
    el.classList.toggle('hidden', !lineVisible(el.dataset.kind));
    if (el.dataset.kind === 'rx' || el.dataset.kind === 'tx') {
      const hex = $('showHex').checked ? `<span class="hex">${esc(el.dataset.hex)}</span>` : '';
      const asc = $('showAscii').checked ? `<span class="asc">${esc(el.dataset.text)}</span>` : '';
      el.querySelector('.body').innerHTML = `${hex}${asc}<span class="n">(${el.dataset.n})</span>`;
    }
  }
}
['showRx', 'showTx', 'showInfo', 'showHex', 'showAscii'].forEach((id) => $(id).addEventListener('change', reflowVisibility));

$('btnClear').onclick = () => { mon.innerHTML = ''; rxTotal = txTotal = 0; updCounters(); };
$('btnPause').onclick = () => {
  paused = !paused;
  $('btnPause').textContent = paused ? 'Resume' : 'Pause';
  mon.classList.toggle('paused', paused);
  if (!paused) { pausedBuf.forEach(renderLine); pausedBuf = []; }
};
$('btnCopy').onclick = async () => {
  const lines = [...mon.children].filter((e) => !e.classList.contains('hidden')).map((e) => e.innerText.replace(/\n/g, ' '));
  try { await navigator.clipboard.writeText(lines.join('\n')); toast(`copied ${lines.length} lines`); }
  catch (e) { toast('clipboard: ' + e, true); }
};
function updCounters() { $('cntRx').textContent = 'RX ' + fmtBytes(rxTotal); $('cntTx').textContent = 'TX ' + fmtBytes(txTotal); }

// ---------- ports ----------
// boundPort is the port this window is working with: the one that is open,
// or the one a recycle is trying to get back. It is tracked separately from
// the <select> because the select is rebuilt from what exists RIGHT NOW, and
// the whole point of an unplug is that the port we care about does not.
let boundPort = '';
let lastPortSig = '';

async function refreshPorts(opts) {
  const sel = $('port');
  const keep = (opts && opts.keep) || sel.value || boundPort;
  try {
    const list = (await App().ListPorts()) || [];

    // Nothing changed? Leave the DOM alone. This runs on every health tick,
    // and rebuilding an open <select> underneath the operator closes it.
    const sig = list.join(',') + '|' + keep;
    if (sig === lastPortSig) return;
    lastPortSig = sig;

    sel.innerHTML = '';
    for (const p of list) {
      const o = document.createElement('option');
      o.value = o.textContent = p;
      sel.appendChild(o);
    }

    // Keep the port we are bound to in the list even when it is NOT present,
    // marked as such. Dropping it silently re-points the selection at the
    // first surviving device — so after a cable pull the window showed COM11
    // selected while the supervisor was still recovering COM3, and pressing
    // Open would have connected to a different adapter than the one being
    // watched. A selection must never change because hardware moved.
    if (keep && !list.includes(keep)) {
      const o = document.createElement('option');
      o.value = keep;
      o.textContent = keep + ' — not present';
      sel.insertBefore(o, sel.firstChild);
    }
    if (!sel.options.length) {
      sel.innerHTML = '<option value="">(no COM ports)</option>';
      return;
    }
    if (keep) sel.value = keep;
  } catch (e) {
    toast('ListPorts: ' + e, true);
  }
}
$('btnRefresh').onclick = () => refreshPorts({ keep: $('port').value });

// ---------- open / close ----------
function cfgFromForm() {
  return {
    name: $('port').value, baud: $('baud').value, dataBits: $('dataBits').value,
    parity: $('parity').value, stopBits: $('stopBits').value, handshake: $('handshake').value,
    dtr: $('dtr').checked, rts: $('rts').checked,
    readGapMs: $('readGapMs').value, writeTimeoutMs: $('writeTimeoutMs').value,
  };
}
function setOpenUI(open, st) {
  isOpen = open;
  const b = $('btnOpen');
  b.textContent = open ? 'Close' : 'Open';
  b.classList.toggle('close', open);
  ['port', 'baud', 'dataBits', 'parity', 'stopBits', 'handshake', 'readGapMs', 'writeTimeoutMs', 'btnRefresh']
    .forEach((id) => ($(id).disabled = open));
  $('chipPort').textContent = open ? st.port : (st.port ? st.port + ' — lost' : 'no port');
  $('chipCfg').textContent = open ? st.config : '';
  document.title = 'Go Serial Bench — ' + (open ? st.port : 'idle');
  if (!open) { setHealth('', 'CLOSED', 'open a port'); stopRepeat(); }
}
$('btnOpen').onclick = async () => {
  try {
    if (!isOpen) {
      const c = cfgFromForm();
      if (!c.name) { toast('no port selected', true); return; }
      localStorage.setItem('gsb.cfg', JSON.stringify(c));
      await App().Open(c);
    } else {
      await App().Close();
    }
  } catch (e) { toast(String(e), true); }
  applyState(await App().State());
};

// ---------- lines ----------
$('dtr').onchange = () => App().SetDTR($('dtr').checked).catch((e) => toast(e, true));
$('rts').onchange = () => App().SetRTS($('rts').checked).catch((e) => toast(e, true));
$('btnBreak').onclick = () => App().SendBreak(100).catch((e) => toast(e, true));
$('btnPurge').onclick = () => App().Purge().then(() => toast('purged driver queues')).catch((e) => toast(e, true));
$('autoRecycle').onchange = () => App().SetAutoRecycle($('autoRecycle').checked);

// ---------- send ----------
async function doSend() {
  const t = $('sendText').value;
  if (!t) return;
  try {
    await App().Send(t, $('sendHex').checked, $('sendCR').checked, $('sendLF').checked);
  } catch (e) { toast('send: ' + e, true); stopRepeat(); }
}
$('btnSend').onclick = doSend;
$('sendText').addEventListener('keydown', (e) => { if (e.key === 'Enter') doSend(); });

function stopRepeat() {
  if (repeatTimer) clearInterval(repeatTimer);
  repeatTimer = null; $('btnRepeat').textContent = 'Start'; $('btnRepeat').classList.remove('on');
}
$('btnRepeat').onclick = () => {
  if (repeatTimer) { stopRepeat(); return; }
  const ms = Math.max(10, parseInt($('repeatMs').value, 10) || 500);
  repeatN = 0;
  repeatTimer = setInterval(async () => { await doSend(); repeatN++; $('repeatCount').textContent = `#${repeatN}`; }, ms);
  $('btnRepeat').textContent = 'Stop'; $('btnRepeat').classList.add('on');
};

// presets: click = load, shift+click = save
document.querySelectorAll('.preset').forEach((b) => {
  const key = 'gsb.preset' + b.dataset.slot;
  const refresh = () => b.classList.toggle('has', !!localStorage.getItem(key));
  refresh();
  b.onclick = (e) => {
    if (e.shiftKey) { localStorage.setItem(key, JSON.stringify({ t: $('sendText').value, hex: $('sendHex').checked })); refresh(); toast('preset ' + b.dataset.slot + ' saved'); }
    else { const v = localStorage.getItem(key); if (v) { const o = JSON.parse(v); $('sendText').value = o.t; $('sendHex').checked = o.hex; } }
  };
});

// ---------- state / health rendering ----------
function applyState(st) {
  if (!st) return;
  // Track the port this window is bound to, and keep the dropdown honest
  // without the operator having to press Refresh.
  //
  // The list used to rebuild ONLY on a manual Refresh, so after a cable pull
  // the window kept showing a stale set: the supervisor recovered COM3 by
  // itself and logged LINK RECYCLED OK, while the dropdown still showed only
  // COM11/COM12 and the operator reasonably concluded nothing had been
  // detected. The recovery was real and invisible.
  //
  // Binding is sticky only while the port is open or being recovered. After
  // a deliberate Close the operator's own selection is theirs to keep.
  if (st.open || st.vanished || st.degraded) boundPort = st.port || boundPort;
  else boundPort = '';
  refreshPorts({ keep: boundPort || $('port').value });

  setOpenUI(st.open, st);
  $('chipPid').textContent = 'pid ' + st.pid;
  const s = st.stats || {};
  // The watchdog is shown whether or not a port is installed. The supervisor
  // carries the count across an outage on purpose, because a drop reads as
  // "the program restarted"; blanking it here would throw that away at
  // exactly the moment it is most worth reading.
  $('wd').textContent = String(s.Watchdog ?? 0);
  $('wdAdv').textContent = st.open ? (st.wdAdvancing ? 'yes' : 'NO') : 'stalled';
  $('wdAdv').className = st.open ? (st.wdAdvancing ? 'ok' : 'bad') : 'warn';
  $('sRx').textContent = s.RxBytes ?? 0; $('sTx').textContent = s.TxBytes ?? 0;
  $('sReads').textContent = s.Reads ?? 0; $('sWrites').textContent = s.Writes ?? 0;
  $('sErr').textContent = s.Errors ?? 0; $('sStreak').textContent = s.ErrorStreak ?? 0;
  $('sStreak').className = (s.ErrorStreak ?? 0) > 0 ? 'warn' : '';
  $('sDeg').textContent = st.degraded ? 'YES' : 'no'; $('sDeg').className = st.degraded ? 'bad' : '';
  $('sRec').textContent = st.recycles ?? 0;
  // The facts the hardening pass added. They are rendered here because a
  // counter the server can reach but the page cannot show is a counter
  // nobody will ever act on: 'present' and 'vanished' are how an operator
  // tells an unplugged adapter from a quiet one, 'wedged' and 'leaked
  // handles' are how a driver that will not cancel I/O becomes visible
  // instead of being silently absorbed, and 'events dropped' keeps silence
  // that was CHOSEN distinguishable from silence that means breakage.
  const line = s.Line || {};
  $('sProbe').textContent = s.ProbeErrors ?? 0;
  $('sLineFP').textContent = (line.Frame ?? 0) + '/' + (line.Parity ?? 0);
  $('sLineOR').textContent = (line.Overrun ?? 0) + '/' + (line.RxOver ?? 0);
  $('sTOW').textContent = s.TimedOutWaits ?? 0;
  $('sPresent').textContent = st.port ? (st.present ? 'yes' : 'NO') : '—';
  $('sPresent').className = st.port && !st.present ? 'bad' : '';
  $('sVanished').textContent = st.vanished ? 'YES' : 'no';
  $('sVanished').className = st.vanished ? 'bad' : '';
  $('sWedged').textContent = st.wedged ? 'YES' : 'no';
  $('sWedged').className = st.wedged ? 'bad' : '';
  $('sLeaked').textContent = st.leakedHandles ?? 0;
  $('sLeaked').className = (st.leakedHandles ?? 0) > 0 ? 'bad' : '';
  $('sSup').textContent = st.eventsSuppressed ?? 0;
  $('sSup').className = (st.eventsSuppressed ?? 0) > 0 ? 'warn' : '';
  $('sTxQ').textContent = st.txQueued ?? 0;
  $('sTxD').textContent = st.txDropped ?? 0;
  $('sTxD').className = (st.txDropped ?? 0) > 0 ? 'warn' : '';
  $('sLastErr').textContent = s.LastError || '—';
  $('sUp').textContent = st.uptimeS + ' s';
  $('sHb').textContent = new Date().toTimeString().slice(0, 8);
  $('dtr').checked = st.dtr; $('rts').checked = st.rts;
  const m = st.modem || {};
  $('ledCTS').classList.toggle('on', !!m.cts); $('ledDSR').classList.toggle('on', !!m.dsr);
  $('ledRI').classList.toggle('on', !!m.ri); $('ledDCD').classList.toggle('on', !!m.dcd);
  $('autoRecycle').checked = !!st.autoRecycle;

  // 'Connecting' is a distinct state from 'closed'. Without it, a CreateFile
  // against a re-enumerating USB adapter looks exactly like a port that was
  // never opened, and the operator clicks Open again instead of waiting.
  if (st.opening) { setHealth('h-warn', 'CONNECTING', 'opening the port — CreateFile in flight'); return; }
  if (!st.open) {
    // The big label must say what HAPPENED, not just that no port is
    // installed. A real cable pull showed 'CLOSED — port vanished', which
    // reads like the operator disconnected; the supervisor meanwhile was
    // actively retrying. 'CLOSED' is reserved for a port nobody is trying to
    // reopen, so the word and the behaviour agree.
    if (st.wedged) {
      setHealth('h-bad', 'WEDGED', 'handle leaked deliberately, not closed — open a fresh port');
    } else if (st.vanished) {
      setHealth('h-bad', 'VANISHED', (st.port || 'the port') + ' left the machine — retrying every 5 s');
    } else if (st.degraded) {
      setHealth('h-bad', 'RECYCLING', 'link degraded — closing and reopening ' + (st.port || ''));
    } else {
      setHealth('', 'CLOSED', 'open a port');
    }
    return;
  }
  if (st.healthy) setHealth('h-ok', 'HEALTHY', `${st.port} · watchdog advancing`);
  else if (st.vanished) setHealth('h-bad', 'VANISHED', st.port + ' is no longer on this machine');
  else if (st.degraded) setHealth('h-bad', 'DEGRADED', 'error streak — link not trusted');
  else setHealth('h-warn', 'OPEN · UNPROVEN', st.wdAdvancing ? 'waiting for heartbeat' : 'watchdog NOT advancing');
}

// ---------- events from Go ----------
function wire() {
  RT().EventsOn('log', (l) => {
    if (l.kind === 'rx') rxTotal += l.n; else if (l.kind === 'tx') txTotal += l.n;
    updCounters(); addLine(l);
  });
  RT().EventsOn('health', applyState);
  RT().EventsOn('state', applyState);
}

// ---------- theme: light <-> dark toggle + automatic text contrast ----------
// Independent of the Wails bridge so it also works when the page is opened in a plain browser.
// Surfaces come from style.css token sets (html[data-theme]); text colours are NOT
// trusted from CSS — after every switch we measure the real surface luminance and
// derive readable text/muted colours for the body and, separately, for the nav bar
// (which is deliberately the inverse of the theme).
const THEME_KEY = 'gsb.theme';

function parseColor(str) {                       // "#rgb" | "#rrggbb" | "rgb(a,b,c)" -> [r,g,b] 0..255
  str = (str || '').trim();
  let m;
  if ((m = str.match(/^#([0-9a-f]{3})$/i))) return [...m[1]].map(h => parseInt(h + h, 16));
  if ((m = str.match(/^#([0-9a-f]{6})$/i))) return [0, 2, 4].map(i => parseInt(m[1].slice(i, i + 2), 16));
  if ((m = str.match(/^rgba?\(([^)]+)\)$/i))) return m[1].split(',').slice(0, 3).map(Number);
  return [128, 128, 128];
}
function luminance([r, g, b]) {                  // WCAG relative luminance 0 (black) .. 1 (white)
  const f = (c) => { c /= 255; return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4); };
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
}
// Given a surface colour, return {text, muted} that read on it. Text ~ 12:1, muted ~ 4.5:1 contrast.
function contrastFor(surface) {
  const L = luminance(parseColor(surface));
  return L > 0.35
    ? { text: '#1b1f26', muted: '#5c6573' }        // light surface -> dark letters
    : { text: '#e6e9ee', muted: '#9aa3b2' };       // dark surface  -> light letters
}
function applyTheme(theme, save) {
  const root = document.documentElement;
  root.dataset.theme = theme;
  // measure the surfaces the CSS just selected, then set the text tokens to contrast with them
  const cs = getComputedStyle(root);
  const body = contrastFor(cs.getPropertyValue('--panels'));   // cards/toolbar carry most of the text
  const nav  = contrastFor(cs.getPropertyValue('--nav'));
  root.style.setProperty('--text', body.text);
  root.style.setProperty('--muted', body.muted);
  root.style.setProperty('--nav-text', nav.text);
  root.style.setProperty('--nav-muted', nav.muted);
  const b = $('btnTheme');
  if (b) b.textContent = theme === 'light' ? '☾ Dark' : '☀ Light';   // label = what you switch TO
  if (save) { try { localStorage.setItem(THEME_KEY, theme); } catch {} }
}
(function themeInit() {
  let t = 'light'; try { t = localStorage.getItem(THEME_KEY) || 'light'; } catch {}
  applyTheme(t, false);
  $('btnTheme').addEventListener('click', () => applyTheme(document.documentElement.dataset.theme === 'light' ? 'dark' : 'light', true));
  // drop the retired per-surface picker keys from the earlier View menu
  try { ['page', 'nav', 'mon', 'panels'].forEach(k => localStorage.removeItem('gsb.view.' + k)); } catch {}
})();

// ---------- boot ----------
(async function boot() {
  // restore last config
  try { const c = JSON.parse(localStorage.getItem('gsb.cfg') || 'null'); if (c) {
    for (const k of ['baud', 'dataBits', 'parity', 'stopBits', 'handshake', 'readGapMs', 'writeTimeoutMs']) if (c[k] != null) $(k).value = c[k];
    $('dtr').checked = !!c.dtr; $('rts').checked = !!c.rts;
  } } catch {}
  wire();
  // Restore the saved port THROUGH refreshPorts, not by assigning to the
  // <select> afterwards. Setting .value to an option that does not exist
  // silently leaves the control blank — no name, and not "(no COM ports)"
  // either. Passing it as `keep` makes an absent saved port appear as
  // "COM3 — not present", so the choice stays visible and it is clear what
  // Open would connect to.
  let savedPort = '';
  try {
    const c = JSON.parse(localStorage.getItem('gsb.cfg') || 'null');
    if (c && c.name) savedPort = c.name;
  } catch {}
  await refreshPorts({ keep: savedPort });
  applyState(await App().State());
  toast('ready — pick a port, Open, then use the second instance on the other end');
})();
