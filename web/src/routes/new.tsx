import { useEffect, useMemo, useRef, useState } from "react"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import {
  IconArrowLeft,
  IconArrowRight,
  IconBrandGithub,
  IconCheck,
  IconFolder,
  IconGitBranch,
  IconLoader2,
  IconLock,
  IconSearch,
  IconWorld,
} from "@tabler/icons-react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Separator } from "@/components/ui/separator"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { ConnectionBanner } from "@/components/app/connection-banner"
import { PageLoading } from "@/components/app/page-state"
import { PageHeader } from "@/components/app/page-header"
import {
  cancelOnboardingJob,
  connectGitHub,
  disconnectGitHub,
  getGitHubConnection,
  getOnboardingJob,
  inspectProject,
  submitOnboardingJob,
} from "@/lib/harbor.functions"
import { overviewQueryOptions, reposQueryOptions } from "@/lib/queries"
import type {
  DeploymentJob,
  GitHubRepo,
  OnboardingConfig,
  ProjectInspection,
} from "@/lib/types"
import {
  deployAppInputSchema,
  submitOnboardingJobInputSchema,
} from "@/lib/validation"

export const Route = createFileRoute("/new")({ component: ImportProjectPage })

type ImportValues = {
  name: string
  repoURL: string
  ref: string
  rootDir: string
}
type SourceMode = "browse" | "url"

const githubURLPattern =
  /^https:\/\/github\.com\/[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?\/[A-Za-z0-9][A-Za-z0-9._-]{0,99}(?:\.git)?$/
const githubOwnerPattern = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$/
const terminalJobStates = new Set<DeploymentJob["state"]>([
  "SUCCEEDED",
  "FAILED",
  "CANCELED",
])

function slugFromRepository(name: string) {
  let slug = name
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63)
  if (!/^[a-z]/.test(slug)) slug = `app-${slug}`.slice(0, 63)
  return slug.replace(/-+$/g, "")
}

function projectKey(values: ImportValues) {
  return JSON.stringify([
    values.name.trim(),
    values.repoURL.trim(),
    values.ref.trim(),
    values.rootDir.trim(),
  ])
}

function stageLabel(stage: string) {
  const labels: Record<string, string> = {
    QUEUED: "Queued",
    CLONING: "Getting source",
    PROVISIONING_DATABASE: "Preparing SQLite database",
    BUILDING: "Building project",
    STARTING: "Starting app",
    HEALTH_CHECK: "Checking app",
    COMPLETED: "Ready",
    FAILED: "Needs attention",
    CANCELED: "Canceled",
  }
  return labels[stage] ?? "Working"
}

