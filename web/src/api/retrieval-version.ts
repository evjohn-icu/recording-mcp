import type { RetrievalVersion } from './types'

// v3 is experimental and only offered when the backend advertises it; v1 stays
// the default no matter what the URL contains.
const BASE_VERSIONS: readonly RetrievalVersion[] = ['v1', 'v2']

export function availableRetrievalVersions(v3Available: boolean): RetrievalVersion[] {
  return v3Available ? [...BASE_VERSIONS, 'v3'] : [...BASE_VERSIONS]
}

export function resolveRetrievalVersion(requested: string | null | undefined, v3Available: boolean): RetrievalVersion {
  if (requested && (availableRetrievalVersions(v3Available) as string[]).includes(requested)) {
    return requested as RetrievalVersion
  }
  return 'v1'
}
