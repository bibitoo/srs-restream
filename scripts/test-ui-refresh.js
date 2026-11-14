// 前端刷新逻辑回归测试（无需浏览器）
//
// 背景：控制台每 5 秒自动刷新一次数据。如果每次刷新都用 innerHTML 重建 DOM，
// 正在播放的 <video> 会被销毁重建 —— 表现为"视频只能播 2 秒就断"。
//
// 本测试用最小 DOM stub 加载 control/web/index.html 里的脚本，验证：
//   1) 首次渲染会建立 DOM
//   2) 数据未变化时，重复刷新不重建 DOM（不打断播放/输入）
//   3) 有视频正在播放时，即使数据变化也不重建
//   4) 停止播放且数据变化时，正常重建
//   5) 设置页在用户输入（焦点在输入框）时不重建
//
// 用法：node scripts/test-ui-refresh.js
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const htmlPath = path.join(__dirname, '..', 'control', 'web', 'index.html');
const html = fs.readFileSync(htmlPath, 'utf8');
const m = html.match(/<script>([\s\S]*)<\/script>/);
if (!m) { console.error('未找到 <script> 块'); process.exit(1); }
const js = m[1];
const tmp = '/tmp/ui-under-test.js';
fs.writeFileSync(tmp, js);

// ---------------- 最小 DOM stub ----------------
const writes = {};
const bump = (id) => { writes[id] = (writes[id] || 0) + 1; };
const els = {};

function mkEl(id) {
  if (els[id]) return els[id];
  const el = {
    id, _html: '', textContent: '', style: {}, dataset: {}, className: '', tagName: 'DIV',
    set innerHTML(v) { this._html = v; bump(id); },
    get innerHTML() { return this._html; },
    contains() { return false; },
    addEventListener() {}, appendChild() {}, removeChild() {}, focus() {},
    // 模拟"已经渲染过"：只要 innerHTML 非空就认为存在子元素
    querySelector(sel) { return this._html ? {} : null; },
    querySelectorAll() { return []; },
  };
  els[id] = el;
  return el;
}

const state = {
  videoPlaying: false,
  recordings: [{ id: 'rec1', stream: 'live/stream', file: 'a.mp4', size: 217457467,
                 duration: 295.1, active: false, started_at: '2026-11-13T09:44:13+08:00' }],
  streams: [{ key: 'live/cam1', app: 'live', stream: 'cam1', active: true,
              video: 'H264 1280x720', kbps_recv: 1200, clients: 0 }],
  settings: { rtmp_base: 'rtmp://127.0.0.1:1935/', rtmp_public: 'rtmp://127.0.0.1:19351/', srs_http: 'http://127.0.0.1:8080' },
};

