// Go 브라우저 인수 시험이 준비한 서버를 headless Chrome으로 조작한다.
// 외부 의존성 없이 Chrome DevTools Protocol의 실제 입력·네트워크·화면 에뮬레이션을 쓴다.
'use strict';
const assert = require('node:assert/strict');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');

const config = JSON.parse(process.env.WEB09_CONFIG);
const passed = [];
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
function pass(name, detail = '') {
  passed.push(name);
  console.log('PASS ' + name + (detail ? ' · ' + detail : ''));
}

class CDP {
  constructor(url) {
    this.socket = new WebSocket(url);
    this.next = 0;
    this.pending = new Map();
    this.listeners = new Set();
  }
  open() {
    return new Promise((resolve, reject) => {
      this.socket.onopen = resolve;
      this.socket.onerror = reject;
      this.socket.onmessage = (event) => {
        const message = JSON.parse(event.data);
        if (message.id && this.pending.has(message.id)) {
          const { resolve: done, reject: fail } = this.pending.get(message.id);
          this.pending.delete(message.id);
          if (message.error) fail(new Error(message.error.message));
          else done(message.result);
        } else if (message.method) {
          for (const listener of this.listeners) listener(message);
        }
      };
    });
  }
  send(method, params = {}) {
    const id = ++this.next;
    this.socket.send(JSON.stringify({ id, method, params }));
    return new Promise((resolve, reject) => this.pending.set(id, { resolve, reject }));
  }
  waitFor(method, predicate = () => true, timeout = 15000) {
    return new Promise((resolve, reject) => {
      const listener = (message) => {
        if (message.method === method && predicate(message.params)) {
          finish();
          resolve(message.params);
        }
      };
      const timer = setTimeout(() => {
        finish();
        reject(new Error('대기 시간 초과: ' + method));
      }, timeout);
      const finish = () => {
        clearTimeout(timer);
        this.listeners.delete(listener);
      };
      this.listeners.add(listener);
    });
  }
}

let cdp;
const network = { requests: [], responses: [], extra: [], raw: [] };

