import { useState } from "react"
import {
  IconDatabase,
  IconDownload,
  IconShieldCheck,
} from "@tabler/icons-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { FeatureCard, fieldClass, useFeatureAction } from "./feature-card"
import { formatRelative } from "./format"
import { downloadBase64 } from "@/lib/features"
import type { AppView } from "@/lib/types"

export interface DataBinding {
  id: string
  appName: string
  serviceId: string
  createdAt: string
  databaseBytes: number
  walBytes: number
}

export interface DataBackup {
  id: string
  serviceId: string
  serviceName: string
  appName: string
  sizeBytes: number
  createdAt: string
  scheduled: boolean
  verification: "verified" | "stale" | "failed" | string
  verifiedAt?: string
  error?: string
}

export interface BackupPolicy {
  serviceId: string
  frequency: "hourly" | "daily" | "weekly"
  retention: number
  enabled: boolean
  updatedAt: string
}

export interface DataServiceSummary {
  id: string
  name: string
  type: string
  version: string
  state: string
  createdAt: string
  bindings: DataBinding[]
  databaseBytes: number
  walBytes: number
  backups: DataBackup[]
  backupPolicy?: BackupPolicy
}

interface DownloadResult {
  download: {
    fileName: string
    contentType: string
    data: string
    sizeBytes: number
  }
}

interface ExportResult {
  export: {
    fileName: string
    contentType: string
    encryptedData: string
    sizeBytes: number
  }
}

