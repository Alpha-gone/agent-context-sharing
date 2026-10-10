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
  controls.set(id, element);
  return element;
}
const prompt = '노드를 선택하면 본문과 근거 경로를 표시합니다.';
const details = control('node-details', prompt);
const range = control('hop-range');
range.value = html.match(/id="hop-range" type="range" min="0" max="(\d+)"/)[1];
const maximum = range.value;
const fullView = control('full-view');
const status = control('scope-status', '전체 보기');
control('context-graph');
const choices = [...html.matchAll(/data-context-id="([^"]+)"/g)].map((match) => {
  const button = new EventTarget();
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
      choose('고립 노드');
      assert.deepEqual(visibleNodes(), ['고립 노드']);
      assert.equal(status.textContent, '국소 보기');
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
    assert.deepEqual(visibleNodes(), ['선택 파생']);
    assert.match(details.textContent, /선택 파생/);
    assert.equal(range.value, '0');
    assert.equal(cy.edges('.evidence').length, 1);
    assert.equal(cy.edges('.evidence').source().id(), derived.id(), '들어오는 파생 후손은 근거가 아니다');
    changeHops(1);
    assert.deepEqual(visibleNodes(), ['근거 원천', '선택 파생', '파생 후손'].sort());
    cy.edges('.evidence').emit('tap');
    assert.equal(status.textContent, '국소 보기', '간선 클릭은 국소 보기를 해제하지 않는다');
    changeHops(maximum);
    assert.deepEqual(visibleNodes(), ['근거 원천', '선택 파생', '파생 후손', '사건 노드'].sort());
    assert.equal(cy.edges('.evidence').length, 1, '홉 변경은 근거 강조를 유지한다');
    click(fullView);
    expectFullView();
    choose('파생 후손');
    assert.equal(cy.edges('.evidence').length, 2, '반복 선택에서도 원천까지의 근거 경로를 강조한다');
    click(fullView);
    expectFullView();
    console.log('고립 노드 해제 3종·다른 노드 반복 선택·목록 선택·0/1/최대 홉·근거 방향: 통과');
  }
} finally {
  cy.destroy();
}