const sandbox = {
  console, Date, Math, JSON, Number, String, Array, Object, Set, isNaN, parseInt, parseFloat,
  encodeURIComponent, decodeURIComponent,
  // 前端用 localStorage 存访问密钥、用 cookie jsk 给 <video>/HLS 带凭据，两者都要 stub
  localStorage: {
    _d: {},
    getItem(k) { return Object.prototype.hasOwnProperty.call(this._d, k) ? this._d[k] : null; },
    setItem(k, v) { this._d[k] = String(v); },
    removeItem(k) { delete this._d[k]; },
  },
  setTimeout: () => 0, clearTimeout: () => {}, setInterval: () => 0, clearInterval: () => {},
  navigator: {},
  document: {
    hidden: false,
    cookie: '',
    activeElement: { tagName: 'BODY' },
    getElementById: mkEl,
    querySelectorAll: (sel) => {
      if (sel.indexOf('#rec-box video') === 0) {
        return state.videoPlaying ? [{ paused: false, ended: false, currentTime: 12, addEventListener() {} }] : [];
      }
      return [];
    },
    createElement: () => ({ style: {}, appendChild() {}, select() {}, value: '', remove() {}, setAttribute() {}, click() {} }),
    body: { appendChild() {}, removeChild() {} },
  },
  location: { origin: 'http://127.0.0.1:19861', hostname: '127.0.0.1', href: 'http://127.0.0.1:19861/' },
  fetch: async (url) => {
    const copy = (x) => JSON.parse(JSON.stringify(x));
    let data = {};
    if (url.indexOf('/api/recordings') === 0) data = { recordings: copy(state.recordings) };
    else if (url.indexOf('/api/streams') === 0) data = { streams: copy(state.streams) };
    else if (url.indexOf('/api/jobs') === 0) data = { jobs: [] };
    else if (url.indexOf('/api/targets') === 0) data = { targets: [] };
    else if (url.indexOf('/api/overview') === 0) data = { srs_version: '8.0.74', streams_active: 1, jobs_running: 0 };
    else if (url.indexOf('/api/settings') === 0) data = copy(state.settings);
    else if (url.indexOf('/api/layouts') === 0) data = { layouts: [] };
    return { ok: true, status: 200, json: async () => data };
  },
  alert: () => {}, confirm: () => true, prompt: () => '',
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;

vm.createContext(sandbox);
vm.runInContext(js, sandbox, { filename: tmp });

const realSleep = (ms) => new Promise((r) => global.setTimeout(r, ms));
let pass = 0, fail = 0;
function check(name, cond, extra) {
  if (cond) { pass++; console.log(`  ✅ ${name}`); }
  else { fail++; console.log(`  ❌ ${name}${extra ? ' — ' + extra : ''}`); }
}

(async () => {
  await realSleep(80);
  console.log('== 录像库 ==');
  const r1 = writes['rec-box'] || 0;
  check('首次渲染建立 DOM', r1 === 1, `重建次数=${r1}`);

  await sandbox.loadAll(false);
  await sandbox.loadAll(false);
  const r2 = writes['rec-box'] || 0;
  check('数据未变时 2 次刷新不重建（不打断播放）', r2 === r1, `重建次数=${r2}`);

  state.videoPlaying = true;
  state.recordings[0].duration = 300;
  await sandbox.loadAll(false);
  const r3 = writes['rec-box'] || 0;
  check('播放中即使数据变化也不重建', r3 === r2, `重建次数=${r3}`);

  state.videoPlaying = false;
  sandbox.recLastInteract = 0;
  state.recordings[0].duration = 301;
  await sandbox.loadAll(false);
  const r4 = writes['rec-box'] || 0;
  check('未播放且数据变化时正常更新', r4 > r3, `重建次数=${r4}`);

  console.log('== 直播流列表 ==');
  const s1 = writes['streams-box'] || 0;
  check('首次渲染建立表格', s1 >= 1, `重建次数=${s1}`);
  await sandbox.loadAll(false);
  const s2 = writes['streams-box'] || 0;
  check('数据未变时不重建表格', s2 === s1, `重建次数=${s2}`);
  state.streams[0].kbps_recv = 9999;
  await sandbox.loadAll(false);
  const s3 = writes['streams-box'] || 0;
  check('数据变化时更新表格', s3 > s2, `重建次数=${s3}`);

  console.log('== 设置页 ==');
  const g1 = writes['settings-box'] || 0;
  await sandbox.loadAll(false);
  await sandbox.loadAll(false);
  const g2 = writes['settings-box'] || 0;
  check('数据未变时不重建设置表单', g2 === g1, `重建次数=${g2}`);

  // 模拟焦点在设置区输入框内
  const box = els['settings-box'];
  box.contains = () => true;
  sandbox.document.activeElement = { tagName: 'INPUT' };
  state.settings.rtmp_public = 'rtmp://10.0.0.1:19351/';
  await sandbox.loadAll(false);
  const g3 = writes['settings-box'] || 0;
  check('用户正在输入时不重建（不清空输入内容）', g3 === g2, `重建次数=${g3}`);

  console.log(`\n通过 ${pass} 项，失败 ${fail} 项`);
  console.log(fail === 0 ? 'RESULT_OK' : 'RESULT_FAILED');
  process.exit(fail === 0 ? 0 : 1);
})();