export function DataServiceTools({
  services,
  apps,
}: {
  services: DataServiceSummary[]
  apps: AppView[]
}) {
  const [serviceName, setServiceName] = useState("")
  const [linkAppName, setLinkAppName] = useState("")
  const [backupAppName, setBackupAppName] = useState("")
  const [newServiceName, setNewServiceName] = useState("")
  const [passphrase, setPassphrase] = useState("")
  const [selectedApps, setSelectedApps] = useState<string[]>([])
  const [restoreNames, setRestoreNames] = useState<Record<string, string>>({})
  const [policyDrafts, setPolicyDrafts] = useState<
    Record<string, { frequency: string; retention: string; enabled: boolean }>
  >({})

  const createService = useFeatureAction("dataServices", () =>
    setNewServiceName("")
  )
  const linkService = useFeatureAction("dataServices")
  const unlinkService = useFeatureAction("dataServices")
  const snapshot = useFeatureAction("dataServices")
  const verify = useFeatureAction("dataServices")
  const restore = useFeatureAction("dataServices")
  const policy = useFeatureAction("dataServices")
  const backupDownload = useFeatureAction<DownloadResult>(
    "dataServices",
    (result) =>
      downloadBase64(
        result.download.fileName,
        result.download.data,
        result.download.contentType
      )
  )
  const exportArchive = useFeatureAction<ExportResult>(
    "dataServices",
    (result) => {
      downloadBase64(
        result.export.fileName,
        result.export.encryptedData,
        result.export.contentType
      )
      setPassphrase("")
    }
  )

  const appNames = apps.map((app) => app.Application.Name)
  const passphraseBytes = new TextEncoder().encode(passphrase).length
  const setPolicyDraft = (
    service: DataServiceSummary,
    update: Partial<{ frequency: string; retention: string; enabled: boolean }>
  ) => {
    const current = policyDrafts[service.id] ?? {
      frequency: service.backupPolicy?.frequency ?? "daily",
      retention: String(service.backupPolicy?.retention ?? 7),
      enabled: service.backupPolicy?.enabled ?? false,
    }
    setPolicyDrafts((drafts) => ({
      ...drafts,
      [service.id]: { ...current, ...update },
    }))
  }

  return (
    <div className="grid gap-4">
      <div className="grid gap-4 lg:grid-cols-2">
        <FeatureCard
          title="Create SQLite service"
          description="Create a persistent SQLite service for app database bindings."
        >
          <form
            className="flex flex-col gap-3 sm:flex-row sm:items-end"
            onSubmit={(event) => {
              event.preventDefault()
              createService.mutate({
                action: "create",
                serviceType: "sqlite",
                name: newServiceName,
              })
            }}
          >
            <label className={`${fieldClass} flex-1`}>
              Service name
              <Input
                value={newServiceName}
                maxLength={128}
                onChange={(event) => setNewServiceName(event.target.value)}
                required
              />
            </label>
            <Button
              type="submit"
              disabled={createService.isPending || !newServiceName.trim()}
            >
              {createService.isPending ? "Creating…" : "Create service"}
            </Button>
          </form>
        </FeatureCard>

        <FeatureCard
          title="Link an app"
          description="The binding becomes active on the app’s next restart or deployment."
        >
          <form
            className="grid gap-3 sm:grid-cols-[1fr_1fr_auto] sm:items-end"
            onSubmit={(event) => {
              event.preventDefault()
              linkService.mutate({
                action: "link",
                appName: linkAppName,
                serviceName,
              })
            }}
          >
            <label className={fieldClass}>
              Application
              <select
                className="h-9 rounded-md border bg-background px-2"
                value={linkAppName}
                onChange={(event) => setLinkAppName(event.target.value)}
                required
              >
                <option value="">Choose an app</option>
                {appNames.map((name) => (
                  <option key={name} value={name}>
                    {name}
                  </option>
                ))}
              </select>
            </label>
            <label className={fieldClass}>
              SQLite service
              <select
                className="h-9 rounded-md border bg-background px-2"
                value={serviceName}
                onChange={(event) => setServiceName(event.target.value)}
                required
              >
                <option value="">Choose a service</option>
                {services
                  .filter((service) => service.type === "sqlite")
                  .map((service) => (
                    <option key={service.id} value={service.name}>
                      {service.name}
                    </option>
                  ))}
              </select>
            </label>
            <Button
              type="submit"
              disabled={linkService.isPending || !linkAppName || !serviceName}
            >
              {linkService.isPending ? "Linking…" : "Link app"}
            </Button>
          </form>
          <p className="text-xs text-muted-foreground">
            Removing a binding preserves the database file. A new link to the
            same service reconnects to that data.
          </p>
        </FeatureCard>
      </div>

      {services.length === 0 ? (
        <FeatureCard title="No data services yet">
          <p className="text-sm text-muted-foreground">
            Create a SQLite service above to get started.
          </p>
        </FeatureCard>
      ) : (
        services.map((service) => {
          if (service.type !== "sqlite") {
            return (
              <FeatureCard
                key={service.id}
                title={service.name}
                description={`${service.type} ${service.version} · ${service.state} · ${formatRelative(service.createdAt)}`}
              >
                <p className="text-sm text-muted-foreground">
                  Harbor’s current data-service tools support SQLite. This
                  service record is shown for reference.
                </p>
              </FeatureCard>
            )
          }
          const draft = policyDrafts[service.id] ?? {
            frequency: service.backupPolicy?.frequency ?? "daily",
            retention: String(service.backupPolicy?.retention ?? 7),
            enabled: service.backupPolicy?.enabled ?? false,
          }
          return (
            <FeatureCard
              key={service.id}
              title={service.name}
              description={`${service.type} ${service.version} · ${service.state} · ${formatRelative(service.createdAt)}`}
            >
              <div className="grid gap-3 sm:grid-cols-3">
                <Metric
                  label="Database files"
                  value={formatBytes(service.databaseBytes)}
                />
                <Metric
                  label="WAL files"
                  value={formatBytes(service.walBytes)}
                />
                <Metric
                  label="Bindings"
                  value={String(service.bindings.length)}
                />
              </div>

              <section className="space-y-2">
                <h3 className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">
                  Application bindings
                </h3>
                {service.bindings.length ? (
                  service.bindings.map((binding) => (
                    <div
                      key={binding.id}
                      className="flex flex-col gap-2 rounded-lg border border-border/70 p-3 sm:flex-row sm:items-center sm:justify-between"
                    >
                      <div className="min-w-0">
                        <p className="truncate text-sm font-medium">
                          {binding.appName}
                        </p>
                        <p className="text-xs text-muted-foreground">
                          Database {formatBytes(binding.databaseBytes)} · WAL{" "}
                          {formatBytes(binding.walBytes)}
                        </p>
                      </div>
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={unlinkService.isPending}
                        onClick={() =>
                          unlinkService.mutate({
                            action: "unlink",
                            appName: binding.appName,
                            serviceName: service.name,
                          })
                        }
                      >
                        Unlink · keep data
                      </Button>
                    </div>
                  ))
                ) : (
                  <p className="text-xs text-muted-foreground">
                    No applications are linked.
                  </p>
                )}
              </section>

              <section className="rounded-lg bg-muted/35 p-3">
                <div className="flex items-center gap-2 text-sm font-medium">
                  <IconShieldCheck size={16} /> Scheduled backups
                </div>
                <form
                  className="mt-3 grid gap-3 sm:grid-cols-[1fr_1fr_auto_auto] sm:items-end"
                  onSubmit={(event) => {
                    event.preventDefault()
                    policy.mutate({
                      action: "setBackupPolicy",
                      serviceName: service.name,
                      frequency: draft.frequency,
                      retention: Number(draft.retention),
                      enabled: draft.enabled,
                    })
                  }}
                >
                  <label className={fieldClass}>
                    Frequency
                    <select
                      className="h-9 rounded-md border bg-background px-2"
                      value={draft.frequency}
                      onChange={(event) =>
                        setPolicyDraft(service, {
                          frequency: event.target.value,
                        })
                      }
                    >
                      <option value="hourly">Hourly</option>
                      <option value="daily">Daily</option>
                      <option value="weekly">Weekly</option>
                    </select>
                  </label>
                  <label className={fieldClass}>
                    Keep scheduled copies
                    <Input
                      type="number"
                      min={1}
                      max={100}
                      value={draft.retention}
                      onChange={(event) =>
                        setPolicyDraft(service, {
                          retention: event.target.value,
                        })
                      }
                      required
                    />
                  </label>
                  <label className="flex h-9 items-center gap-2 text-xs">
                    <input
                      type="checkbox"
                      checked={draft.enabled}
                      onChange={(event) =>
                        setPolicyDraft(service, {
                          enabled: event.target.checked,
                        })
                      }
                    />
                    Enable
                  </label>
                  <Button
                    type="submit"
                    variant="outline"
                    disabled={policy.isPending || !draft.retention}
                  >
                    Save policy
                  </Button>
                </form>
              </section>

              <section className="space-y-2">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <h3 className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">
                    SQLite backups
                  </h3>
                  {service.bindings.length ? (
                    <div className="flex flex-wrap items-center gap-2">
                      <select
                        className="h-8 rounded-md border bg-background px-2 text-xs"
                        aria-label={`Application to back up for ${service.name}`}
                        value={backupAppName}
                        onChange={(event) =>
                          setBackupAppName(event.target.value)
                        }
                      >
                        <option value="">Choose app</option>
                        {service.bindings.map((binding) => (
                          <option key={binding.id} value={binding.appName}>
                            {binding.appName}
                          </option>
                        ))}
                      </select>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={
                          snapshot.isPending ||
                          !backupAppName ||
                          !service.bindings.some(
                            (binding) => binding.appName === backupAppName
                          )
                        }
                        onClick={() =>
                          snapshot.mutate({
                            action: "backup",
                            appName: backupAppName,
                            serviceName: service.name,
                          })
                        }
                      >
                        {snapshot.isPending
                          ? "Snapshotting…"
                          : "Create snapshot"}
                      </Button>
                    </div>
                  ) : null}
                </div>
                <p className="text-xs text-muted-foreground">
                  Inline backup downloads are limited to 2 MiB. Larger snapshots
                  can still be verified or cloned into a new app.
                </p>
                {service.backups.length ? (
                  service.backups.map((backup) => (
                    <div
                      key={backup.id}
                      className="grid gap-3 rounded-lg border border-border/70 p-3 lg:grid-cols-[minmax(0,1fr)_auto] lg:items-center"
                    >
                      <div className="min-w-0">
                        <p className="truncate text-sm font-medium">
                          {backup.appName} ·{" "}
                          {backup.scheduled ? "Scheduled" : "Manual"}
                        </p>
                        <p className="mt-0.5 text-xs text-muted-foreground">
                          {formatRelative(backup.createdAt)} ·{" "}
                          {formatBytes(backup.sizeBytes)} · {backup.id}
                        </p>
                        <p
                          className={`mt-1 text-xs ${backup.verification === "verified" ? "text-emerald-700" : backup.verification === "failed" ? "text-red-700" : "text-amber-700"}`}
                        >
                          Integrity: {backup.verification}
                          {backup.error ? ` · ${backup.error}` : ""}
                        </p>
                      </div>
                      <div className="flex flex-wrap items-center gap-2">
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={verify.isPending}
                          onClick={() =>
                            verify.mutate({
                              action: "verifyBackup",
                              backupId: backup.id,
                            })
                          }
                        >
                          Verify
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={
                            backupDownload.isPending ||
                            backup.sizeBytes > 2 * 1024 * 1024
                          }
                          title={
                            backup.sizeBytes > 2 * 1024 * 1024
                              ? "Inline downloads are limited to 2 MiB."
                              : undefined
                          }
                          onClick={() =>
                            backupDownload.mutate({
                              action: "downloadBackup",
                              backupId: backup.id,
                            })
                          }
                        >
                          <IconDownload size={14} /> Download
                        </Button>
                      </div>
                      <form
                        className="grid gap-2 sm:grid-cols-[minmax(160px,1fr)_auto] lg:col-span-2"
                        onSubmit={(event) => {
                          event.preventDefault()
                          restore.mutate({
                            action: "restoreBackup",
                            backupId: backup.id,
                            serviceName: service.name,
                            newAppName: restoreNames[backup.id] ?? "",
                          })
                        }}
                      >
                        <label className={fieldClass}>
                          Clone this backup into a new app
                          <Input
                            value={restoreNames[backup.id] ?? ""}
                            maxLength={128}
                            placeholder={`${backup.appName}-restore`}
                            onChange={(event) =>
                              setRestoreNames((names) => ({
                                ...names,
                                [backup.id]: event.target.value,
                              }))
                            }
                            required
                          />
                        </label>
                        <Button
                          className="self-end"
                          size="sm"
                          variant="outline"
                          disabled={
                            restore.isPending ||
                            backup.verification === "failed"
                          }
                        >
                          Restore as new app
                        </Button>
                      </form>
                    </div>
                  ))
                ) : (
                  <p className="text-xs text-muted-foreground">
                    No snapshots yet.
                  </p>
                )}
              </section>
            </FeatureCard>
          )
        })
      )}

      <FeatureCard
        title="Encrypted Harbor export"
        description="Create a passphrase-protected archive with a consistent control database snapshot, Harbor encryption keys, source metadata, and selected SQLite app databases. Keep the passphrase safe; Harbor does not store it."
      >
        {services.some((service) => service.type !== "sqlite") ? (
          <p className="rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-900">
            App database exports currently support SQLite only. Leave apps using
            another managed database unchecked; including one will make the
            export fail.
          </p>
        ) : null}
        <label className={`${fieldClass} max-w-xl`}>
          Export passphrase (12–1024 bytes)
          <Input
            type="password"
            autoComplete="new-password"
            minLength={12}
            maxLength={1024}
            value={passphrase}
            onChange={(event) => setPassphrase(event.target.value)}
          />
        </label>
        <fieldset className="grid gap-2 sm:grid-cols-2">
          <legend className="mb-2 text-xs font-medium">
            Include app database files
          </legend>
          {apps.map((app) => {
            const name = app.Application.Name
            const checked = selectedApps.includes(name)
            return (
              <label key={name} className="flex items-center gap-2 text-xs">
                <input
                  type="checkbox"
                  checked={checked}
                  disabled={!checked && selectedApps.length >= 20}
                  onChange={(event) =>
                    setSelectedApps((current) =>
                      event.target.checked
                        ? [...current, name]
                        : current.filter((item) => item !== name)
                    )
                  }
                />
                {name}
              </label>
            )
          })}
          {!apps.length ? (
            <p className="text-xs text-muted-foreground">
              No apps available to include.
            </p>
          ) : null}
        </fieldset>
        <div className="flex flex-wrap items-center gap-3">
          <Button
            disabled={
              exportArchive.isPending ||
              passphraseBytes < 12 ||
              passphraseBytes > 1024
            }
            onClick={() =>
              exportArchive.mutate({
                action: "export",
                passphrase,
                appNames: selectedApps,
              })
            }
          >
            <IconDownload size={15} />{" "}
            {exportArchive.isPending
              ? "Building export…"
              : "Download encrypted export"}
          </Button>
          <span className="text-xs text-muted-foreground">
            Archive limit: 48 MiB before encryption; up to 20 selected apps and
            40 MiB of app database snapshots.
          </span>
        </div>
      </FeatureCard>
    </div>
  )
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center gap-2 rounded-lg border border-border/70 px-3 py-2">
      <IconDatabase size={15} className="text-muted-foreground" />
      <div>
        <p className="text-[10px] tracking-wide text-muted-foreground uppercase">
          {label}
        </p>
        <p className="text-sm font-medium">{value}</p>
      </div>
    </div>
  )
}

function formatBytes(bytes: number) {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B"
  const units = ["B", "KiB", "MiB", "GiB"]
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`
}
