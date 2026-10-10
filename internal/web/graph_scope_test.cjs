// Go HTTP 시험이 렌더링한 HTML을 받아 저장소의 Cytoscape 자산으로 실행한다.
// DOM 이벤트만 대체하고, 홉 확장·표시·근거 경로 계산은 실제 라이브러리를 쓴다.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const cytoscape = require('./assets/cytoscape.min.js');

const html = fs.readFileSync(0, 'utf8');
const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
assert.equal(scripts.length, 1, '시각화 스크립트가 하나여야 한다');

const controls = new Map();
function control(id, textContent = '') {
  const element = new EventTarget();
  element.textContent = textContent;
  const attributes = new Map();
  element.setAttribute = (name, value) => attributes.set(name, value);
  element.getAttribute = (name) => attributes.get(name);
  controls.set(id, element);
  return element;
}
const prompt = '노드를 선택하면 본문과 근거 경로를 표시합니다.';
const details = control('node-details', prompt);
const evidencePrompt = '노드를 선택하면 표시된 부분 그래프의 근거 연결을 안내합니다.';
const evidence = control('node-evidence', evidencePrompt);
const range = control('hop-range');
range.value = html.match(/id="hop-range" type="range" min="0" max="(\d+)"/)[1];
const maximum = range.value;
const fullView = control('full-view');
const status = control('scope-status', '전체 보기');
control('context-graph');
const choices = [...html.matchAll(/data-context-id="([^"]+)"/g)].map((match) => {
  const button = control('choice-' + match[1]);
  button.setAttribute('aria-pressed', 'false');
  button.dataset = { contextId: match[1] };
  return button;
});
const document = new EventTarget();
document.getElementById = (id) => controls.get(id);
document.querySelectorAll = (selector) => {
  assert.equal(selector, '[data-context-id]');
  return choices;
};
let cy;
vm.runInNewContext(scripts[0][1], {
  document,
  cytoscape(options) {
    cy = cytoscape({ ...options, elements: structuredClone(options.elements), style: structuredClone(options.style), container: undefined, headless: true, styleEnabled: true, layout: { name: 'grid' } });
    return cy;
  },
});
assert.equal(cy.nodes().length, choices.length, '표시 노드마다 목록 선택 버튼이 있어야 한다');

function visibleNodes() {
  return cy.nodes().filter((node) => node.visible()).map((node) => node.data('body')).sort();
}
function click(button) {
  button.dispatchEvent(new Event('click'));
}
function changeHops(value) {
  range.value = String(value);
  range.dispatchEvent(new Event('input'));
}
function escape() {
  const event = new Event('keydown');
  Object.defineProperty(event, 'key', { value: 'Escape' });
  document.dispatchEvent(event);
}
function choose(body) {
  const node = cy.nodes().filter((node) => node.data('body') === body);
  assert.equal(node.length, 1);
  node.select();
  node.emit('tap');
  return node;
}
function expectFullView() {
  assert.equal(cy.elements().filter((element) => element.visible()).length, cy.elements().length);
  assert.equal(cy.edges('.evidence').length, 0);
  assert.equal(cy.nodes(':selected').length, 0);
  assert.equal(details.textContent, prompt);
  assert.equal(status.textContent, '전체 보기');
  assert.equal(evidence.textContent, evidencePrompt);
  assert.ok(choices.every((button) => button.getAttribute('aria-pressed') === 'false'));
}

function expectSelection(node) {
  assert.match(status.textContent, new RegExp(node.id()));
  assert.match(status.textContent, /본문과 근거 정보를 갱신했습니다/);
  assert.ok(choices.every((button) => button.getAttribute('aria-pressed') === String(button.dataset.contextId === node.id())));
  assert.match(details.textContent, new RegExp(node.id()));
}

try {
  if (!cy.nodes().length) {
    click(fullView);
    cy.emit('tap');
    escape();
    changeHops(0);
    expectFullView();
    console.log('빈 그래프의 전체 보기·빈 영역·Esc·0홉: 통과');
  } else {
    for (const reset of [() => click(fullView), () => cy.emit('tap'), escape]) {
      const isolated = choose('고립 노드');
      expectSelection(isolated);
      assert.deepEqual(visibleNodes(), ['고립 노드']);
      assert.match(status.textContent, /^국소 보기/);
      assert.match(evidence.textContent, /표시된 부분 그래프에 이 노드의 근거 연결이 없습니다/);
      changeHops(0);
      assert.deepEqual(visibleNodes(), ['고립 노드']);
      changeHops(maximum);
      assert.deepEqual(visibleNodes(), ['고립 노드']);
      reset();
      expectFullView();
      assert.equal(range.value, maximum, '복귀가 홉 값을 바꾸면 안 된다');
      choose('근거 원천');
      assert.match(details.textContent, /근거 원천/);
      reset();
    }
    choose('고립 노드');
    changeHops(0);
    const derived = cy.nodes().filter((node) => node.data('body') === '선택 파생');
    assert.equal(derived.visible(), false);
    const choice = choices.find((button) => button.dataset.contextId === derived.id());
    assert.ok(choice, '숨겨진 파생도 목록에서 선택할 수 있어야 한다');
    click(choice);
    expectSelection(derived);
    assert.deepEqual(visibleNodes(), ['선택 파생']);
    assert.match(details.textContent, /선택 파생/);
    assert.equal(range.value, '0');
    assert.equal(cy.edges('.evidence').length, 1);
    assert.equal(cy.edges('.evidence').source().id(), derived.id(), '들어오는 파생 후손은 근거가 아니다');
    assert.equal(evidence.textContent, '파생 ' + derived.id() + ' → 근거 ' + cy.edges('.evidence').target().id() + ' (derived_from)');
    assert.doesNotMatch(evidence.textContent, new RegExp(cy.nodes().filter((node) => node.data('body') === '파생 후손').id()));
    changeHops(1);
    assert.deepEqual(visibleNodes(), ['근거 원천', '선택 파생', '파생 후손'].sort());
    cy.edges('.evidence').emit('tap');
    assert.match(status.textContent, /^국소 보기/, '간선 클릭은 국소 보기를 해제하지 않는다');
    changeHops(maximum);
    assert.deepEqual(visibleNodes(), ['근거 원천', '선택 파생', '파생 후손', '사건 노드'].sort());
    assert.equal(cy.edges('.evidence').length, 1, '홉 변경은 근거 강조를 유지한다');
    expectSelection(derived);
    click(choice);
    expectFullView();
    click(choice);
    expectSelection(derived);
    click(fullView);
    expectFullView();
    const descendant = choose('파생 후손');
    expectSelection(descendant);
    assert.equal(cy.edges('.evidence').length, 2, '반복 선택에서도 원천까지의 근거 경로를 강조한다');
    assert.equal(evidence.textContent.split('\n').length, 2, '텍스트 근거와 강조 간선이 같아야 한다');
    choose('파생 후손');
    expectFullView();
    click(fullView);
    expectFullView();
    console.log('고립 노드 해제 3종·포인터/목록 토글과 pressed 동기화·0/1/최대 홉·텍스트 근거 방향·변경 안내: 통과');
  }
} finally {
  cy.destroy();
}
