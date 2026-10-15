import { useRef, useState } from "react"
import { useQuery, useMutation } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { FeatureCard, fieldClass, useFeatureAction } from "./feature-card"
import {
  featureCall,
  jsonPayloadByteLength,
  parseDotEnv,
  savedSourceErrorMessage,
  sameInspectionSnapshot,
  shouldClearBuildEnvironmentAfterAction,
} from "@/lib/features"
import { formatDate } from "./format"
import { inspectProject } from "@/lib/harbor.functions"

interface SourceSettings {
  name: string
  repoURL: string
  ref: string
  rootDir: string
  selection: Record<string, unknown>
  includePaths?: string[]
  ignorePaths?: string[]
  buildTimeoutSeconds?: number
  updatedAt: string
}
interface Hook {
  id: string
  name?: string
  appName?: string
  branch: string
  createdAt: string
  revoked?: boolean
}
interface Job {
  checkoutMilliseconds?: number
  id: string
  name: string
  state: string
  stage: string
  error?: string
  createdAt: string
  source?: { commitSHA: string; message: string; author: string; url: string }
  deployment?: { ID: string }
}
export function AppAutomation({ name }: { name: string }) {
  const source = useQuery({
    queryKey: ["harbor", "source", name],
    queryFn: () =>
      featureCall<{ source: SourceSettings; buildEnvironmentKeys: string[] }>(
        "automation",
        { action: "getSource", name }
      ),
    retry: false,
  })
  const hooks = useQuery({
    queryKey: ["harbor", "hooks", name],
    queryFn: () =>
      featureCall<{ hooks: Hook[] }>("automation", {
        action: "listHooks",
        name,
      }),
  })
  const jobs = useQuery({
    queryKey: ["harbor", "jobs", name],
    queryFn: () =>
      featureCall<{ jobs: Job[] }>("automation", { action: "listJobs", name }),
    refetchInterval: 5000,
  })
  const sourceError = savedSourceErrorMessage(source.error)
  const [branch, setBranch] = useState("")
  const [newSecret, setNewSecret] = useState<{
    hook: Hook
    secret: string
    path?: string
    deployPath?: string
  } | null>(null)
  const action = useFeatureAction<{
    hook: Hook
    secret: string
    path?: string
    deployPath?: string
  }>("automation", (data) => {
    if (data.secret) setNewSecret(data)
  })
  return (
    <div className="grid gap-4">
      {source.data ? (
        <SourceForm
          key={JSON.stringify(source.data)}
          source={source.data.source}
          environmentKeys={source.data.buildEnvironmentKeys}
        />
      ) : (
        <FeatureCard title="Repository settings">
          <p className="text-sm">
            {source.isPending
              ? "Loading repository configuration…"
              : (sourceError ??
                "This app has no saved GitHub source. Import a repository to enable automated deployments.")}
          </p>
          {sourceError && (
            <Button
              type="button"
              variant="outline"
              disabled={source.isFetching}
              onClick={() => void source.refetch()}
            >
              {source.isFetching ? "Retrying…" : "Retry"}
            </Button>
          )}
        </FeatureCard>
      )}
      <FeatureCard
        title="Deployment hooks"
        description="Use a secret hook from CI, or configure a signed GitHub push webhook. Tokens can be revoked or regenerated."
      >
        <form
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => {
            e.preventDefault()
            action.mutate({ action: "createHook", name, branch })
          }}
        >
          <label className={fieldClass}>
            Branch
            <Input
              required
              value={branch}
              onChange={(e) => setBranch(e.target.value)}
              placeholder={source.data?.source.ref || "main"}
            />
          </label>
          <Button type="submit" disabled={!source.data || action.isPending}>
            Create hook
          </Button>
        </form>
        {newSecret ? (
          <div className="space-y-2 rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs">
            <p className="font-medium">
              Save this secret now. It will not be shown again.
            </p>
            <Input
              readOnly
              aria-label="New hook secret"
              value={newSecret.secret}
            />
            <p>GitHub payload URL</p>
            <Input
              readOnly
              aria-label="GitHub webhook URL"
              value={`${window.location.origin}${newSecret.path || `/api/hooks/github/${newSecret.hook.id}`}`}
            />
            <p>
              In GitHub repository Settings → Webhooks, choose application/json,
              use this secret, and subscribe to push events.
            </p>
            <p>
              CI endpoint:{" "}
              <code>{`${window.location.origin}${newSecret.deployPath || `/api/hooks/deploy/${newSecret.hook.id}`}`}</code>
            </p>
            <p>
              Send POST with <code>Authorization: Bearer &lt;secret&gt;</code>.
            </p>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setNewSecret(null)}
            >
              I saved the secret
            </Button>
          </div>
        ) : null}
        {hooks.isError ? (
          <p role="alert" className="text-red-700">
            {hooks.error.message}
          </p>
        ) : null}
        {hooks.data?.hooks.map((h) => (
          <div
            key={h.id}
            className="flex flex-wrap items-center justify-between gap-2 rounded-md border p-3 text-xs"
          >
            <div>
              <p className="font-medium">
                {h.branch} · {h.id.slice(-8)}
              </p>
              <p>{formatDate(h.createdAt)}</p>
            </div>
            <div className="flex gap-2">
              <Button
                variant="outline"
                size="sm"
                disabled={action.isPending}
                onClick={() =>
                  action.mutate({ action: "regenerateHook", id: h.id })
                }
              >
                Regenerate secret
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={action.isPending}
                onClick={() =>
                  action.mutate({ action: "revokeHook", id: h.id })
                }
              >
                Revoke
              </Button>
            </div>
          </div>
        ))}
      </FeatureCard>
      <FeatureCard
        title="Deployment queue and Git commits"
        description="Job history survives daemon restarts. Interrupted jobs are recovered or reported explicitly."
      >
        {jobs.isError ? (
          <p role="alert" className="text-red-700">
            {jobs.error.message}
          </p>
        ) : null}
        {jobs.data?.jobs.map((j) => (
          <div key={j.id} className="space-y-2 rounded-lg border p-3 text-xs">
            <div className="flex flex-wrap justify-between gap-2">
              <p className="font-medium">
                {j.state} · {j.stage.toLowerCase().replaceAll("_", " ")}
              </p>
              <p>{formatDate(j.createdAt)}</p>
            </div>
            {j.checkoutMilliseconds !== undefined ? (
              <p>Checkout: {(j.checkoutMilliseconds / 1000).toFixed(2)}s</p>
            ) : null}
            {j.source ? (
              <div>
                <a
                  href={j.source.url}
                  target="_blank"
                  rel="noreferrer"
                  className="font-mono underline"
                >
                  {j.source.commitSHA.slice(0, 10)}
                </a>
                <p className="mt-1 whitespace-pre-wrap">{j.source.message}</p>
                <p className="text-muted-foreground">{j.source.author}</p>
              </div>
            ) : null}
            {j.error ? <p className="text-red-700">{j.error}</p> : null}
            {!["SUCCEEDED", "FAILED", "CANCELED"].includes(j.state) ? (
              <Button
                variant="outline"
                size="sm"
                disabled={action.isPending}
                onClick={() => action.mutate({ action: "cancelJob", id: j.id })}
              >
                Cancel this job
              </Button>
            ) : null}
          </div>
        ))}
        {!jobs.data?.jobs.length ? (
          <p className="text-xs text-muted-foreground">
            No repository deployment jobs recorded.
          </p>
        ) : null}
      </FeatureCard>
    </div>
  )
}
function SourceForm({
  source,
  environmentKeys,
}: {
  source: SourceSettings
  environmentKeys: string[]
}) {
  const [repoURL, setRepo] = useState(source.repoURL)
  const [ref, setRef] = useState(source.ref)
  const [rootDir, setRoot] = useState(source.rootDir)
  const [selection, setSelection] = useState(source.selection)
  const [build, setBuild] = useState(
    String(source.selection.buildCommand ?? "")
  )
  const [start, setStart] = useState(
    String(source.selection.startCommand ?? "/app/server")
  )
  const [health, setHealth] = useState(
    String(source.selection.healthPath ?? "")
  )
  const [timeout, setTimeout] = useState(source.buildTimeoutSeconds || 300)
  const [include, setInclude] = useState((source.includePaths ?? []).join("\n"))
  const [ignore, setIgnore] = useState((source.ignorePaths ?? []).join("\n"))
  const [buildEnv, setBuildEnv] = useState("")
  const [replaceEnv, setReplaceEnv] = useState(false)
  const [error, setError] = useState("")
  const inspectionSnapshot = {
    name: source.name,
    repoURL,
    ref,
    rootDir,
    build,
    start,
    health,
  }
  const currentInspectionSnapshot = useRef(inspectionSnapshot)
  currentInspectionSnapshot.current = inspectionSnapshot
  const action = useFeatureAction("automation", (_data, input) => {
    if (shouldClearBuildEnvironmentAfterAction(input)) {
      setBuildEnv("")
      setReplaceEnv(false)
    }
  })
  const inspection = useMutation({
    mutationFn: (input: Record<string, string>) =>
      inspectProject({
        data: {
          name: input.name,
          repoURL: input.repoURL,
          ref: input.ref,
          rootDir: input.rootDir,
        },
      }),
    onSuccess: (result, input) => {
      if (!sameInspectionSnapshot(input, currentInspectionSnapshot.current))
        return
      setSelection({ ...result.config })
      setBuild(result.config.buildCommand)
      setStart(result.config.startCommand)
      setHealth(result.config.healthPath)
      setError("")
    },
    onError: (err, input) => {
      if (sameInspectionSnapshot(input, currentInspectionSnapshot.current))
        setError(err.message)
    },
  })
  const paths = (value: string) =>
    value
      .split("\n")
      .map((v) => v.trim())
      .filter(Boolean)
  return (
    <FeatureCard
      title="Source and build settings"
      description="Changes apply to subsequent repository deployments. Include and ignore patterns control push triggers, not which files are built."
    >
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          try {
            if (
              replaceEnv &&
              new TextEncoder().encode(buildEnv).length > 48 * 1024
            )
              throw new Error(
                "Build environment imports are limited to 48 KiB."
              )
            const environment = replaceEnv ? parseDotEnv(buildEnv) : undefined
            const input = {
              action: "updateSource",
              source: {
                name: source.name,
                repoURL,
                ref,
                rootDir,
                config: {
                  ...selection,
                  buildCommand: build,
                  startCommand: start,
                  healthPath: health,
                },
                includePaths: paths(include),
                ignorePaths: paths(ignore),
                buildTimeoutSeconds: timeout,
                ...(environment ? { buildEnvironment: environment } : {}),
              },
            }
            if (jsonPayloadByteLength(input) > 64 * 1024)
              throw new Error(
                "Source settings exceed the 64 KiB request limit."
              )
            setError("")
            action.mutate(input)
          } catch (err) {
            setError(
              err instanceof Error ? err.message : "Invalid build environment"
            )
          }
        }}
      >
        <label className={fieldClass}>
          GitHub repository URL
          <Input
            required
            type="url"
            disabled={action.isPending}
            value={repoURL}
            onChange={(e) => setRepo(e.target.value)}
          />
        </label>
        <div className="grid gap-3 sm:grid-cols-2">
          <label className={fieldClass}>
            Branch, tag, or commit
            <Input
              disabled={action.isPending}
              value={ref}
              onChange={(e) => setRef(e.target.value)}
            />
          </label>
          <label className={fieldClass}>
            Repository root directory
            <Input
              disabled={action.isPending}
              value={rootDir}
              onChange={(e) => setRoot(e.target.value)}
            />
          </label>
        </div>
        <Button
          type="button"
          variant="outline"
          className="w-fit"
          disabled={inspection.isPending}
          onClick={() => inspection.mutate({ ...inspectionSnapshot })}
        >
          {inspection.isPending
            ? "Inspecting repository…"
            : "Load build settings from repository"}
        </Button>
        <label className={fieldClass}>
          Build command
          <Input
            required
            disabled={action.isPending}
            value={build}
            onChange={(e) => setBuild(e.target.value)}
          />
        </label>
        <label className={fieldClass}>
          Start command
          <Input
            required
            disabled={action.isPending}
            value={start}
            onChange={(e) => setStart(e.target.value)}
          />
        </label>
        <div className="grid gap-3 sm:grid-cols-2">
          <label className={fieldClass}>
            Health path
            <Input
              disabled={action.isPending}
              value={health}
              onChange={(e) => setHealth(e.target.value)}
            />
          </label>
          <label className={fieldClass}>
            Build timeout (seconds)
            <Input
              type="number"
              disabled={action.isPending}
              min={10}
              max={1800}
              value={timeout}
              onChange={(e) => setTimeout(Number(e.target.value))}
            />
          </label>
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          <label className={fieldClass}>
            Include changed paths · one pattern per line
            <Textarea
              disabled={action.isPending}
              value={include}
              onChange={(e) => setInclude(e.target.value)}
              placeholder="apps/api/**"
            />
          </label>
          <label className={fieldClass}>
            Ignore changed paths · one pattern per line
            <Textarea
              disabled={action.isPending}
              value={ignore}
              onChange={(e) => setIgnore(e.target.value)}
              placeholder="docs/**"
            />
          </label>
        </div>
        <p className="text-xs text-muted-foreground">
          Build environment keys: {environmentKeys.join(", ") || "none"}
        </p>
        <label className="flex items-center gap-2 text-xs">
          <input
            type="checkbox"
            disabled={action.isPending}
            checked={replaceEnv}
            onChange={(e) => setReplaceEnv(e.target.checked)}
          />
          Replace build environment variables
        </label>
        {replaceEnv ? (
          <label className={fieldClass}>
            Build .env document · empty removes all build variables
            <Textarea
              autoComplete="off"
              disabled={action.isPending}
              value={buildEnv}
              onChange={(e) => setBuildEnv(e.target.value)}
              placeholder="PUBLIC_API_URL=https://api.example.com"
            />
          </label>
        ) : null}
        {error ? (
          <p role="alert" className="text-sm text-red-700">
            {error}
          </p>
        ) : null}
        <Button type="submit" className="w-fit" disabled={action.isPending}>
          Save source settings
        </Button>
      </form>
      <Button
        variant="outline"
        disabled={action.isPending}
        onClick={() =>
          action.mutate({
            action: "redeploy",
            name: source.name,
            clearCache: true,
          })
        }
      >
        Clear shared cache and redeploy
      </Button>
    </FeatureCard>
  )
}
