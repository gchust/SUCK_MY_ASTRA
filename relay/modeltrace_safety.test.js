'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { _internals: { wsGradeMessage } } = require('./index.js');

function grade(events) {
  const out = { output: '' };
  let result;
  for (const event of events) {
    const text = typeof event === 'string' ? event : JSON.stringify(event);
    if (wsGradeMessage(text, out, extra => { result = { ...out, ...extra }; })) break;
  }
  return result;
}
const created = { type: 'response.created', response: { id: 'r1', model: 'model-a' } };
const delta = { type: 'response.output_text.delta', response_id: 'r1', delta: '1, 2, 3' };
const completed = { type: 'response.completed', response: { id: 'r1', model: 'model-a', status: 'completed' } };

test('grade requires a correlated completed response', () => {
  const result = grade([created, delta, completed]);
  assert.equal(result.reason, 'ok');
  assert.equal(result.completed, true);
  assert.equal(result.responseID, 'r1');
  assert.equal(result.output, '1, 2, 3');
});

for (const [name, events] of [
  ['partial output is not completion', [created, delta]],
  ['incomplete is not completion', [created, delta, { type: 'response.incomplete', response: { id: 'r1' } }]],
  ['failed is not completion', [created, delta, { type: 'response.failed', response: { id: 'r1', error: { code: 'fixture_error' } } }]],
  ['missing created event', [delta, completed]],
  ['missing created identity', [{ type: 'response.created', response: { model: 'model-a' } }, delta, completed]],
  ['conflicting response identity', [created, delta, { ...completed, response: { ...completed.response, id: 'r2' } }]],
  ['conflicting delta identity', [created, { ...delta, response_id: 'r2' }, completed]],
  ['conflicting model', [created, delta, { ...completed, response: { ...completed.response, model: 'model-b' } }]],
  ['invalid JSON', [created, '{broken']],
]) {
  test(name, () => {
    const result = grade(events);
    assert.notEqual(result?.reason, 'ok');
    assert.notEqual(result?.completed, true);
  });
}
