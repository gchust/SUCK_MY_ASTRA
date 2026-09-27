'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const html = fs.readFileSync(path.resolve(__dirname, '../../go/cloud_mint_ui.html'), 'utf8');

function source(name) {
  const match = html.match(new RegExp('(?:async )?function ' + name + '\\([^]*?\\n\\}'));
  assert.ok(match, 'missing UI function ' + name);
  return match[0];
}

function context(extra = {}) {
  return vm.createContext({ ...extra });
}

test('UI never promotes legacy match or uncertain predictions to a confirmed verdict', () => {
  const ctx = context();
  vm.runInContext(source('modeltraceVerdictLabel'), ctx);
  assert.equal(ctx.modeltraceVerdictLabel({ fingerprint: { match: true } }), '不确定');
  assert.equal(ctx.modeltraceVerdictLabel({ verdict: 'uncertain', fingerprint: { match: true } }), '不确定');
  assert.equal(ctx.modeltraceVerdictLabel({ verdict: 'probe_failed' }), '探测失败');
  assert.equal(ctx.modeltraceVerdictLabel({ verdict: 'fingerprint_match' }), '参考指纹一致');
  assert.equal(ctx.modeltraceVerdictLabel({ verdict: 'fingerprint_mismatch' }), '参考指纹不一致');
});

test('client path disables account controls and displays attribution boundary', () => {
  const inputs = [{ disabled: false }, { disabled: false }];
  const elements = { mtPath: { value: 'client' }, mtAccount: { querySelectorAll: () => inputs }, mtAccountHint: {} };
  const ctx = context({ $: id => elements[id] });
  vm.runInContext(source('syncModeltracePathControls'), ctx);
  ctx.syncModeltracePathControls();
  assert.ok(inputs.every(x => x.disabled));
  assert.match(elements.mtAccountHint.textContent, /账号未核验/);
  elements.mtPath.value = 'fc';
  ctx.syncModeltracePathControls();
  assert.ok(inputs.every(x => !x.disabled));
});

test('client submissions omit remembered account selections without network calls', async () => {
  let submitted;
  const elements = {
    mtTurns: { value: '3' }, mtPath: { value: 'client' }, mtAccount: {},
    mtModel: { value: 'model-a' }, mtSource: { value: 'fc' }, mtURL: { value: '' },
    mtApiKey: { value: 'fixture-only' }, mtRun: {}, mtResult: { replaceChildren() {} },
  };
  const ctx = context({
    $: id => elements[id], connected: true, busy: false, credential: 'fixture-only',
    checklistValues: () => ['account-a', 'account-b'], message() {}, text: () => ({}),
    renderMt() {}, renderMtMulti() {}, AbortController,
    setTimeout: () => 1, clearTimeout() {}, MODELTRACE: '/fixture-modeltrace',
    fetch: async (url, opts) => {
      assert.equal(url, '/fixture-modeltrace');
      submitted = JSON.parse(opts.body);
      return { ok: true, json: async () => ({ verdict: 'uncertain' }) };
    },
  });
  vm.runInContext(source('modeltraceVerdictLabel') + '\n' + source('runModeltrace'), ctx);
  await ctx.runModeltrace();
  assert.deepEqual(submitted.accounts, []);
  assert.equal(submitted.account, '');
  assert.equal(elements.mtRun.disabled, false);
  assert.equal(ctx.busy, false);
  elements.mtPath.value = 'fc';
  await ctx.runModeltrace();
  assert.deepEqual(submitted.accounts, ['account-a', 'account-b']);
});

test('ModelTrace card does not claim identity or capability certification', () => {
  const card = html.match(/<section[^>]*id="modeltraceCard"[^]*?<\/section>/)[0];
  assert.doesNotMatch(card, /真满血|判定真实模型/);
  assert.match(card, /不证明底层模型身份或实际能力/);
  assert.match(card, /不确定/);
});