function ImportProjectPage() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const overview = useQuery(overviewQueryOptions())
  const github = useQuery({
    queryKey: ["github", "connection"] as const,
    queryFn: () => getGitHubConnection(),
    staleTime: 15_000,
    retry: false,
  })
  const [mode, setMode] = useState<SourceMode>("browse")
  const [ownerDraft, setOwnerDraft] = useState("")
  const [submittedOwner, setSubmittedOwner] = useState("")
  const [repoSearch, setRepoSearch] = useState("")
  const [selectedRepo, setSelectedRepo] = useState<GitHubRepo | null>(null)
  const [token, setToken] = useState("")
  const [inspection, setInspection] = useState<ProjectInspection | null>(null)
  const [inspectedKey, setInspectedKey] = useState("")
  const [config, setConfig] = useState<OnboardingConfig | null>(null)
  const [environment, setEnvironment] = useState<Record<string, string>>({})
  const [createSqlite, setCreateSqlite] = useState(false)
  const [sqliteName, setSqliteName] = useState("")
  const [sqliteService, setSqliteService] = useState("")
  const [jobID, setJobID] = useState("")
  const [formError, setFormError] = useState("")
  const currentValues = useRef<ImportValues>({
    name: "",
    repoURL: "",
    ref: "",
    rootDir: "",
  })
  const notifiedJob = useRef("")

  const ownerIsValid = githubOwnerPattern.test(ownerDraft.trim())
  const repos = useQuery({
    ...reposQueryOptions(submittedOwner),
    enabled:
      mode === "browse" &&
      (github.data?.connected === true ||
        githubOwnerPattern.test(submittedOwner)),
  })
  const visibleRepos = useMemo(() => {
    const query = repoSearch.trim().toLowerCase()
    const list = repos.data ?? []
    return query
      ? list.filter((repo) =>
          `${repo.fullName} ${repo.description ?? ""} ${repo.language ?? ""}`
            .toLowerCase()
            .includes(query)
        )
      : list
  }, [repoSearch, repos.data])

  const connectMutation = useMutation({
    mutationFn: (value: string) => connectGitHub({ data: { token: value } }),
    onSuccess: async (connection) => {
      setToken("")
      await queryClient.invalidateQueries({
        queryKey: ["github", "connection"],
      })
      await queryClient.invalidateQueries({ queryKey: ["github", "repos"] })
      toast.success(
        `Connected to GitHub${connection.login ? ` as ${connection.login}` : ""}`
      )
    },
    onError: (error) =>
      toast.error(error.message || "GitHub token could not be verified."),
  })
  const disconnectMutation = useMutation({
    mutationFn: () => disconnectGitHub(),
    onSuccess: async () => {
      setSubmittedOwner("")
      setSelectedRepo(null)
      await queryClient.invalidateQueries({
        queryKey: ["github", "connection"],
      })
      await queryClient.invalidateQueries({ queryKey: ["github", "repos"] })
      toast.success("GitHub connection removed")
    },
    onError: (error) =>
      toast.error(error.message || "GitHub connection could not be removed."),
  })
  const inspectMutation = useMutation({
    mutationFn: (values: ImportValues) =>
      inspectProject({ data: { ...values, name: values.name.trim() } }),
    onSuccess: (result, values) => {
      if (projectKey(currentValues.current) !== projectKey(values)) return
      setInspection(result)
      setInspectedKey(projectKey(values))
      setConfig(result.config)
      setEnvironment(
        Object.fromEntries(result.environmentKeys.map((key) => [key, ""]))
      )
      setFormError("")
    },
    onError: (error) =>
      setFormError(
        error.message || "Harbor could not inspect this repository."
      ),
  })
  const deployMutation = useMutation({
    mutationFn: (input: {
      name: string
      repoURL: string
      ref: string
      rootDir: string
      config: OnboardingConfig
      environment: Record<string, string>
      sqliteService?: string
      createSqlite?: boolean
    }) => {
      const parsed = submitOnboardingJobInputSchema.safeParse(input)
      if (!parsed.success)
        throw new Error("Review the project settings and try again.")
      return submitOnboardingJob({ data: parsed.data })
    },
    onSuccess: (job) => {
      setEnvironment({})
      setJobID(job.id)
      notifiedJob.current = ""
      setFormError("")
      toast.message("Deployment started", {
        description: "You can follow its progress here.",
      })
    },
    onError: (error) =>
      setFormError(error.message || "Harbor could not start this deployment."),
  })
  const cancelMutation = useMutation({
    mutationFn: (id: string) => cancelOnboardingJob({ data: { id } }),
    onError: (error) =>
      toast.error(error.message || "Harbor could not cancel this deployment."),
  })

  const form = useForm({
    defaultValues: currentValues.current,
    onSubmit: async ({ value }) => {
      const normalized = {
        name: value.name.trim(),
        repoURL: value.repoURL.trim(),
        ref: value.ref.trim(),
        rootDir: value.rootDir.trim(),
      }
      const validation = deployAppInputSchema.safeParse(normalized)
      if (!validation.success) {
        setFormError("Enter a valid app name and GitHub repository URL.")
        return
      }
      setFormError("")
      if (inspectedKey !== projectKey(normalized) || !inspection || !config) {
        inspectMutation.mutate(normalized)
        return
      }
      deployMutation.mutate({
        ...normalized,
        config,
        environment,
        ...(createSqlite
          ? { sqliteService: sqliteName.trim(), createSqlite: true }
          : sqliteService
            ? { sqliteService }
            : {}),
      })
    },
  })

  const activeJob = useQuery({
    queryKey: ["onboarding", "job", jobID] as const,
    queryFn: () => getOnboardingJob({ data: { id: jobID } }),
    enabled: Boolean(jobID),
    refetchInterval: (query) =>
      query.state.data && terminalJobStates.has(query.state.data.state)
        ? false
        : 1_000,
    retry: false,
  })

  useEffect(() => {
    const job = activeJob.data
    if (
      !job ||
      !terminalJobStates.has(job.state) ||
      notifiedJob.current === job.id
    )
      return
    notifiedJob.current = job.id
    if (job.state === "SUCCEEDED") {
      void queryClient.invalidateQueries({
        queryKey: overviewQueryOptions().queryKey,
      })
      toast.success(`${job.name} is ready`)
      void navigate({ to: "/projects/$name", params: { name: job.name } })
    } else if (job.state === "FAILED") {
      toast.error(
        job.error || "Deployment failed. Review the attempt logs below."
      )
    } else if (job.state === "CANCELED") {
      toast.message("Deployment canceled")
    }
  }, [activeJob.data, navigate, queryClient])

  if (overview.isPending) return <PageLoading rows={3} />

  function changeSource<T extends keyof ImportValues>(
    key: T,
    value: ImportValues[T]
  ) {
    const next = { ...currentValues.current, [key]: value }
    currentValues.current = next
    switch (key) {
      case "name":
        form.setFieldValue("name", value)
        break
      case "repoURL":
        form.setFieldValue("repoURL", value)
        break
      case "ref":
        form.setFieldValue("ref", value)
        break
      case "rootDir":
        form.setFieldValue("rootDir", value)
        break
    }
    setInspection(null)
    setInspectedKey("")
    setConfig(null)
    setEnvironment({})
    setFormError("")
  }

  function chooseRepo(repo: GitHubRepo) {
    setSelectedRepo(repo)
    changeSource("repoURL", repo.url)
    changeSource("name", slugFromRepository(repo.name))
    changeSource("ref", repo.defaultBranch)
  }

  const checking = inspectMutation.isPending
  const submitting = deployMutation.isPending
  const busy =
    checking ||
    submitting ||
    Boolean(
      jobID && (!activeJob.data || !terminalJobStates.has(activeJob.data.state))
    )
  const inspectionCurrent = Boolean(
    inspection && config && inspectedKey === projectKey(currentValues.current)
  )
  const githubConnectionReady = github.data?.connected === true

  return (
    <>
      <div className="mb-5">
        <Link
          to="/"
          className="inline-flex items-center gap-1.5 text-xs font-medium text-muted-foreground hover:text-foreground"
        >
          <IconArrowLeft size={14} /> Projects
        </Link>
      </div>
      <PageHeader title="Import project" />
      {!overview.data?.connected || overview.data.error || overview.isError ? (
        <ConnectionBanner
          connected={Boolean(overview.data?.connected)}
          error={
            overview.data?.error ||
            (overview.isError ? overview.error.message : undefined)
          }
        />
      ) : null}

      <div className="grid items-start gap-5 xl:grid-cols-[minmax(0,1fr)_320px]">
        <div className="space-y-5">
          <Card className="border-border/80 bg-white">
            <CardHeader className="border-b border-border/60 pb-4">
              <CardTitle className="text-[15px]">Repository source</CardTitle>
            </CardHeader>
            <CardContent className="pt-4">
              <Tabs
                value={mode}
                onValueChange={(value) => setMode(value as SourceMode)}
                className="gap-4"
              >
                <TabsList className="w-full sm:w-fit">
                  <TabsTrigger
                    value="browse"
                    disabled={Boolean(busy)}
                    className="gap-1.5"
                  >
                    <IconBrandGithub size={15} />
                    Browse GitHub
                  </TabsTrigger>
                  <TabsTrigger
                    value="url"
                    disabled={Boolean(busy)}
                    className="gap-1.5"
                  >
                    <IconWorld size={15} />
                    Repository URL
                  </TabsTrigger>
                </TabsList>
                <TabsContent value="browse" className="space-y-4">
                  {!githubConnectionReady ? (
                    <p className="rounded-lg border border-border/70 bg-[#fbfcfb] px-3 py-2.5 text-xs leading-5 text-muted-foreground">
                      Connect GitHub to browse repositories. Public repository
                      URLs can still be imported below without connecting.
                    </p>
                  ) : null}
                  <Field>
                    <FieldLabel htmlFor="github-owner">
                      GitHub owner{githubConnectionReady ? " (optional)" : ""}
                    </FieldLabel>
                    <div className="relative">
                      <IconBrandGithub
                        size={16}
                        className="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-muted-foreground"
                      />
                      <Input
                        id="github-owner"
                        value={ownerDraft}
                        onChange={(event) => {
                          setOwnerDraft(event.target.value)
                          setSubmittedOwner("")
                          setRepoSearch("")
                          setSelectedRepo(null)
                        }}
                        onKeyDown={(event) => {
                          if (event.key === "Enter") {
                            event.preventDefault()
                            if (
                              ownerIsValid ||
                              (githubConnectionReady && !ownerDraft.trim())
                            )
                              setSubmittedOwner(ownerDraft.trim())
                          }
                        }}
                        placeholder="Username or organization"
                        className="pl-8"
                        autoComplete="off"
                        aria-invalid={
                          ownerDraft.trim().length > 0 && !ownerIsValid
                        }
                        disabled={Boolean(busy)}
                      />
                    </div>
                    {ownerDraft.trim().length > 0 && !ownerIsValid ? (
                      <p className="text-xs text-destructive">
                        Use a valid GitHub user or organization name.
                      </p>
                    ) : null}
                  </Field>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled={
                      !(
                        ownerIsValid ||
                        (githubConnectionReady && !ownerDraft.trim())
                      ) ||
                      repos.isFetching ||
                      Boolean(busy)
                    }
                    onClick={() => setSubmittedOwner(ownerDraft.trim())}
                  >
                    {repos.isFetching ? (
                      <IconLoader2
                        data-icon="inline-start"
                        className="animate-spin"
                      />
                    ) : (
                      <IconSearch data-icon="inline-start" />
                    )}
                    {repos.isFetching
                      ? "Loading repositories…"
                      : githubConnectionReady && !ownerDraft.trim()
                        ? "Load accessible repositories"
                        : "Load public repositories"}
                  </Button>
                  {repos.isError &&
                  submittedOwner.toLowerCase() ===
                    ownerDraft.trim().toLowerCase() ? (
                    <div
                      role="alert"
                      className="rounded-lg border border-[#edc8c5] bg-[#fcf0ef] px-3 py-2.5 text-sm text-[#a4463f]"
                    >
                      {repos.error.message ||
                        "Could not load GitHub repositories."}
                    </div>
                  ) : null}
                  {repos.data &&
                  repos.data.length === 0 &&
                  submittedOwner.toLowerCase() ===
                    ownerDraft.trim().toLowerCase() ? (
                    <div className="rounded-lg border border-dashed border-border px-4 py-7 text-center text-sm text-muted-foreground">
                      No repositories found for {submittedOwner}.
                    </div>
                  ) : null}
                  {repos.data &&
                  repos.data.length > 0 &&
                  submittedOwner.toLowerCase() ===
                    ownerDraft.trim().toLowerCase() ? (
                    <div className="overflow-hidden rounded-xl border border-border/70">
                      <div className="flex flex-col gap-2 border-b border-border/60 bg-[#fbfcfb] p-2 sm:flex-row sm:items-center sm:justify-between">
                        <p className="px-1 text-xs text-muted-foreground">
                          {visibleRepos.length} of {repos.data.length}{" "}
                          repositories
                        </p>
                        <div className="relative sm:w-[220px]">
                          <IconSearch
                            size={14}
                            className="pointer-events-none absolute top-1/2 left-2 -translate-y-1/2 text-muted-foreground"
                          />
                          <Input
                            aria-label="Filter repositories"
                            value={repoSearch}
                            onChange={(event) =>
                              setRepoSearch(event.target.value)
                            }
                            placeholder="Filter repositories"
                            className="h-7 border-transparent bg-white pl-7 text-xs"
                            disabled={Boolean(busy)}
                          />
                        </div>
                      </div>
                      <div className="max-h-[320px] divide-y divide-border/60 overflow-y-auto">
                        {visibleRepos.map((repo) => {
                          const selected =
                            selectedRepo?.fullName === repo.fullName
                          return (
                            <Button
                              key={repo.fullName}
                              type="button"
                              variant="ghost"
                              onClick={() => chooseRepo(repo)}
                              className={`h-auto w-full justify-start gap-3 rounded-none px-3.5 py-3 text-left font-normal whitespace-normal ${selected ? "bg-primary/10" : "bg-white"}`}
                              aria-pressed={selected}
                              disabled={Boolean(busy)}
                            >
                              <span className="mt-0.5 grid size-8 shrink-0 place-items-center rounded-lg bg-[#f4f5f3] text-foreground">
                                <IconBrandGithub size={16} />
                              </span>
                              <span className="min-w-0 flex-1">
                                <span className="flex flex-wrap items-center gap-1.5 text-sm font-medium">
                                  <span className="truncate">
                                    {repo.fullName}
                                  </span>
                                  {repo.private ? (
                                    <span className="inline-flex items-center gap-1 rounded-full bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
                                      <IconLock size={10} />
                                      Private
                                    </span>
                                  ) : null}
                                  {selected ? (
                                    <IconCheck
                                      size={14}
                                      className="text-primary-foreground"
                                    />
                                  ) : null}
                                </span>
                                <span className="mt-0.5 block truncate text-xs text-muted-foreground">
                                  {repo.description ||
                                    [
                                      repo.language,
                                      repo.defaultBranch &&
                                        `Default branch: ${repo.defaultBranch}`,
                                    ]
                                      .filter(Boolean)
                                      .join(" · ")}
                                </span>
                              </span>
                            </Button>
                          )
                        })}
                        {visibleRepos.length === 0 ? (
                          <p className="px-4 py-7 text-center text-sm text-muted-foreground">
                            No repositories match that filter.
                          </p>
                        ) : null}
                      </div>
                    </div>
                  ) : null}
                </TabsContent>
                <TabsContent value="url" className="space-y-4">
                  <Field>
                    <FieldLabel htmlFor="repo-url">
                      GitHub repository URL
                    </FieldLabel>
                    <div className="relative">
                      <IconWorld
                        size={16}
                        className="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-muted-foreground"
                      />
                      <Input
                        id="repo-url"
                        value={form.state.values.repoURL}
                        onChange={(event) => {
                          changeSource("repoURL", event.target.value)
                          setSelectedRepo(null)
                        }}
                        placeholder="https://github.com/owner/repository"
                        className="pl-8"
                        aria-invalid={
                          form.state.values.repoURL.length > 0 &&
                          !githubURLPattern.test(
                            form.state.values.repoURL.trim()
                          )
                        }
                        autoComplete="url"
                        disabled={Boolean(busy)}
                      />
                    </div>
                    <FieldDescription>
                      Public repositories work without a GitHub connection.
                      Private URLs use the connected token or a daemon-side
                      GITHUB_TOKEN.
                    </FieldDescription>
                  </Field>
                </TabsContent>
              </Tabs>

              <Separator className="my-5" />
              <form
                className="space-y-4"
                onSubmit={(event) => {
                  event.preventDefault()
                  event.stopPropagation()
                  void form.handleSubmit()
                }}
              >
                <div className="grid gap-4 sm:grid-cols-2">
                  <form.Field name="name">
                    {(field) => (
                      <Field>
                        <FieldLabel htmlFor="app-name">
                          Application name
                        </FieldLabel>
                        <Input
                          id="app-name"
                          value={field.state.value}
                          onBlur={field.handleBlur}
                          onChange={(event) =>
                            changeSource("name", event.target.value)
                          }
                          placeholder="my-app"
                          autoComplete="off"
                          disabled={Boolean(busy)}
                        />
                        <FieldDescription>
                          Lowercase letters, numbers, and hyphens.
                        </FieldDescription>
                      </Field>
                    )}
                  </form.Field>
                  <form.Field name="ref">
                    {(field) => (
                      <Field>
                        <FieldLabel htmlFor="branch-ref">
                          Branch, tag, or commit
                        </FieldLabel>
                        <div className="relative">
                          <IconGitBranch
                            size={15}
                            className="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-muted-foreground"
                          />
                          <Input
                            id="branch-ref"
                            value={field.state.value}
                            onChange={(event) =>
                              changeSource("ref", event.target.value)
                            }
                            placeholder="Default branch"
                            className="pl-8"
                            autoComplete="off"
                            disabled={Boolean(busy)}
                          />
                        </div>
                        <FieldDescription>
                          Leave blank to use the repository default.
                        </FieldDescription>
                      </Field>
                    )}
                  </form.Field>
                </div>
                <form.Field name="rootDir">
                  {(field) => (
                    <Field>
                      <FieldLabel htmlFor="root-dir">Root directory</FieldLabel>
                      <div className="relative">
                        <IconFolder
                          size={15}
                          className="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-muted-foreground"
                        />
                        <Input
                          id="root-dir"
                          value={field.state.value}
                          onChange={(event) =>
                            changeSource("rootDir", event.target.value)
                          }
                          placeholder="."
                          className="pl-8"
                          disabled={Boolean(busy)}
                        />
                      </div>
                      <FieldDescription>
                        Optional repository-relative path containing the app.
                      </FieldDescription>
                    </Field>
                  )}
                </form.Field>

                {formError ? (
                  <div
                    role="alert"
                    className="rounded-lg border border-[#ead9ad] bg-[#fffaf0] px-3 py-2.5 text-sm leading-5 text-[#806d43]"
                  >
                    {formError}
                  </div>
                ) : null}
                {inspectionCurrent ? (
                  <div className="rounded-lg border border-emerald-200 bg-emerald-50 px-3 py-2 text-xs text-emerald-800">
                    Project inspected. Review the settings below before
                    deploying.
                  </div>
                ) : null}
                <div className="flex justify-end border-t border-border/60 pt-4">
                  <Button
                    type="submit"
                    size="sm"
                    disabled={!overview.data?.connected || Boolean(busy)}
                  >
                    {checking || submitting ? (
                      <>
                        <IconLoader2
                          data-icon="inline-start"
                          className="animate-spin"
                        />
                        {checking ? "Inspecting…" : "Queueing…"}
                      </>
                    ) : inspectionCurrent ? (
                      <>
                        Deploy application{" "}
                        <IconArrowRight data-icon="inline-end" />
                      </>
                    ) : (
                      <>
                        Inspect project{" "}
                        <IconArrowRight data-icon="inline-end" />
                      </>
                    )}
                  </Button>
                </div>
              </form>
            </CardContent>
          </Card>

          {inspectionCurrent && inspection && config ? (
            <Card className="border-border/80 bg-white">
              <CardHeader className="border-b border-border/60 pb-4">
                <CardTitle className="text-[15px]">
                  Build and runtime settings
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-4 pt-4">
                {inspection.existingConfig ? (
                  <p className="rounded-lg border border-border/70 bg-[#fbfcfb] px-3 py-2.5 text-xs leading-5 text-muted-foreground">
                    This repository has a <code>paas.yaml</code>; its build and
                    runtime settings are authoritative.
                  </p>
                ) : null}
                {inspection.warnings.map((warning) => (
                  <p
                    key={warning}
                    className="rounded-lg border border-[#ead9ad] bg-[#fffaf0] px-3 py-2 text-xs text-[#806d43]"
                  >
                    {warning}
                  </p>
                ))}
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field>
                    <FieldLabel htmlFor="runtime">Runtime</FieldLabel>
                    <select
                      id="runtime"
                      value={config.runtime}
                      disabled={inspection.existingConfig || Boolean(busy)}
                      onChange={(event) =>
                        setConfig({
                          ...config,
                          runtime: event.target
                            .value as OnboardingConfig["runtime"],
                        })
                      }
                      className="h-9 rounded-md border border-input bg-background px-3 text-sm"
                    >
                      <option value="go">Go</option>
                      <option value="node">Node.js</option>
                    </select>
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="health-path">Health path</FieldLabel>
                    <Input
                      id="health-path"
                      value={config.healthPath}
                      placeholder="Optional"
                      disabled={inspection.existingConfig || Boolean(busy)}
                      onChange={(event) =>
                        setConfig({ ...config, healthPath: event.target.value })
                      }
                    />
                  </Field>
                </div>
                <Field>
                  <FieldLabel htmlFor="build-command">Build command</FieldLabel>
                  <Input
                    id="build-command"
                    value={config.buildCommand}
                    disabled={inspection.existingConfig || Boolean(busy)}
                    onChange={(event) =>
                      setConfig({ ...config, buildCommand: event.target.value })
                    }
                  />
                </Field>
                <Field>
                  <FieldLabel htmlFor="start-command">Start command</FieldLabel>
                  <Input
                    id="start-command"
                    value={config.startCommand}
                    disabled={inspection.existingConfig || Boolean(busy)}
                    onChange={(event) =>
                      setConfig({ ...config, startCommand: event.target.value })
                    }
                  />
                </Field>
                <div className="grid gap-4 sm:grid-cols-3">
                  <Field>
                    <FieldLabel htmlFor="memory-limit">Memory limit</FieldLabel>
                    <Input
                      id="memory-limit"
                      value={config.memoryLimit}
                      disabled={inspection.existingConfig || Boolean(busy)}
                      onChange={(event) =>
                        setConfig({
                          ...config,
                          memoryLimit: event.target.value,
                        })
                      }
                    />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="cpu-quota">CPU quota</FieldLabel>
                    <Input
                      id="cpu-quota"
                      type="number"
                      min="0.01"
                      step="0.01"
                      value={config.cpuQuota}
                      disabled={inspection.existingConfig || Boolean(busy)}
                      onChange={(event) =>
                        setConfig({
                          ...config,
                          cpuQuota: Number(event.target.value),
                        })
                      }
                    />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="tasks-limit">Task limit</FieldLabel>
                    <Input
                      id="tasks-limit"
                      type="number"
                      min="1"
                      step="1"
                      value={config.tasksLimit}
                      disabled={inspection.existingConfig || Boolean(busy)}
                      onChange={(event) =>
                        setConfig({
                          ...config,
                          tasksLimit: Number(event.target.value),
                        })
                      }
                    />
                  </Field>
                </div>
                {inspection.environmentKeys.length > 0 ? (
                  <>
                    <Separator />
                    <div>
                      <h3 className="text-sm font-medium">
                        Environment values
                      </h3>
                      <p className="mt-1 text-xs text-muted-foreground">
                        Names came from the sample file. Values stay private and
                        are never shown again.
                      </p>
                    </div>
                    <div className="grid gap-3 sm:grid-cols-2">
                      {inspection.environmentKeys.map((key) => (
                        <Field key={key}>
                          <FieldLabel htmlFor={`env-${key}`}>{key}</FieldLabel>
                          <Input
                            id={`env-${key}`}
                            type="password"
                            autoComplete="new-password"
                            value={environment[key] ?? ""}
                            onChange={(event) =>
                              setEnvironment({
                                ...environment,
                                [key]: event.target.value,
                              })
                            }
                            disabled={Boolean(busy)}
                          />
                        </Field>
                      ))}
                    </div>
                  </>
                ) : null}
                <Separator />
                <div>
                  <h3 className="text-sm font-medium">Database</h3>
                  <p className="mt-1 text-xs text-muted-foreground">
                    Optional. The app gets a persistent, isolated SQLite
                    database file through DATABASE_URL. Your app needs a SQLite
                    driver; no database credentials or server are needed.
                  </p>
                </div>
                <label className="flex items-center gap-2 text-sm">
                  <input
                    type="radio"
                    name="database-choice"
                    checked={!createSqlite && !sqliteService}
                    disabled={Boolean(busy)}
                    onChange={() => {
                      setCreateSqlite(false)
                      setSqliteService("")
                      setSqliteName("")
                    }}
                  />
                  No database
                </label>
                {overview.data?.services.some(
                  (service) =>
                    service.type === "sqlite" && service.state === "READY"
                ) ? (
                  <>
                    <label className="flex items-center gap-2 text-sm">
                      <input
                        type="radio"
                        name="database-choice"
                        checked={!createSqlite && Boolean(sqliteService)}
                        disabled={Boolean(busy)}
                        onChange={() => {
                          setCreateSqlite(false)
                          setSqliteName("")
                          setSqliteService(
                            overview.data.services.find(
                              (service) =>
                                service.type === "sqlite" &&
                                service.state === "READY"
                            )?.name ?? ""
                          )
                        }}
                      />
                      Link an existing SQLite service
                    </label>
                    {!createSqlite && sqliteService ? (
                      <select
                        aria-label="SQLite service"
                        value={sqliteService}
                        disabled={Boolean(busy)}
                        onChange={(event) =>
                          setSqliteService(event.target.value)
                        }
                        className="h-9 w-full rounded-md border border-input bg-background px-3 text-sm"
                      >
                        <option value="">Choose a service</option>
                        {overview.data.services
                          .filter(
                            (service) =>
                              service.type === "sqlite" &&
                              service.state === "READY"
                          )
                          .map((service) => (
                            <option key={service.id} value={service.name}>
                              {service.name}
                            </option>
                          ))}
                      </select>
                    ) : null}
                  </>
                ) : null}
                <label className="flex items-center gap-2 text-sm">
                  <input
                    type="radio"
                    name="database-choice"
                    checked={createSqlite}
                    disabled={Boolean(busy)}
                    onChange={() => {
                      setCreateSqlite(true)
                      setSqliteService("")
                    }}
                  />
                  Create a managed SQLite service
                </label>
                {createSqlite ? (
                  <Field>
                    <FieldLabel htmlFor="sqlite-name">Service name</FieldLabel>
                    <Input
                      id="sqlite-name"
                      value={sqliteName}
                      onChange={(event) => setSqliteName(event.target.value)}
                      placeholder={`${form.state.values.name}-db`}
                      disabled={Boolean(busy)}
                    />
                  </Field>
                ) : null}
              </CardContent>
            </Card>
          ) : null}

          {jobID ? (
            <Card className="border-border/80 bg-white">
              {activeJob.isError && !activeJob.data ? (
                <>
                  <CardHeader className="border-b border-border/60 pb-4">
                    <CardTitle className="text-[15px]">
                      Progress unavailable
                    </CardTitle>
                  </CardHeader>
                  <CardContent className="space-y-3 pt-4">
                    <p role="alert" className="text-sm text-muted-foreground">
                      {activeJob.error.message ||
                        "The deployment status could not be loaded. The daemon may have restarted."}
                    </p>
                    <div className="flex gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => void activeJob.refetch()}
                      >
                        Retry status
                      </Button>
                      {/not found/i.test(activeJob.error.message) ? (
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => setJobID("")}
                        >
                          Start over
                        </Button>
                      ) : null}
                    </div>
                  </CardContent>
                </>
              ) : activeJob.data ? (
                <>
                  <CardHeader className="flex flex-row items-center justify-between border-b border-border/60 pb-4">
                    <div>
                      <CardTitle className="text-[15px]">
                        Deployment progress
                      </CardTitle>
                      <p className="mt-1 text-xs text-muted-foreground">
                        {stageLabel(activeJob.data.stage)}
                      </p>
                    </div>
                    {!terminalJobStates.has(activeJob.data.state) ? (
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={
                          cancelMutation.isPending ||
                          activeJob.data.state === "CANCELLING"
                        }
                        onClick={() => cancelMutation.mutate(activeJob.data.id)}
                      >
                        Cancel
                      </Button>
                    ) : null}
                  </CardHeader>
                  <CardContent className="space-y-3 pt-4">
                    {activeJob.data.error ? (
                      <p
                        role="alert"
                        className="rounded-lg border border-[#edc8c5] bg-[#fcf0ef] px-3 py-2 text-sm text-[#a4463f]"
                      >
                        {activeJob.data.error}
                      </p>
                    ) : null}
                    {activeJob.isError ? (
                      <p
                        role="status"
                        className="text-xs text-muted-foreground"
                      >
                        Latest progress could not be refreshed.
                      </p>
                    ) : null}
                    {activeJob.data.logs.length > 0 ? (
                      <pre className="max-h-64 overflow-auto rounded-lg bg-[#f5f6f4] p-3 text-xs leading-5 text-muted-foreground">
                        {activeJob.data.logs.join("\n")}
                      </pre>
                    ) : null}
                  </CardContent>
                </>
              ) : (
                <CardContent className="flex items-center gap-2 pt-4 text-sm text-muted-foreground">
                  <IconLoader2 size={15} className="animate-spin" />
                  Loading deployment progress…
                </CardContent>
              )}
            </Card>
          ) : null}
        </div>

        <aside className="space-y-4">
          <Card className="border-border/80 bg-[#fbfcfb] shadow-none">
            <CardHeader className="pb-2">
              <CardTitle className="text-sm">Connect GitHub</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3 text-xs leading-5 text-muted-foreground">
              {github.data?.connected ? (
                <>
                  <p>
                    Connected as{" "}
                    <span className="font-medium text-foreground">
                      {github.data.login}
                    </span>
                    .
                  </p>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled={disconnectMutation.isPending}
                    onClick={() => disconnectMutation.mutate()}
                  >
                    Disconnect GitHub
                  </Button>
                </>
              ) : (
                <>
                  <p>
                    Use a fine-grained personal access token to browse private
                    repositories and clone them.
                  </p>
                  <Field>
                    <FieldLabel htmlFor="github-token">
                      Personal access token
                    </FieldLabel>
                    <Input
                      id="github-token"
                      type="password"
                      autoComplete="new-password"
                      value={token}
                      onChange={(event) => setToken(event.target.value)}
                      placeholder="github_pat_…"
                      disabled={connectMutation.isPending}
                    />
                  </Field>
                  <Button
                    type="button"
                    size="sm"
                    disabled={
                      token.trim().length < 10 || connectMutation.isPending
                    }
                    onClick={() => connectMutation.mutate(token)}
                  >
                    {connectMutation.isPending ? (
                      <>
                        <IconLoader2
                          data-icon="inline-start"
                          className="animate-spin"
                        />
                        Checking token…
                      </>
                    ) : (
                      "Connect GitHub"
                    )}
                  </Button>
                  <p>
                    For a fine-grained token, choose the repositories to deploy,
                    grant <strong>Metadata: read</strong> to list them and{" "}
                    <strong>Contents: read</strong> to clone private source.
                    Organization approval may be required.
                  </p>
                  <p>
                    <a
                      className="underline"
                      href="https://docs.github.com/en/rest/repos/repos"
                      target="_blank"
                      rel="noreferrer"
                    >
                      GitHub repository API permissions
                    </a>{" "}
                    ·{" "}
                    <a
                      className="underline"
                      href="https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens"
                      target="_blank"
                      rel="noreferrer"
                    >
                      Fine-grained token permissions
                    </a>
                  </p>
                  {github.isError ? (
                    <p role="alert" className="text-[#a4463f]">
                      {github.error.message ||
                        "GitHub connection status is unavailable."}
                    </p>
                  ) : null}
                </>
              )}
              <p>
                Public repository URLs also work without connecting. Harbor
                stores the token encrypted on the server and never returns it to
                this page.
              </p>
            </CardContent>
          </Card>
          <Card className="border-border/80 bg-[#fbfcfb] shadow-none">
            <CardHeader className="pb-2">
              <CardTitle className="text-sm">What happens next</CardTitle>
            </CardHeader>
            <CardContent className="text-xs leading-5 text-muted-foreground">
              Harbor clones into a temporary checkout, detects Go or Node
              projects, and validates settings before building. It never commits
              changes to your repository.
            </CardContent>
          </Card>
        </aside>
      </div>
    </>
  )
}
