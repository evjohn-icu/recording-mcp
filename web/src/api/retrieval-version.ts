import type { RetrievalVersion } from './types'

// The picker offers v3 when the backend advertises it. URL versions are
// normalized independently so an explicit v3 request reaches the backend.
const BASE_VERSIONS: readonly RetrievalVersion[] = ['v1', 'v2']

export function availableRetrievalVersions(v3Available: boolean): RetrievalVersion[] {
  return v3Available ? [...BASE_VERSIONS, 'v3'] : [...BASE_VERSIONS]
}

export function resolveRetrievalVersion(requested: string | null | undefined): RetrievalVersion {
  if (requested === 'v1' || requested === 'v2' || requested === 'v3') {
    return requested
  }
  return 'v1'
}