async function evaluate(expression) {
  const result = await cdp.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
  if (result.exceptionDetails) {
    throw new Error('페이지 평가 실패: ' + (result.exceptionDetails.exception?.description || result.exceptionDetails.text));
  }
  return result.result.value;
}
async function go(url) {
  const loaded = cdp.waitFor('Page.loadEventFired');
  await cdp.send('Page.navigate', { url });
  await loaded;
}
async function currentPath() {
  return evaluate('location.pathname + location.search');
}
// mark는 조건에 맞는 요소 하나에 시험 표식을 달고 그 선택자를 돌려준다.
let marks = 0;
async function mark(selector, text) {
  const id = String(++marks);
  await evaluate(`(() => {
    const matches = [...document.querySelectorAll(${JSON.stringify(selector)})].filter((element) => ${text === undefined ? 'true' : `element.textContent.trim() === ${JSON.stringify(text)}`});
    if (matches.length !== 1) throw new Error(${JSON.stringify(selector + ' ' + (text || ''))} + ' 대상 수 ' + matches.length);
    matches[0].setAttribute('data-web09', ${JSON.stringify(id)});
  })()`);
  return `[data-web09="${id}"]`;
}
async function point(selector) {
  return evaluate(`(() => {
    const element = document.querySelector(${JSON.stringify(selector)});
    element.scrollIntoView({ block: 'center', inline: 'center' });
    const rect = element.getBoundingClientRect();
    return { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 };
  })()`);
}
async function mouse(x, y) {
  await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y });
  await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: 'left', clickCount: 1 });
  await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: 'left', clickCount: 1 });
}
async function click(selector, navigate = false) {
  const target = await point(selector);
  const loaded = navigate ? cdp.waitFor('Page.loadEventFired') : null;
  await mouse(target.x, target.y);
  if (loaded) await loaded;
  else await sleep(50);
}
async function type(selector, text) {
  await click(selector);
  await evaluate(`document.querySelector(${JSON.stringify(selector)}).select?.()`);
  await cdp.send('Input.insertText', { text });
}
const keys = {
  Enter: { key: 'Enter', code: 'Enter', windowsVirtualKeyCode: 13, text: '\r' },
  Space: { key: ' ', code: 'Space', windowsVirtualKeyCode: 32, text: ' ' },
  Escape: { key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 },
  Tab: { key: 'Tab', code: 'Tab', windowsVirtualKeyCode: 9 },
  ArrowLeft: { key: 'ArrowLeft', code: 'ArrowLeft', windowsVirtualKeyCode: 37 },
  ArrowRight: { key: 'ArrowRight', code: 'ArrowRight', windowsVirtualKeyCode: 39 },
};
async function press(name, navigate = false, modifiers = 0) {
  const key = keys[name];
  const loaded = navigate ? cdp.waitFor('Page.loadEventFired') : null;
  await cdp.send('Input.dispatchKeyEvent', { type: key.text ? 'keyDown' : 'rawKeyDown', modifiers, ...key });
  await cdp.send('Input.dispatchKeyEvent', { type: 'keyUp', modifiers, ...key });
  if (loaded) await loaded;
  else await sleep(50);
}
async function screenshot(name) {
  const image = await cdp.send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false });
  fs.writeFileSync(path.join(config.evidence, name + '.png'), Buffer.from(image.data, 'base64'));
}
// pageChecks는 모든 화면에 공통인 한국어 문서·민감정보 비노출을 확인한다.
async function pageChecks(name) {
  const page = await evaluate(`({ lang: document.documentElement.lang, title: document.title, html: document.documentElement.outerHTML, cookie: document.cookie })`);
  assert.equal(page.lang, 'ko', name + ' 문서 언어');
  assert.match(page.title, / · 에이전트 컨텍스트$/, name + ' 제목');
  for (const secret of [config.password, config.sessionMarker].filter(Boolean)) {
    assert.ok(!page.html.includes(secret), name + ' 화면에 민감정보가 있다');
  }
  assert.ok(!page.cookie.includes('agent_context_session'), name + ' 세션 쿠키가 스크립트에 보인다');
}
async function rows(selector) {
  return evaluate(`[...document.querySelectorAll(${JSON.stringify(selector)})].map((element) => element.textContent.trim())`);
}
async function scope() {
  return evaluate(`(() => {
    const cy = document.getElementById('context-graph')._cyreg.cy;
    const active = document.activeElement;
    return {
      status: document.getElementById('scope-status').textContent,
      visible: cy.nodes(':visible').map((node) => node.data('body')).sort(),
      nodes: cy.nodes().map((node) => node.data('body')).sort(),
      evidenceEdges: cy.edges('.evidence').length,
      pressed: [...document.querySelectorAll('[data-context-id]')].filter((button) => button.getAttribute('aria-pressed') === 'true').map((button) => button.dataset.contextId),
      active: active.dataset.contextId || active.id || active.tagName,
      details: document.getElementById('node-details').textContent,
      evidence: document.getElementById('node-evidence').textContent,
      range: document.getElementById('hop-range').value,
      valuetext: document.getElementById('hop-range').getAttribute('aria-valuetext'),
    };
  })()`);
}
async function nodePoint(body) {
  return evaluate(`(() => {
    const container = document.getElementById('context-graph');
    container.scrollIntoView({ block: 'center' });
    const cy = container._cyreg.cy;
    const node = cy.nodes().filter((candidate) => candidate.data('body') === ${JSON.stringify(body)});
    if (node.length !== 1) throw new Error('노드 ' + ${JSON.stringify(body)});
    const rect = container.getBoundingClientRect();
    const position = node.renderedPosition();
    return { x: rect.left + position.x, y: rect.top + position.y };
  })()`);
}

// 접근성 트리는 브라우저가 노출한 의미를 검사한다. 실제 스크린리더 음성과 구분한다.
async function accessibility(name) {
  const { nodes } = await cdp.send('Accessibility.getFullAXTree');
  fs.writeFileSync(path.join(config.evidence, 'ax-' + name + '.json'), JSON.stringify(nodes, null, 2));
  return nodes.filter((node) => !node.ignored);
}
async function axElement(selector, nodes) {
  const { root } = await cdp.send('DOM.getDocument');
  const { nodeId } = await cdp.send('DOM.querySelector', { nodeId: root.nodeId, selector });
  assert.ok(nodeId, '접근성 검사 대상: ' + selector);
  const { node } = await cdp.send('DOM.describeNode', { nodeId });
  const result = nodes.find((candidate) => candidate.backendDOMNodeId === node.backendNodeId);
  assert.ok(result, '접근성 트리에서 노출된 대상: ' + selector);
  return result;
}
function axProperty(node, name) {
  return node.properties?.find((property) => property.name === name)?.value.value;
}
async function axText(selector, nodes) {
  const root = await axElement(selector, nodes);
  function text(node) {
    if (node.role.value === 'StaticText') return node.name.value;
    return (node.childIds || []).map((id) => nodes.find((child) => child.nodeId === id)).filter(Boolean).map(text).join('\n');
  }
  return text(root);
}

