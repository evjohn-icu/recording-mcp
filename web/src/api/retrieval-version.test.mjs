import test from 'node:test'
import assert from 'node:assert/strict'
import { availableRetrievalVersions, resolveRetrievalVersion } from './retrieval-version.ts'

// resolveRetrievalVersion only normalizes the URL literal: which versions the
// dropdown offers is a separate concern (availableRetrievalVersions). An
// explicit v3 URL must never be coerced back to v1 while capabilities load,
// return a legacy response or report v3 unavailable — the backend answers such
// requests itself with admission_unavailable and empty evidence.

test('v3 is only offered when the backend advertises it', () => {
  assert.deepEqual(availableRetrievalVersions(false), ['v1', 'v2'])
  assert.deepEqual(availableRetrievalVersions(true), ['v1', 'v2', 'v3'])
})

test('default stays v1 without a URL parameter', () => {
  assert.equal(resolveRetrievalVersion(null), 'v1')
  assert.equal(resolveRetrievalVersion(undefined), 'v1')
  assert.equal(resolveRetrievalVersion(''), 'v1')
})

test('v2 URL literal is preserved', () => {
  assert.equal(resolveRetrievalVersion('v2'), 'v2')
})

test('explicit v3 URL literal is preserved even when v3 is not advertised', () => {
  assert.equal(resolveRetrievalVersion('v3'), 'v3')
  // The requested version is independent of the dropdown's available list:
  // v3 stays requestable through the URL even while the dropdown hides it.
  assert.equal(availableRetrievalVersions(false).includes('v3'), false)
})

test('unknown parameters fall back to v1', () => {
  assert.equal(resolveRetrievalVersion('v9'), 'v1')
  assert.equal(resolveRetrievalVersion('V2'), 'v1')
})
