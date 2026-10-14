import { useEffect, useRef, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { FeatureCard } from "./feature-card"
import { downloadText, featureCall, type LogDelta } from "@/lib/features"

export function AdvancedLogs({
  name,
  attempts,
  productionId,
}: {
  name: string
  attempts: Array<{ id: string; state: string }>
  productionId?: string
}) {
  const [deploymentId, setDeploymentId] = useState("")
  const [source, setSource] = useState("all")
  const [paused, setPaused] = useState(false)
  const [follow, setFollow] = useState(true)
  const [search, setSearch] = useState("")
  const [matchIndex, setMatchIndex] = useState(0)
  const [output, setOutput] = useState({ identity: "", text: "" })
  const cursor = useRef({ identity: "", value: "" })
  const pre = useRef<HTMLPreElement>(null)
  const identity = `${name}:${deploymentId}:${source}`
  const query = useQuery({
    queryKey: ["harbor", "log-stream", name, deploymentId, source],
    queryFn: async () => {
      if (cursor.current.identity !== identity)
        cursor.current = { identity, value: "" }
      const result = await featureCall<LogDelta>("observe", {
        action: "logs",
        name,
        deploymentId,
        source,
        cursor: cursor.current.value,
      })
      cursor.current = { identity, value: result.cursor }
      return { ...result, identity }
    },
    enabled: !paused,
    refetchInterval: paused ? false : 2000,
    refetchIntervalInBackground: false,
    retry: false,
  })
  useEffect(() => {
    if (!query.data || query.data.identity !== identity) return
    const result = query.data
    setOutput((previous) => ({
      identity,
      text: (result.reset || previous.identity !== identity
        ? result.text
        : previous.text + result.text
      ).slice(-1_048_576),
    }))
  }, [query.dataUpdatedAt, identity])
  useEffect(() => {
    if (follow && pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [output, follow])
  const text = output.identity === identity ? output.text : ""
  const segments: Array<{ text: string; match: boolean; index: number }> = []
  let count = 0
  if (search) {
    let start = 0
    let found = text.toLowerCase().indexOf(search.toLowerCase())
    while (found >= 0 && count < 2000) {
      segments.push({
        text: text.slice(start, found),
        match: false,
        index: count,
      })
      segments.push({
        text: text.slice(found, found + search.length),
        match: true,
        index: count++,
      })
      start = found + search.length
      found = text.toLowerCase().indexOf(search.toLowerCase(), start)
    }
    segments.push({ text: text.slice(start), match: false, index: count })
  }
  useEffect(() => {
    if (search)
      pre.current
        ?.querySelector(`[data-match="${matchIndex}"]`)
        ?.scrollIntoView({ block: "nearest" })
  }, [matchIndex, search])
  return (
    <FeatureCard
      title="Deployment logs"
      description="Logs stay redacted. Following retains up to 1 MiB; source availability depends on the runtime backend."
    >
      <div className="flex flex-wrap items-center gap-2">
        <select
          aria-label="Log deployment"
          className="h-9 max-w-full rounded-md border px-2 text-xs"
          value={deploymentId}
          onChange={(e) => setDeploymentId(e.target.value)}
        >
          <option value="">Latest attempt</option>
          {productionId ? (
            <option value={productionId}>
              Production · {productionId.slice(-8)}
            </option>
          ) : null}
          {attempts
            .filter((a) => a.id !== productionId)
            .map((a) => (
              <option key={a.id} value={a.id}>
                {a.state} · {a.id.slice(-8)}
              </option>
            ))}
        </select>
        <select
          aria-label="Log source"
          className="h-9 rounded-md border px-2 text-xs"
          value={source}
          onChange={(e) => setSource(e.target.value)}
        >
          <option value="all">All output</option>
          <option value="build">Build</option>
          <option value="runtime">Runtime</option>
        </select>
        <Button variant="outline" size="sm" onClick={() => setPaused(!paused)}>
          {paused ? "Resume" : "Pause"}
        </Button>
        <label className="flex items-center gap-1.5 text-xs">
          <input
            type="checkbox"
            checked={follow}
            onChange={(e) => setFollow(e.target.checked)}
          />
          Auto-scroll
        </label>
        <Button
          variant="outline"
          size="sm"
          disabled={!text}
          onClick={() =>
            downloadText(
              `${name}-${deploymentId || "latest"}-${source}.log`,
              text
            )
          }
        >
          Download logs
        </Button>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Input
          aria-label="Search logs"
          placeholder="Search loaded logs"
          value={search}
          onChange={(e) => {
            setSearch(e.target.value)
            setMatchIndex(0)
          }}
          className="max-w-sm"
        />
        <span className="text-xs text-muted-foreground">{count} matches</span>
        <Button
          variant="outline"
          size="sm"
          disabled={!count}
          onClick={() => {
            setFollow(false)
            setMatchIndex((matchIndex - 1 + count) % count)
          }}
        >
          Previous
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={!count}
          onClick={() => {
            setFollow(false)
            setMatchIndex((matchIndex + 1) % count)
          }}
        >
          Next
        </Button>
      </div>
      {query.isError ? (
        <p role="alert" className="text-sm text-red-700">
          {query.error.message}
        </p>
      ) : null}
      <pre
        ref={pre}
        className="max-h-[540px] min-h-28 overflow-auto rounded-xl bg-[#202726] p-4 font-mono text-xs leading-6 text-[#e6ebe7]"
      >
        <code>
          {search
            ? segments.map((s, i) =>
                s.match ? (
                  <mark
                    key={i}
                    data-match={s.index}
                    className={
                      s.index === matchIndex ? "bg-orange-300" : "bg-yellow-200"
                    }
                  >
                    {s.text}
                  </mark>
                ) : (
                  s.text
                )
              )
            : text || "No log output available."}
        </code>
      </pre>
    </FeatureCard>
  )
}