async function keyboardOrder() {
  const controls = await evaluate(`(() => {
    return [...document.querySelectorAll('a[href], button, input, select, textarea, [tabindex]')]
      .filter((element) => !element.disabled && element.tabIndex >= 0 && element.getClientRects().length)
      .map((element, index) => { element.dataset.test06Focus = String(index); return { index, label: element.getAttribute('aria-label') || element.textContent.trim() || element.id }; });
  })()`);
  assert.ok(controls.length >= 10, '상세 화면의 모든 조작 수단');
  await evaluate(`document.querySelector('[data-test06-focus="0"]').focus()`);
  for (let index = 1; index < controls.length; index++) {
    await press('Tab');
    assert.equal(await evaluate('document.activeElement.dataset.test06Focus'), String(index), 'Tab: ' + controls[index].label);
    assert.ok(await evaluate(`document.activeElement.matches(':focus-visible')`), 'Tab 포커스 표시');
  }
  for (let index = controls.length - 2; index >= 0; index--) {
    await press('Tab', false, 8); // CDP modifiers의 Shift 비트.
    assert.equal(await evaluate('document.activeElement.dataset.test06Focus'), String(index), 'Shift+Tab: ' + controls[index].label);
    assert.ok(await evaluate(`document.activeElement.matches(':focus-visible')`), 'Shift+Tab 포커스 표시');
  }
  fs.writeFileSync(path.join(config.evidence, 'keyboard-order.json'), JSON.stringify(controls, null, 2));
  pass('TEST-06 상세 전체 Tab·Shift+Tab 순서', controls.length + '개 조작 수단, 양방향 포커스 유지');
}

async function keyboardAccessibility() {
  await keyboardOrder();
  const isolated = '[data-context-id="' + config.contexts.isolated + '"]';
  await evaluate(`document.querySelector(${JSON.stringify(isolated)}).focus()`);
  await press('Enter');
  assert.deepEqual((await scope()).visible, ['고립 원천']);
  await press('Escape');
  assert.equal((await scope()).active, config.contexts.isolated);

  const descendant = '[data-context-id="' + config.contexts.descendant + '"]';
  await evaluate(`document.querySelector(${JSON.stringify(descendant)}).focus()`);
  const initial = await accessibility('unselected');
  const initialButton = await axElement(descendant, initial);
  assert.equal(initialButton.role.value, 'button');
  assert.equal(axProperty(initialButton, 'pressed'), 'false');
  await press('Enter');
  const selected = await accessibility('selected');
  const button = await axElement(descendant, selected);
  assert.equal(button.name.value, initialButton.name.value, '선택 뒤에도 버튼 이름 유지');
  assert.equal(axProperty(button, 'pressed'), 'true');
  assert.equal(axProperty(button, 'focused'), true);
  const status = await axElement('#scope-status', selected);
  assert.equal(status.role.value, 'status');
  assert.equal(axProperty(status, 'live'), 'polite');
  assert.equal(axProperty(status, 'atomic'), true);
  const state = await scope();
  assert.match(state.status, /^국소 보기/);
  assert.ok(!state.status.includes('파생 후손') && !state.status.includes('derived_from'), '짧은 상태 안내에 본문·근거를 넣지 않는다');
  assert.ok(selected.some((node) => node.role.value === 'StaticText' && node.name?.value === state.status), '변경 상태가 접근성 트리에 노출된다');

  await evaluate(`document.getElementById('hop-range').focus()`);
  const maximum = await evaluate(`Number(document.getElementById('hop-range').max)`);
  assert.equal(maximum, 4, '다중 시작점 5개의 슬라이더 상한은 n-1');
  for (let index = 0; index < maximum; index++) await press('ArrowLeft');
  const zero = await accessibility('zero-hop');
  const slider = await axElement('#hop-range', zero);
  assert.equal(slider.role.value, 'slider');
  assert.equal(slider.value.value, 0);
  assert.equal(axProperty(slider, 'valuemin'), 0);
  assert.equal(axProperty(slider, 'valuemax'), maximum);
  const domValueText = (await scope()).valuetext;
  assert.equal(domValueText, '0홉');
  // Chromium의 CDP 직렬화는 valuetext에 native kValue(숫자 문자열)를 넣기도 한다.
  // DOM의 단위 안내와 AX의 수치를 각각 검증하고 실제 음성 안내로 간주하지 않는다.
  const axValueText = axProperty(slider, 'valuetext');
  assert.ok([domValueText, String(slider.value.value)].includes(axValueText), 'DOM/AX 슬라이더 값');
  fs.writeFileSync(path.join(config.evidence, 'range-value-text.json'), JSON.stringify({ dom: domValueText, ax: axValueText }));
  assert.deepEqual((await scope()).visible, ['파생 후손']);
  const evidence = (await scope()).evidence;
  const accessibleEvidence = await axText('#node-evidence', zero);
  for (const [from, to] of [['descendant', 'derived'], ['derived', 'source']]) {
    const connection = '파생 ' + config.contexts[from] + ' → 근거 ' + config.contexts[to] + ' (derived_from)';
    assert.ok(evidence.includes(connection), 'DOM 근거 방향: ' + connection);
    assert.ok(accessibleEvidence.includes(connection), '숨긴 근거의 텍스트가 접근성 트리에 남는다: ' + connection);
  }
  for (let index = 0; index < maximum; index++) await press('ArrowRight');
  assert.equal((await scope()).range, String(maximum));
  assert.equal((await scope()).visible.length, 4, '최대 홉도 고립 노드는 포함하지 않는다');

  await evaluate(`document.getElementById('details-link').focus()`);
  await press('Enter');
  const details = await accessibility('details');
  const heading = await axElement('#node-details-heading', details);
  assert.equal(heading.role.value, 'heading');
  assert.equal(axProperty(heading, 'level'), 2);
  assert.equal(axProperty(heading, 'focused'), true);
  assert.match(await axText('#node-details', details), /파생 후손/, '선택 상세 본문 읽기 대상 노출');
  assert.equal((await axElement('#context-graph', details)).role.value, 'image');
  await evaluate(`document.querySelector(${JSON.stringify(descendant)}).focus()`);
  await press('Space');
  const cleared = await accessibility('cleared');
  assert.equal(axProperty(await axElement(descendant, cleared), 'pressed'), 'false');
  assert.equal((await scope()).status, '전체 보기');
  assert.ok(!(await scope()).evidence.includes('derived_from'), '선택 해제 뒤 근거 초기화');
  pass('TEST-06 접근성 트리·키보드', 'pressed·polite/atomic·슬라이더 0/최대·제목 포커스·본문/근거·해제');
}

