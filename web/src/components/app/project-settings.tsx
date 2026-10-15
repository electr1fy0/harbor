import { useState, type FormEvent } from "react"
import { useMutation } from "@tanstack/react-query"
import { IconKey, IconTrash } from "@tabler/icons-react"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { configureApp } from "@/lib/harbor.functions"
import { shouldClearRuntimeSecretDraft } from "@/lib/features"
import type { AppSettings } from "@/lib/types"
import type { ConfigureAppInput } from "@/lib/validation"

interface ProjectSettingsProps {
  name: string
  settings: AppSettings
  onSaved: (settings: AppSettings) => void | Promise<void>
}

export function ProjectSettings({
  name,
  settings,
  onSaved,
}: ProjectSettingsProps) {
  const [current, setCurrent] = useState(settings)
  const [environmentKey, setEnvironmentKey] = useState("")
  const [environmentValue, setEnvironmentValue] = useState("")
  const [memoryLimit, setMemoryLimit] = useState(settings.memoryLimit)
  const [cpuQuota, setCpuQuota] = useState(String(settings.cpuQuota || ""))
  const [tasksLimit, setTasksLimit] = useState(
    String(settings.tasksLimit || "")
  )
  const [message, setMessage] = useState("")

  const updateMutation = useMutation({
    mutationFn: (input: Omit<ConfigureAppInput, "name">) =>
      configureApp({ data: { name, ...input } }),
    onSuccess: async (updated, input) => {
      setCurrent(updated)
      setMemoryLimit(updated.memoryLimit)
      setCpuQuota(String(updated.cpuQuota || ""))
      setTasksLimit(String(updated.tasksLimit || ""))
      if (shouldClearRuntimeSecretDraft(input)) {
        setEnvironmentKey("")
        setEnvironmentValue("")
      }
      setMessage("")
      await onSaved(updated)
    },
    onError: (error) =>
      setMessage(error.message || "Could not update settings."),
  })

  const pending = updateMutation.isPending

  function saveEnvironment(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const key = environmentKey.trim()
    if (!/^[A-Za-z_][A-Za-z0-9_]{0,127}$/.test(key)) {
      setMessage("Use a valid environment variable name.")
      return
    }
    if (!environmentValue) {
      setMessage("Enter a value to add or replace this secret.")
      return
    }
    updateMutation.mutate({ environment: { [key]: environmentValue } })
  }

  function saveResources(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const cpu = Number(cpuQuota)
    const tasks = Number(tasksLimit)
    if (
      !memoryLimit.trim() ||
      !Number.isFinite(cpu) ||
      cpu <= 0 ||
      !Number.isInteger(tasks) ||
      tasks <= 0
    ) {
      setMessage(
        "Enter a memory limit, positive CPU quota, and positive task limit."
      )
      return
    }
    updateMutation.mutate({
      memoryLimit: memoryLimit.trim(),
      cpuQuota: cpu,
      tasksLimit: tasks,
    })
  }

  return (
    <div className="grid gap-4">
      <Card className="border-border/80 bg-white">
        <CardHeader className="flex-row items-start gap-3 border-b border-border/60 pb-4">
          <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-[#f5f6f4] text-muted-foreground">
            <IconKey size={18} />
          </span>
          <div>
            <CardTitle className="text-sm">
              Runtime environment secrets
            </CardTitle>
            <CardDescription className="mt-1 leading-5">
              Harbor stores values server-side and shows key names only. Updates
              take effect on the next deployment.
            </CardDescription>
          </div>
        </CardHeader>
        <CardContent className="space-y-4 pt-4">
          {current.environmentKeys.length > 0 ? (
            <ul className="space-y-2" aria-label="Environment variable keys">
              {current.environmentKeys.map((key) => (
                <li
                  key={key}
                  className="flex items-center justify-between gap-3 rounded-lg border border-border/70 px-3 py-2"
                >
                  <code className="truncate text-xs">{key}</code>
                  <Button
                    type="button"
                    size="sm"
                    variant="ghost"
                    disabled={pending}
                    onClick={() =>
                      updateMutation.mutate({ removeEnvironment: [key] })
                    }
                    aria-label={`Remove ${key}`}
                  >
                    <IconTrash size={15} />
                  </Button>
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-sm text-muted-foreground">
              No environment secrets are configured.
            </p>
          )}
          <form
            className="grid gap-3 sm:grid-cols-[minmax(120px,0.7fr)_minmax(180px,1fr)_auto] sm:items-end"
            onSubmit={saveEnvironment}
          >
            <div className="space-y-2">
              <Label htmlFor="environment-key">Key</Label>
              <Input
                id="environment-key"
                autoComplete="off"
                placeholder="API_TOKEN"
                value={environmentKey}
                onChange={(event) => setEnvironmentKey(event.target.value)}
                disabled={pending}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="environment-value">Value</Label>
              <Input
                id="environment-value"
                type="password"
                autoComplete="new-password"
                placeholder="Enter secret value"
                value={environmentValue}
                onChange={(event) => setEnvironmentValue(event.target.value)}
                disabled={pending}
              />
            </div>
            <Button type="submit" variant="outline" disabled={pending}>
              Save secret
            </Button>
          </form>
        </CardContent>
      </Card>

      <Card className="border-border/80 bg-white">
        <CardHeader className="border-b border-border/60 pb-4">
          <CardTitle className="text-sm">Runtime resource limits</CardTitle>
          <CardDescription className="mt-1 leading-5">
            These limits are used by the next deployment.
          </CardDescription>
        </CardHeader>
        <CardContent className="pt-4">
          <div className="mb-4 flex flex-wrap gap-2">
            {[
              { name: "Tiny", memory: "128Mi", cpu: "0.25", tasks: "64" },
              { name: "Small", memory: "512Mi", cpu: "0.5", tasks: "128" },
              { name: "Medium", memory: "1Gi", cpu: "1", tasks: "256" },
            ].map((preset) => (
              <Button
                key={preset.name}
                type="button"
                variant="outline"
                size="sm"
                disabled={pending}
                onClick={() => {
                  setMemoryLimit(preset.memory)
                  setCpuQuota(preset.cpu)
                  setTasksLimit(preset.tasks)
                }}
              >
                {preset.name} · {preset.memory}
              </Button>
            ))}
          </div>
          <form
            className="grid gap-3 sm:grid-cols-4 sm:items-end"
            onSubmit={saveResources}
          >
            <div className="space-y-2">
              <Label htmlFor="memory-limit">Memory</Label>
              <Input
                id="memory-limit"
                placeholder="128Mi"
                value={memoryLimit}
                onChange={(event) => setMemoryLimit(event.target.value)}
                disabled={pending}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="cpu-quota">CPU quota (vCPU)</Label>
              <Input
                id="cpu-quota"
                type="number"
                min="0.01"
                step="0.01"
                value={cpuQuota}
                onChange={(event) => setCpuQuota(event.target.value)}
                disabled={pending}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="tasks-limit">Task limit</Label>
              <Input
                id="tasks-limit"
                type="number"
                min="1"
                step="1"
                value={tasksLimit}
                onChange={(event) => setTasksLimit(event.target.value)}
                disabled={pending}
              />
            </div>
            <Button type="submit" disabled={pending}>
              {pending ? "Saving…" : "Save limits"}
            </Button>
          </form>
        </CardContent>
      </Card>

      {message ? (
        <p
          role="alert"
          className="rounded-lg border border-[#edc8c5] bg-[#fcf0ef] px-3 py-2.5 text-sm text-[#a4463f]"
        >
          {message}
        </p>
      ) : null}
    </div>
  )
}
