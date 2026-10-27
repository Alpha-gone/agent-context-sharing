'use strict';
// 합성 계정의 실제 브라우저 로그인·동의·CLI loopback callback만 수행한다.
// 자격·인가 URI·페이지 본문과 CDP 원문은 출력하지 않는다.
const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');
const config = JSON.parse(fs.readFileSync(0, 'utf8'));
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
let stage = 'launch', socket, chrome, cancelled = false;
process.on('SIGINT', () => { cancelled = true; });
const timer = setTimeout(() => { cancelled = true; }, 60000);

async function until(check) {
  const limit = Date.now() + 15000;
  while (Date.now() < limit && !cancelled) {
    const value = await check();
    if (value) return value;
    await pause(50);
  }
  throw new Error('stage timeout');
}

async function main() {
  chrome = spawn(config.chrome, [
    '--headless=new', '--remote-debugging-port=0', '--user-data-dir=' + config.profile,
    '--ignore-certificate-errors-spki-list=' + config.spki,
    '--no-first-run', '--no-default-browser-check', '--disable-extensions', 'about:blank',
  ], { stdio: 'ignore' });
  const portFile = path.join(config.profile, 'DevToolsActivePort');
  await until(() => fs.existsSync(portFile) && fs.readFileSync(portFile, 'utf8').includes('\n'));
  const port = fs.readFileSync(portFile, 'utf8').split('\n')[0];
  const version = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
  const page = await (await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: 'PUT' })).json();
  socket = new WebSocket(page.webSocketDebuggerUrl);
  let serial = 0;
  const pending = new Map();
  socket.onmessage = ({ data }) => {
    const message = JSON.parse(data);
    const call = pending.get(message.id);
    if (!call) return;
    pending.delete(message.id); clearTimeout(call.timer);
    if (message.error) call.reject(new Error('CDP request failed'));
    else call.resolve(message.result);
  };
  await new Promise((resolve, reject) => { socket.onopen = resolve; socket.onerror = reject; });
  function send(method, params = {}) {
    const id = ++serial;
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => { pending.delete(id); reject(new Error('CDP timeout')); }, 3000);
      pending.set(id, { resolve, reject, timer: timeout });
      socket.send(JSON.stringify({ id, method, params }));
    });
  }
  async function evaluate(expression) {
    const result = await send('Runtime.evaluate', { expression, returnByValue: true });
    if (result.exceptionDetails) throw new Error('page evaluation failed');
    return result.result.value;
  }
  async function click(selector) {
    const point = await evaluate(`(() => { const r = document.querySelector(${JSON.stringify(selector)}).getBoundingClientRect(); return {x:r.x+r.width/2,y:r.y+r.height/2}; })()`);
    await send('Input.dispatchMouseEvent', { type: 'mousePressed', ...point, button: 'left', clickCount: 1 });
    await send('Input.dispatchMouseEvent', { type: 'mouseReleased', ...point, button: 'left', clickCount: 1 });
  }
  await send('Page.enable'); await send('Runtime.enable');
  stage = 'login';
  await send('Page.navigate', { url: config.authorization });
  await until(() => evaluate('location.pathname === "/login" && !!document.querySelector("input[name=login_id]")'));
  for (const [selector, text] of [['input[name=login_id]', config.login], ['input[name=password]', config.password]]) {
    await evaluate(`document.querySelector(${JSON.stringify(selector)}).focus()`);
    await send('Input.insertText', { text });
  }
  await click('button[type=submit]');
  stage = 'consent';
  await until(() => evaluate('location.pathname === "/authorize" && !!document.querySelector("button[value=allow]")'));
  await click('button[value=allow]');
  stage = 'callback';
  await until(() => evaluate('location.origin === "http://127.0.0.1:49152" && location.pathname === "/callback"'));
  const callback = await evaluate('({issuer:new URL(location.href).searchParams.get("iss"),state:new URL(location.href).searchParams.get("state")})');
  if (callback.issuer !== config.origin || callback.state !== new URL(config.authorization).searchParams.get('state')) {
    throw new Error('callback binding failed');
  }
  console.log(JSON.stringify({ browser: version.Browser, login: true, consent: true, callback: true, issuer_match: true, state_match: true }));
}

(async () => {
  try { await main(); }
  catch { console.log(JSON.stringify({ failed_stage: stage })); process.exitCode = 1; }
  finally {
    clearTimeout(timer);
    socket?.close();
    if (chrome && chrome.exitCode === null) {
      const closed = new Promise((resolve) => chrome.once('close', resolve));
      chrome.kill('SIGTERM');
      await Promise.race([closed, pause(2000)]);
      if (chrome.exitCode === null) { chrome.kill('SIGKILL'); await closed; }
    }
  }
})();