async function graphVariants() {
  for (const [name, graph, expected] of [
    ['empty', config.emptyGraph, []], ['single', config.singleGraph, ['단일 원천']],
  ]) {
    await go(config.base + '/graphs/' + graph);
    await pageChecks(name);
    let state = await scope();
    assert.deepEqual(state.nodes, expected);
    assert.equal(state.range, '0');
    assert.equal(await evaluate(`document.getElementById('hop-range').max`), '0');
    if (expected.length) {
      await evaluate(`document.querySelector('[data-context-id]').focus()`);
      await press('Enter');
      state = await scope();
      assert.deepEqual(state.visible, expected);
      assert.deepEqual(state.pressed, [config.singleContext]);
      await press('Escape');
      assert.equal((await scope()).active, config.singleContext);
    } else {
      assert.match(await evaluate('document.body.textContent'), /표시할 활성 컨텍스트가 없습니다/);
    }
    await click('#full-view');
    await press('Escape');
    state = await scope();
    assert.equal(state.status, '전체 보기');
    assert.deepEqual(state.pressed, []);
    assert.deepEqual(state.visible, expected);
    await accessibility(name);
    pass('TEST-06 ' + name + ' 실제 그래프', '0홉·선택/복귀·빈 상태');
  }
}

async function invalidCursor() {
  const response = cdp.waitFor('Network.responseReceived', (params) => params.type === 'Document' && params.response.url.includes('cursor=broken'));
  await go(config.base + '/graphs?name=' + encodeURIComponent('목록') + '&cursor=broken');
  assert.equal((await response).response.status, 400);
  await pageChecks('잘못된 커서');
  assert.equal(await evaluate(`document.getElementById('graph-name').value`), '목록');
  assert.match(await evaluate('document.body.textContent'), /목록 커서가 올바르지 않습니다/);
  pass('TEST-06 잘못된 커서의 실제 오류 화면', 'HTTP 400·이름 필터 유지');
}

async function login() {
  await go(config.base + '/graphs');
  assert.equal(await currentPath(), '/login', '세션 없는 업무 화면은 로그인으로 이동');
  await pageChecks('로그인');
  await type('#login-id', config.login);
  await type('#login-password', config.password);
  await press('Enter', true);
  assert.equal(await currentPath(), '/graphs');
  const { cookies } = await cdp.send('Network.getCookies', { urls: [config.base] });
  const session = cookies.find((cookie) => cookie.name === 'agent_context_session');
  assert.ok(session && session.httpOnly && session.secure && session.sameSite === 'Lax', '세션 쿠키 속성');
  config.sessionMarker = session.value;
  await pageChecks('그래프 목록');
  pass('로그인·세션 쿠키', 'HttpOnly·Secure·SameSite=Lax, 스크립트 비노출');
}

