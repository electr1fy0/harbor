import { useMemo, useState } from "react"
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
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Separator } from "@/components/ui/separator"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { ConnectionBanner } from "@/components/app/connection-banner"
import { PageLoading } from "@/components/app/page-state"
import { PageHeader } from "@/components/app/page-header"
import { deployApp, getGitHubRepo } from "@/lib/harbor.functions"
import { overviewQueryOptions, reposQueryOptions } from "@/lib/queries"
import type { GitHubRepo } from "@/lib/types"
import { deployAppInputSchema } from "@/lib/validation"

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

function slugFromRepository(name: string) {
  let slug = name
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63)
  if (!/^[a-z]/.test(slug)) slug = `app-${slug}`.slice(0, 63)
  return slug.replace(/-+$/g, "")
}

function ImportProjectPage() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const overview = useQuery(overviewQueryOptions())
  const [mode, setMode] = useState<SourceMode>("browse")
  const [ownerDraft, setOwnerDraft] = useState("")
  const [submittedOwner, setSubmittedOwner] = useState("")
  const [repoSearch, setRepoSearch] = useState("")
  const [repoURLDraft, setRepoURLDraft] = useState("")
  const [selectedRepo, setSelectedRepo] = useState<GitHubRepo | null>(null)
  const [formError, setFormError] = useState("")

  const repos = useQuery({
    ...reposQueryOptions(submittedOwner),
    enabled: mode === "browse" && githubOwnerPattern.test(submittedOwner),
  })
  const normalizedRepoURL = repoURLDraft.trim()
  const metadata = useQuery({
    queryKey: ["github", "repo", normalizedRepoURL],
    queryFn: () => getGitHubRepo({ data: { repoURL: normalizedRepoURL } }),
    enabled: githubURLPattern.test(normalizedRepoURL),
    staleTime: 60_000,
    retry: false,
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

  const deployMutation = useMutation({
    mutationFn: (values: ImportValues) => deployApp({ data: values }),
    onSuccess: async (result, values) => {
      await queryClient.invalidateQueries({
        queryKey: overviewQueryOptions().queryKey,
      })
      if (result.deployment?.State === "RUNNING") {
        toast.success(`${values.name} deployed successfully`)
      } else {
        toast.error(
          result.deployment?.Error ||
            `The deployment for ${values.name} did not reach a running state. Review its logs.`
        )
      }
      await navigate({ to: "/projects/$name", params: { name: values.name } })
    },
    onError: (error) =>
      toast.error(error.message || "Harbor could not start this deployment."),
  })

  const form = useForm({
    defaultValues: {
      name: "",
      repoURL: "",
      ref: "",
      rootDir: "",
    } satisfies ImportValues,
    onSubmit: async ({ value }) => {
      const name = value.name.trim()
      const repoURL = value.repoURL.trim()
      const rootDir = value.rootDir.trim()
      const validation = deployAppInputSchema.safeParse({
        name,
        repoURL,
        ref: value.ref.trim(),
        rootDir,
      })
      if (!validation.success) {
        const labels: Record<string, string> = {
          name: "application name",
          repoURL: "GitHub URL",
          ref: "branch or commit",
          rootDir: "root directory",
        }
        const invalidFields = [
          ...new Set(
            validation.error.issues.map(
              (issue) => labels[String(issue.path[0])] || "form fields"
            )
          ),
        ]
        setFormError(`Check ${invalidFields.join(", ")}.`)
        return
      }
      setFormError("")
      try {
        await deployMutation.mutateAsync(validation.data)
      } catch {
        // The mutation keeps the safe server error for the inline message and toast.
      }
    },
  })

  const pending = deployMutation.isPending
  const ownerIsValid = githubOwnerPattern.test(ownerDraft.trim())
  const showRepoResults =
    Boolean(submittedOwner) &&
    submittedOwner.toLowerCase() === ownerDraft.trim().toLowerCase()

  if (overview.isPending) return <PageLoading rows={3} />
  function chooseRepo(repo: GitHubRepo) {
    setSelectedRepo(repo)
    setRepoURLDraft(repo.url)
    form.setFieldValue("repoURL", repo.url)
    form.setFieldValue("name", slugFromRepository(repo.name))
    form.setFieldValue("ref", repo.defaultBranch)
    setFormError("")
  }

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
                  disabled={pending}
                  className="gap-1.5"
                >
                  <IconBrandGithub size={15} />
                  Browse GitHub
                </TabsTrigger>
                <TabsTrigger value="url" disabled={pending} className="gap-1.5">
                  <IconWorld size={15} />
                  Repository URL
                </TabsTrigger>
              </TabsList>
              <TabsContent value="browse" className="space-y-4">
                <Field>
                  <FieldLabel htmlFor="github-owner">GitHub owner</FieldLabel>
                  <div className="relative">
                    <IconBrandGithub
                      size={16}
                      className="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-muted-foreground"
                    />
                    <Input
                      id="github-owner"
                      value={ownerDraft}
                      onKeyDown={(event) => {
                        if (event.key === "Enter") {
                          event.preventDefault()
                          if (ownerIsValid && !pending) {
                            setSubmittedOwner(ownerDraft.trim())
                          }
                        }
                      }}
                      onChange={(event) => {
                        setOwnerDraft(event.target.value)
                        setRepoSearch("")
                        setSelectedRepo(null)
                        setRepoURLDraft("")
                        form.setFieldValue("repoURL", "")
                        form.setFieldValue("ref", "")
                        setFormError("")
                      }}
                      placeholder="Username or organization"
                      className="pl-8"
                      autoComplete="off"
                      aria-invalid={
                        ownerDraft.trim().length > 0 && !ownerIsValid
                      }
                      disabled={pending}
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
                  disabled={!ownerIsValid || repos.isFetching || pending}
                  onClick={() => setSubmittedOwner(ownerDraft.trim())}
                >
                  {repos.isFetching && showRepoResults ? (
                    <IconLoader2
                      data-icon="inline-start"
                      className="animate-spin"
                    />
                  ) : (
                    <IconSearch data-icon="inline-start" />
                  )}
                  {repos.isFetching && showRepoResults
                    ? "Loading repositories…"
                    : "Load repositories"}
                </Button>

                {repos.isError && showRepoResults ? (
                  <div
                    role="alert"
                    className="rounded-lg border border-[#edc8c5] bg-[#fcf0ef] px-3 py-2.5 text-sm text-[#a4463f]"
                  >
                    {repos.error.message ||
                      `Could not load repositories for ${submittedOwner}.`}
                  </div>
                ) : null}
                {repos.data && showRepoResults && repos.data.length === 0 ? (
                  <div className="rounded-lg border border-dashed border-border px-4 py-7 text-center text-sm text-muted-foreground">
                    No repositories found for {submittedOwner}.
                  </div>
                ) : null}
                {repos.data && showRepoResults && repos.data.length > 0 ? (
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
                          disabled={pending}
                        />
                      </div>
                    </div>
                    <div className="max-h-[320px] divide-y divide-border/60 overflow-y-auto">
                      {visibleRepos.map((repo) => {
                        const selected =
                          selectedRepo?.fullName === repo.fullName
                        const repoDetails =
                          repo.description ||
                          [
                            repo.language,
                            repo.defaultBranch &&
                              `Default branch: ${repo.defaultBranch}`,
                          ]
                            .filter(Boolean)
                            .join(" · ")
                        return (
                          <Button
                            key={repo.fullName}
                            type="button"
                            variant="ghost"
                            size="default"
                            onClick={() => chooseRepo(repo)}
                            className={`h-auto w-full justify-start gap-3 rounded-none px-3.5 py-3 text-left font-normal whitespace-normal ${selected ? "bg-primary/10" : "bg-white"}`}
                            aria-pressed={selected}
                            disabled={pending}
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
                              {repoDetails ? (
                                <span className="mt-0.5 block truncate text-xs text-muted-foreground">
                                  {repoDetails}
                                </span>
                              ) : null}
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
                    <form.Field
                      name="repoURL"
                      validators={{
                        onBlur: ({ value }) =>
                          !value.trim() || githubURLPattern.test(value.trim())
                            ? undefined
                            : "Enter an HTTPS github.com/owner/repository URL.",
                      }}
                    >
                      {(field) => (
                        <>
                          <Input
                            id="repo-url"
                            value={field.state.value}
                            onBlur={field.handleBlur}
                            onChange={(event) => {
                              field.handleChange(event.target.value)
                              setRepoURLDraft(event.target.value)
                              setSelectedRepo(null)
                              setFormError("")
                            }}
                            placeholder="https://github.com/owner/repository"
                            className="pl-8"
                            aria-invalid={field.state.meta.errors.length > 0}
                            autoComplete="url"
                            disabled={pending}
                          />
                          {field.state.meta.errors.length > 0 &&
                          field.state.meta.isTouched ? (
                            <p className="mt-1 text-xs text-destructive">
                              {field.state.meta.errors.join(" ")}
                            </p>
                          ) : null}
                        </>
                      )}
                    </form.Field>
                  </div>
                </Field>
                {metadata.isFetching ? (
                  <p className="flex items-center gap-2 text-xs text-muted-foreground">
                    <IconLoader2 size={14} className="animate-spin" />
                    Checking GitHub repository…
                  </p>
                ) : null}
                {metadata.isError ? (
                  <p
                    role="alert"
                    className="rounded-lg border border-[#ead9ad] bg-[#fffaf0] px-3 py-2.5 text-xs leading-5 text-[#806d43]"
                  >
                    GitHub metadata is unavailable. You can still submit the URL
                    and Harbor will verify source access when it deploys.
                  </p>
                ) : null}
                {metadata.data ? <RepoMetadata repo={metadata.data} /> : null}
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
                <form.Field
                  name="name"
                  validators={{
                    onBlur: ({ value }) =>
                      /^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(value)
                        ? undefined
                        : "Use a lowercase slug beginning with a letter (up to 63 characters).",
                  }}
                >
                  {(field) => (
                    <Field>
                      <FieldLabel htmlFor="app-name">
                        Application name
                      </FieldLabel>
                      <Input
                        id="app-name"
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(event) => {
                          field.handleChange(event.target.value)
                          setFormError("")
                        }}
                        placeholder="my-app"
                        autoComplete="off"
                        aria-invalid={field.state.meta.errors.length > 0}
                        disabled={pending}
                      />
                      <FieldDescription>
                        Lowercase letters, numbers, and hyphens.
                      </FieldDescription>
                      {field.state.meta.errors.length > 0 &&
                      field.state.meta.isTouched ? (
                        <p className="text-xs text-destructive">
                          {field.state.meta.errors.join(" ")}
                        </p>
                      ) : null}
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
                          list="github-branches"
                          value={field.state.value}
                          onChange={(event) => {
                            field.handleChange(event.target.value)
                            setFormError("")
                          }}
                          placeholder="Default branch"
                          className="pl-8"
                          autoComplete="off"
                          disabled={pending}
                        />
                        <datalist id="github-branches">
                          {metadata.data?.branches?.map((branch) => (
                            <option key={branch.name} value={branch.name} />
                          ))}
                        </datalist>
                      </div>
                      <FieldDescription>
                        Leave blank to use the repository default.
                      </FieldDescription>
                    </Field>
                  )}
                </form.Field>
              </div>

              <form.Field
                name="rootDir"
                validators={{
                  onBlur: ({ value }) =>
                    value === "" ||
                    value === "." ||
                    (!value.startsWith("/") &&
                      !value.includes("\\") &&
                      value
                        .split("/")
                        .every((part) => part && part !== "." && part !== ".."))
                      ? undefined
                      : "Use a repository-relative path without . or .. segments.",
                }}
              >
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
                        onBlur={field.handleBlur}
                        onChange={(event) => {
                          field.handleChange(event.target.value)
                          setFormError("")
                        }}
                        placeholder="."
                        className="pl-8"
                        aria-invalid={field.state.meta.errors.length > 0}
                        disabled={pending}
                      />
                    </div>
                    <FieldDescription>
                      Optional path within the repository. The selected
                      directory must contain{" "}
                      <code className="rounded bg-muted px-1 py-0.5 font-mono text-[11px] text-foreground">
                        paas.yaml
                      </code>
                      .
                    </FieldDescription>
                    {field.state.meta.errors.length > 0 &&
                    field.state.meta.isTouched ? (
                      <p className="text-xs text-destructive">
                        {field.state.meta.errors.join(" ")}
                      </p>
                    ) : null}
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
              {deployMutation.isError ? (
                <div
                  role="alert"
                  className="rounded-lg border border-[#edc8c5] bg-[#fcf0ef] px-3 py-2.5 text-sm leading-5 text-[#a4463f]"
                >
                  {deployMutation.error.message ||
                    "Harbor could not start this deployment."}
                </div>
              ) : null}
              <div className="flex flex-col-reverse gap-2 border-t border-border/60 pt-4 sm:flex-row sm:items-center sm:justify-end">
                <div className="flex items-center gap-2">
                  <Link
                    to="/"
                    className={buttonVariants({ size: "sm", variant: "ghost" })}
                  >
                    Cancel
                  </Link>
                  <form.Subscribe
                    selector={(state) =>
                      Boolean(
                        state.values.name.trim() && state.values.repoURL.trim()
                      )
                    }
                  >
                    {(hasRequiredValues) => (
                      <Button
                        type="submit"
                        size="sm"
                        disabled={
                          pending ||
                          !overview.data?.connected ||
                          !hasRequiredValues
                        }
                      >
                        {pending ? (
                          <>
                            <IconLoader2
                              data-icon="inline-start"
                              className="animate-spin"
                            />
                            Deploying…
                          </>
                        ) : (
                          <>
                            Deploy application{" "}
                            <IconArrowRight data-icon="inline-end" />
                          </>
                        )}
                      </Button>
                    )}
                  </form.Subscribe>
                </div>
              </div>
            </form>
          </CardContent>
        </Card>

        <aside className="space-y-4">
          <Card className="border-border/80 bg-[#fbfcfb] shadow-none">
            <CardHeader className="pb-2">
              <CardTitle className="text-sm">Private repositories</CardTitle>
            </CardHeader>
            <CardContent className="text-xs leading-5 text-muted-foreground">
              Set{" "}
              <code className="rounded bg-muted px-1 py-0.5 font-mono text-[11px] text-foreground">
                GITHUB_TOKEN
              </code>{" "}
              in both the web server and daemon. Public repositories need no
              token.
            </CardContent>
          </Card>
        </aside>
      </div>
    </>
  )
}

function RepoMetadata({ repo }: { repo: GitHubRepo }) {
  const details = [
    repo.description,
    repo.defaultBranch && `Default branch ${repo.defaultBranch}`,
  ]
    .filter(Boolean)
    .join(" · ")

  return (
    <div className="rounded-lg border border-border/70 bg-[#fbfcfb] px-3 py-2.5">
      <div className="flex flex-wrap items-center gap-2 text-xs font-medium">
        <IconBrandGithub size={14} />
        <span>{repo.fullName}</span>
        {repo.private ? (
          <span className="inline-flex items-center gap-1 rounded-full bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
            <IconLock size={10} />
            Private
          </span>
        ) : null}
        {repo.language ? (
          <span className="rounded-full bg-white px-1.5 py-0.5 text-[10px] text-muted-foreground">
            {repo.language}
          </span>
        ) : null}
      </div>
      {details ? (
        <p className="mt-1 text-xs text-muted-foreground">{details}</p>
      ) : null}
    </div>
  )
}
