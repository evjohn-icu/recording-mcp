import test from 'node:test'
import assert from 'node:assert/strict'
import { availableRetrievalVersions, resolveRetrievalVersion } from './retrieval-version.ts'

test('v3 is only offered when the backend advertises it', () => {
  assert.deepEqual(availableRetrievalVersions(false), ['v1', 'v2'])
  assert.deepEqual(availableRetrievalVersions(true), ['v1', 'v2', 'v3'])
})

test('default stays v1 without a URL parameter', () => {
  assert.equal(resolveRetrievalVersion(null, false), 'v1')
  assert.equal(resolveRetrievalVersion(undefined, true), 'v1')
  assert.equal(resolveRetrievalVersion('', true), 'v1')
})

test('v2 is honored regardless of v3 availability', () => {
  assert.equal(resolveRetrievalVersion('v2', false), 'v2')
  assert.equal(resolveRetrievalVersion('v2', true), 'v2')
})

test('v3 URL parameter is not coerced back to v1 when available', () => {
  assert.equal(resolveRetrievalVersion('v3', true), 'v3')
})

test('v3 URL parameter falls back to v1 when unavailable', () => {
  assert.equal(resolveRetrievalVersion('v3', false), 'v1')
})

test('unknown parameters fall back to v1', () => {
  assert.equal(resolveRetrievalVersion('v9', true), 'v1')
  assert.equal(resolveRetrievalVersion('V2', true), 'v1')
})