async function graphList() {
  network.requests = [];
  await type('#graph-name', '목록');
  await click(await mark('button', '적용'), true);
  let path = await currentPath();
  assert.match(path, /^\/graphs\?name=%EB%AA%A9%EB%A1%9D$/);
  let names = await rows('table tbody tr td:first-child');
  assert.equal(names.length, 2, '첫 페이지 크기');
  await click(await mark('a', '더 보기'), true);
  path = await currentPath();
  assert.ok(path.includes('name=%EB%AA%A9%EB%A1%9D') && path.includes('cursor='), '더 보기의 필터·커서 유지');
  names = names.concat(await rows('table tbody tr td:first-child'));
  assert.deepEqual(names.sort(), config.listGraphs.slice().sort());
  await go(config.base + '/graphs');
  await click('#grade-viewer');
  await click(await mark('button', '적용'), true);
  path = await currentPath();
  assert.equal(path, '/graphs?name=&grade=viewer');
  assert.deepEqual(await rows('table tbody tr td:first-child'), [config.viewerGraph]);
  assert.equal(await evaluate(`document.querySelector('input[name="grade"][value="viewer"]').checked`), true, '선택한 등급 유지');
  assert.ok(!network.requests.some((url) => url.includes('cytoscape')), '목록 화면이 시각화 자산을 읽었다');
  pass('그래프 목록 필터·페이지', '이름 3건 2쪽, 열람자 1건, 시각화 자산 미적재');
}

