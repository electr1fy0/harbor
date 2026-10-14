import { useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { configureApp } from "@/lib/harbor.functions"
import { parseDotEnv } from "@/lib/features"
import { FeatureCard } from "./feature-card"

export function EnvironmentImport({
  name,
  existingKeys,
}: {
  name: string
  existingKeys: string[]
}) {
  const [text, setText] = useState("")
  const [error, setError] = useState("")
  const [values, setValues] = useState<Record<string, string> | null>(null)
  const client = useQueryClient()
  const save = useMutation({
    mutationFn: () =>
      configureApp({ data: { name, environment: values ?? {} } }),
    onSuccess: async () => {
      setText("")
      setValues(null)
      toast.success("Environment variables saved")
      await client.invalidateQueries({ queryKey: ["harbor"] })
    },
    onError: (e) => setError(e.message),
  })
  return (
    <FeatureCard
      title="Import environment variables"
      description="Paste a .env document or choose a file. Values stay write-only; imports update listed keys and keep other keys."
    >
      <input
        type="file"
        accept=".env,text/plain"
        aria-label="Choose environment file"
        onChange={async (e) => {
          const file = e.target.files?.[0]
          if (!file) return
          if (file.size > 60000) {
            setError("File is limited to 60 KB.")
            return
          }
          setText(await file.text())
          setValues(null)
          setError("")
        }}
      />
      <Textarea
        aria-label="Environment document"
        className="min-h-28 font-mono text-xs"
        value={text}
        onChange={(e) => {
          setText(e.target.value)
          setValues(null)
        }}
        placeholder={"API_KEY=value\nAPP_MODE=production"}
      />
      <Button
        variant="outline"
        onClick={() => {
          try {
            setValues(parseDotEnv(text))
            setError("")
          } catch (e) {
            setValues(null)
            setError(
              e instanceof Error
                ? e.message
                : "Could not parse environment document."
            )
          }
        }}
      >
        Preview changes
      </Button>
      {error ? (
        <p role="alert" className="text-sm text-red-700">
          {error}
        </p>
      ) : null}
      {values ? (
        <div className="space-y-3">
          <ul className="text-xs">
            {Object.keys(values).map((key) => (
              <li key={key} className="py-1">
                <code>{key}</code> ·{" "}
                {existingKeys.includes(key)
                  ? "replace existing value"
                  : "add new key"}
              </li>
            ))}
          </ul>
          <Button
            disabled={!Object.keys(values).length || save.isPending}
            onClick={() => save.mutate()}
          >
            Import {Object.keys(values).length} variables
          </Button>
        </div>
      ) : null}
    </FeatureCard>
  )
}
