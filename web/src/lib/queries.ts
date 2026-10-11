import { queryOptions } from "@tanstack/react-query"
import { getAuthStatus } from "./auth.functions"
import {
  getApp,
  getCapacity,
  getOverview,
  listGitHubRepos,
} from "./harbor.functions"

export function authQueryOptions() {
  return queryOptions({
    queryKey: ["harbor", "auth"] as const,
    queryFn: () => getAuthStatus(),
    staleTime: 30_000,
    retry: false,
  })
}

export function overviewQueryOptions() {
  return queryOptions({
    queryKey: ["harbor", "overview"] as const,
    queryFn: () => getOverview(),
    staleTime: 5_000,
    refetchInterval: 5_000,
    refetchIntervalInBackground: false,
  })
}

export function appQueryOptions(name: string) {
  return queryOptions({
    queryKey: ["harbor", "app", name] as const,
    queryFn: () => getApp({ data: { name } }),
    staleTime: 3_000,
    refetchInterval: 5_000,
    refetchIntervalInBackground: false,
  })
}

export function capacityQueryOptions(name?: string) {
  return queryOptions({
    queryKey: ["harbor", "capacity", name ?? "all"] as const,
    queryFn: () => getCapacity({ data: name ? { name } : {} }),
    staleTime: 10_000,
    refetchInterval: 15_000,
    refetchIntervalInBackground: false,
  })
}

export function reposQueryOptions(owner: string) {
  const normalizedOwner = owner.trim().toLowerCase()
  return queryOptions({
    queryKey: ["github", "repos", normalizedOwner] as const,
    queryFn: () => listGitHubRepos({ data: { owner: normalizedOwner } }),
    staleTime: 60_000,
  })
}