async function detail() {
  network.responses = [];
  network.extra = [];
  await go(config.base + '/graphs/' + config.detailGraph);
  await pageChecks('그래프 상세');
  let state = await scope();
  assert.equal(state.status, '전체 보기');
  assert.deepEqual(state.visible, state.nodes);
  assert.equal(state.nodes.length, 5);
  assert.equal(await evaluate(`document.querySelectorAll('.graph-legend li').length`), 8, '계층·상태·연결 범례');
  await screenshot('detail-desktop');

  const isolated = await nodePoint('고립 원천');
  await mouse(isolated.x, isolated.y);
  await sleep(100);
  state = await scope();
  assert.match(state.status, /^국소 보기/);
  assert.deepEqual(state.visible, ['고립 원천']);
  assert.deepEqual(state.pressed, [config.contexts.isolated]);
  const box = await evaluate(`(() => { const rect = document.getElementById('context-graph').getBoundingClientRect(); return { x: rect.left + 8, y: rect.top + 8 }; })()`);
  await mouse(box.x, box.y);
  await sleep(100);
  state = await scope();
  assert.equal(state.status, '전체 보기', '빈 영역 선택은 전체 보기로 돌아간다');
  assert.deepEqual(state.pressed, []);
  pass('포인터 노드 선택·빈 영역 복귀');

  const derived = `[data-context-id="${config.contexts.derived}"]`;
  await evaluate(`document.querySelector(${JSON.stringify(derived)}).focus()`);
  await press('Enter');
  state = await scope();
  assert.deepEqual(state.pressed, [config.contexts.derived]);
  assert.equal(state.active, config.contexts.derived, 'Enter 선택 뒤 포커스 유지');
  assert.match(state.details, /선택 파생/);
  await press('Space');
  state = await scope();
  assert.deepEqual(state.pressed, [], 'Space 재선택은 해제한다');
  assert.equal(state.status, '전체 보기');
  assert.equal(state.active, config.contexts.derived, 'Space 해제 뒤 포커스 유지');
  await press('Enter');
  await press('Escape');
  state = await scope();
  assert.equal(state.status, '전체 보기', 'Esc는 전체 보기로 돌아간다');
  assert.equal(state.active, config.contexts.derived);
  pass('키보드 Enter·Space·Esc 선택과 포커스 유지');

  const descendant = `[data-context-id="${config.contexts.descendant}"]`;
  await evaluate(`document.querySelector(${JSON.stringify(descendant)}).focus()`);
  await press('Enter');
  state = await scope();
  assert.equal(state.evidenceEdges, 2);
  assert.equal(state.evidence.split('\n').length, 2, '텍스트 근거 두 단계');
  const before = Number(state.range);
  await evaluate(`document.getElementById('hop-range').focus()`);
  for (let i = 0; i < before; i++) await press('ArrowLeft');
  state = await scope();
  assert.equal(state.range, '0');
  assert.equal(state.valuetext, '0홉');
  assert.deepEqual(state.visible, ['파생 후손']);
  assert.equal(state.evidence.split('\n').length, 2, '숨겨진 근거도 텍스트에 남는다');
  assert.match(state.status, / · 0홉 · /);
  await click(`[data-context-id="${config.contexts.event}"]`);
  state = await scope();
  assert.deepEqual(state.pressed, [config.contexts.event], '국소 범위 밖 노드의 목록 재선택');
  assert.deepEqual(state.visible, ['사건 노드']);
  await evaluate(`document.getElementById('details-link').focus()`);
  await press('Enter');
  state = await scope();
  assert.equal(state.active, 'node-details-heading', '상세 이동 링크는 제목으로 포커스를 옮긴다');
  await click('#full-view');
  state = await scope();
  assert.equal(state.status, '전체 보기');
  assert.equal(state.range, '0', '전체 보기 복귀는 홉 값을 유지한다');
  assert.deepEqual(state.visible, state.nodes);
  pass('방향키 홉·숨겨진 근거·범위 밖 재선택·상세 이동·전체 보기 버튼');

  await evaluate(`document.getElementById('full-view').focus()`);
  await press('Tab');
  const focus = await evaluate(`(() => { const element = document.activeElement; const style = getComputedStyle(element); return { id: element.id, visible: element.matches(':focus-visible'), outline: style.outlineStyle, width: style.outlineWidth }; })()`);
  assert.equal(focus.id, 'details-link');
  assert.ok(focus.visible && focus.outline !== 'none' && focus.width !== '0px', '키보드 포커스 표시');
  pass('Tab 이동의 보이는 포커스', focus.outline + ' ' + focus.width);

  const asset = network.responses.find((response) => response.url.endsWith('/assets/cytoscape.min.js'));
  assert.ok(asset && asset.status === 200 && asset.etag, '최초 시각화 자산 응답');
  network.responses = [];
  network.extra = [];
  network.raw = [];
  const status = await evaluate(`fetch('/assets/cytoscape.min.js', { cache: 'no-cache' }).then((response) => response.status)`);
  await sleep(200);
  const revalidated = network.responses.find((response) => response.url.endsWith('/assets/cytoscape.min.js'));
  const conditional = network.extra.find((headers) => headers['if-none-match'] === asset.etag);
  assert.equal(status, 200, '재검증 결과는 캐시 본문으로 제공된다');
  // Chrome은 재검증한 응답을 캐시 본문의 200으로 보고하므로 네트워크 원시 상태로 304를 대조한다.
  assert.ok(revalidated && conditional && network.raw.includes(304), '자산 조건부 재검증 ' + JSON.stringify({ revalidated, conditional: Boolean(conditional), raw: network.raw }));
  pass('시각화 자산 ETag 재검증', 'If-None-Match → 304');
}

async function teams() {
  await go(config.base + '/access');
  await pageChecks('팀 관리');
  await type('#new-team-name', config.teamName);
  await click(await mark('button', '팀 생성'), true);
  assert.equal(await currentPath(), '/access');
  const team = await evaluate(`(() => {
    const heading = [...document.querySelectorAll('article h3')].find((element) => element.textContent.trim() === ${JSON.stringify(config.teamName)});
    if (!heading) throw new Error('새 팀이 없다');
    return heading.closest('article').querySelector('input[name="team_id"]').value;
  })()`);
  await type('#add-member-' + team, config.memberLogin);
  await click(await mark('button', config.teamName + ' 구성원 추가'), true);
  const members = await evaluate(`[...document.querySelector('#add-member-${team}').closest('article').querySelectorAll('li')].map((item) => item.textContent.trim())`);
  assert.ok(members.includes(config.memberLogin), '추가한 구성원이 표시된다');
  pass('팀 생성·구성원 추가', team);
  return team;
}

async function deletion() {
  const base = config.base + '/graphs/' + config.detailGraph + '/deletion';
  await go(base);
  await pageChecks('삭제와 복구');
  await click(`a[href$="context_id=${config.contexts.event}"]`, true);
  assert.match(await evaluate(`document.body.textContent`), /이 사건에 붙은 확정 관계 \d+개/);
  await click(await mark('button', '이 노드 소프트 삭제'), true);
  assert.equal(await currentPath(), '/graphs/' + config.detailGraph + '/deletion');
  await go(config.base + '/graphs/' + config.detailGraph);
  assert.ok(!(await scope()).nodes.includes('사건 노드'), '삭제한 노드는 상세에 보이지 않는다');
  await go(base);
  await click('nav[aria-label="삭제와 복구 보기"] a[href$="?tab=restore"]', true);
  assert.equal(await evaluate(`document.querySelector('a[aria-current="page"]').textContent`), '복구 대상');
  assert.ok((await evaluate('document.body.textContent')).includes(config.contexts.event));
  await click(`form:has(input[name="context_id"][value="${config.contexts.event}"]) button`, true);
  assert.equal(await currentPath(), '/graphs/' + config.detailGraph + '/deletion?tab=restore');
  assert.ok(!(await evaluate('document.body.textContent')).includes(config.contexts.event), '복구한 노드가 목록에서 빠진다');
  pass('노드 삭제·복구 대상 보기·노드 복구');

  await go(config.base + '/graphs/' + config.deleteGraph.id + '/deletion');
  await type('#graph-name-confirm', config.deleteGraph.name);
  await click(await mark('button', '그래프 소프트 삭제'), true);
  assert.equal(await currentPath(), '/graphs');
  await click(await mark('a', config.deleteGraph.name + ' 복구'), true);
  assert.match(await evaluate('document.body.textContent'), /삭제된 그래프입니다/);
  await click('nav[aria-label="삭제와 복구 보기"] a[href$="?tab=restore"]', true);
  await click(await mark('button', '그래프 복구'), true);
  assert.equal(await currentPath(), '/graphs');
  pass('그래프 이름 확인 삭제·목록 진입·그래프 복구');
}

async function audit() {
  const base = config.base + '/graphs/' + config.detailGraph + '/audit';
  await go(base);
  await pageChecks('감사 기록');
  assert.equal((await rows('table tbody tr')).length, 2, '감사 페이지 크기');
  await click(await mark('a', '더 보기'), true);
  assert.ok((await currentPath()).includes('cursor='));
  await go(base);
  await type('#audit-target', config.contexts.event);
  await click(await mark('button', '필터 적용'), true);
  const path = await currentPath();
  assert.ok(path.includes('target=' + config.contexts.event) && !path.includes('cursor='), '새 필터는 커서를 싣지 않는다');
  const targets = await rows('table tbody tr td:nth-child(6)');
  assert.ok(targets.length > 0 && targets.every((value) => value === config.contexts.event), '대상 필터 결과');
  pass('감사 더 보기·대상 필터', targets.length + '행');
}

async function connections() {
  await go(config.base + '/connections');
  await pageChecks('연결 관리');
  assert.ok((await evaluate('document.body.textContent')).includes(config.agent));
  await click(`form:has(input[name="target_id"][value="${config.agent}"]) button`, true);
  assert.equal(await currentPath(), '/connections');
  const text = await evaluate('document.body.textContent');
  assert.ok(!text.includes(config.agent) && text.includes('활성 에이전트 등록이 없습니다.'));
  pass('본인 에이전트 등록 폐기');
}

async function crossOrigin() {
  const rejected = cdp.waitFor('Network.responseReceived', (params) => params.type === 'Document' && params.response.url.startsWith(config.base));
  const loaded = cdp.waitFor('Page.loadEventFired', () => true, 15000);
  await cdp.send('Page.navigate', { url: config.attacker });
  const { response } = await rejected;
  await loaded;
  await sleep(200);
  assert.equal(response.status, 403, '교차 출처 POST 거부');
  assert.match(await evaluate('document.body.textContent'), /다른 출처에서 보낸 요청은 처리할 수 없습니다/);
  pass('같은 사이트 다른 출처의 상태 변경 거부', 'Sec-Fetch-Site → 403');
}

async function smallScreens() {
  await cdp.send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
  const pages = {
    graphs: '/graphs', detail: '/graphs/' + config.detailGraph, deletion: '/graphs/' + config.detailGraph + '/deletion',
    access: '/graphs/' + config.detailGraph + '/access', audit: '/graphs/' + config.detailGraph + '/audit', connections: '/connections', operator: '/operator/restores',
  };
  const widths = [];
  for (const [name, target] of Object.entries(pages)) {
    await go(config.base + target);
    await pageChecks(name);
    const size = await evaluate(`({ scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth })`);
    assert.ok(size.scroll <= size.client, name + ' 작은 화면 가로 넘침 ' + size.scroll + '>' + size.client);
    widths.push(name + ' ' + size.scroll + '/' + size.client);
    const caption = { audit: 'audit-caption', access: 'grants-caption' }[name];
    if (caption) {
      // 넓은 표는 페이지가 아니라 키보드로 스크롤할 수 있는 표 영역 안에서만 넘친다.
      const table = await evaluate(`(() => { const region = document.querySelector('.table-scroll'); region.focus(); return { scroll: region.scrollWidth, client: region.clientWidth, focused: document.activeElement === region, name: region.getAttribute('aria-labelledby') }; })()`);
      assert.ok(table.scroll >= 720 && table.scroll > table.client && table.focused && table.name === caption, name + ' 표 스크롤 영역 ' + JSON.stringify(table));
      await press('ArrowRight');
      assert.ok(await evaluate(`document.querySelector('.table-scroll').scrollLeft > 0`), '방향키로 표 영역을 가로 스크롤한다');
    }
    await screenshot('mobile-' + name);
  }
  await go(config.base + pages.detail);
  const layout = await evaluate(`(() => { const [left, graph, right] = document.querySelectorAll('.graph-layout > *'); const a = left.getBoundingClientRect(), b = graph.getBoundingClientRect(), c = right.getBoundingClientRect(); return a.bottom <= b.top + 1 && b.bottom <= c.top + 1; })()`);
  assert.ok(layout, '작은 화면에서 세 영역을 한 열로 쌓는다');
  await cdp.send('Emulation.clearDeviceMetricsOverride');
  pass('390px 작은 화면 가로 넘침 없음·넓은 표 스크롤 영역·상세 한 열 배치', widths.join(', '));
}

async function logout() {
  await go(config.base + '/logout');
  await click(await mark('button', '로그아웃'), true);
  assert.equal(await currentPath(), '/login');
  await go(config.base + '/graphs');
  assert.equal(await currentPath(), '/login', '로그아웃 뒤 업무 화면 차단');
  pass('로그아웃 뒤 세션 차단');
}

async function main() {
  const chrome = spawn(config.chrome, [
    '--headless=new', '--remote-debugging-port=0', '--user-data-dir=' + config.profile, '--ignore-certificate-errors-spki-list=' + config.spki,
    '--no-first-run', '--no-default-browser-check', '--disable-extensions', '--window-size=1280,900', 'about:blank',
  ], { stdio: 'ignore' });
  try {
    const portFile = path.join(config.profile, 'DevToolsActivePort');
    for (let i = 0; i < 300 && !(fs.existsSync(portFile) && fs.readFileSync(portFile, 'utf8').includes('\n')); i++) await sleep(50);
    const port = fs.readFileSync(portFile, 'utf8').split('\n')[0];
    const version = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
    const target = await (await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: 'PUT' })).json();
    cdp = new CDP(target.webSocketDebuggerUrl);
    await cdp.open();
    cdp.listeners.add((message) => {
      if (message.method === 'Network.requestWillBeSent') network.requests.push(message.params.request.url);
      if (message.method === 'Network.responseReceived') {
        const { response, type } = message.params;
        const etag = Object.entries(response.headers).find(([key]) => key.toLowerCase() === 'etag');
        network.responses.push({ url: response.url, status: response.status, type, etag: etag && etag[1] });
      }
      if (message.method === 'Network.responseReceivedExtraInfo') network.raw.push(message.params.statusCode);
      if (message.method === 'Network.requestWillBeSentExtraInfo') {
        network.extra.push(Object.fromEntries(Object.entries(message.params.headers).map(([key, value]) => [key.toLowerCase(), value])));
      }
    });
    await cdp.send('Page.enable');
    await cdp.send('Network.enable');
    await cdp.send('Runtime.enable');
    await cdp.send('Accessibility.enable');
    console.log('BROWSER ' + version.Browser + ' · ' + version['User-Agent']);
    await login();
    await graphList();
    await detail();
    await keyboardAccessibility();
    await graphVariants();
    await invalidCursor();
    await teams();
    await deletion();
    await audit();
    await connections();
    await crossOrigin();
    await smallScreens();
    await logout();
    console.log('WEB09 ' + JSON.stringify({ browser: version.Browser, passed }));
    const result = { browser: version.Browser, node: process.version, os: os.platform() + ' ' + os.release(), passed: passed.filter((name) => name.startsWith('TEST-06')), screenReader: '미실행', safari: '미실행' };
    fs.writeFileSync(path.join(config.evidence, 'test06.json'), JSON.stringify(result, null, 2));
    console.log('TEST06 ' + JSON.stringify(result));
  } finally {
    chrome.kill('SIGTERM');
    await new Promise((resolve) => chrome.once('exit', resolve));
  }
}

main().then(() => process.exit(0), (error) => {
  console.error('FAIL ' + (error.stack || error));
  process.exit(1);
});
