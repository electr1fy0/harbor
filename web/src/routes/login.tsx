import { useEffect, useState, type FormEvent } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { createFileRoute, Link, useRouter } from "@tanstack/react-router"
import { IconShip } from "@tabler/icons-react"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { login } from "@/lib/auth.functions"
import { authQueryOptions } from "@/lib/queries"

export const Route = createFileRoute("/login")({ component: LoginPage })

function LoginPage() {
  const router = useRouter()
  const queryClient = useQueryClient()
  const auth = useQuery(authQueryOptions())
  const [password, setPassword] = useState("")
  const [message, setMessage] = useState("")

  const loginMutation = useMutation({
    mutationFn: (value: string) => login({ data: { password: value } }),
    onSuccess: async (result) => {
      setPassword("")
      if (!result.ok) {
        setMessage(result.message || "Sign-in failed.")
        return
      }
      await queryClient.invalidateQueries({ queryKey: ["harbor", "auth"] })
      await queryClient.setQueryData(authQueryOptions().queryKey, {
        mode: "public" as const,
        authenticated: true,
      })
      await router.navigate({ to: "/" })
    },
    onError: (error) => {
      setPassword("")
      setMessage(error.message || "Sign-in failed.")
    },
  })

  useEffect(() => {
    if (auth.data?.mode === "local" || auth.data?.authenticated) {
      void router.navigate({ to: "/" })
    }
  }, [auth.data, router])

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setMessage("")
    loginMutation.mutate(password)
  }

  if (auth.isPending) {
    return (
      <main className="grid min-h-svh place-items-center px-4 py-10">
        <p className="text-sm text-muted-foreground">Checking sign-in…</p>
      </main>
    )
  }

  if (auth.isError) {
    return (
      <main className="grid min-h-svh place-items-center px-4 py-10">
        <Card className="w-full max-w-sm border-border/80 bg-white">
          <CardHeader>
            <CardTitle className="text-sm">Harbor is unavailable</CardTitle>
          </CardHeader>
          <CardContent>
            <p role="alert" className="text-sm text-muted-foreground">
              {auth.error.message}
            </p>
          </CardContent>
        </Card>
      </main>
    )
  }

  if (auth.data.mode === "local") {
    return (
      <main className="grid min-h-svh place-items-center px-4 py-10">
        <Card className="w-full max-w-sm border-border/80 bg-white">
          <CardHeader>
            <CardTitle className="text-sm">Local access is enabled</CardTitle>
          </CardHeader>
          <CardContent>
            <Link
              to="/"
              className="text-sm font-medium text-primary-foreground hover:underline"
            >
              Open the dashboard
            </Link>
          </CardContent>
        </Card>
      </main>
    )
  }

  return (
    <main className="grid min-h-svh place-items-center px-4 py-10">
      <div className="w-full max-w-sm">
        <div className="mb-5 flex items-center justify-center gap-2.5">
          <span className="grid size-9 place-items-center rounded-[11px] bg-[#202726] text-primary shadow-sm">
            <IconShip size={19} stroke={1.9} />
          </span>
          <span className="text-lg font-semibold tracking-[-0.04em] text-[#17211f]">
            harbor
          </span>
        </div>
        <Card className="border-border/80 bg-white">
          <CardHeader>
            <CardTitle className="text-base">Sign in to Harbor</CardTitle>
            <p className="text-sm leading-5 text-muted-foreground">
              Enter the administrator password configured for this dashboard.
            </p>
          </CardHeader>
          <CardContent>
            <form className="space-y-4" onSubmit={submit}>
              <div className="space-y-2">
                <Label htmlFor="admin-password">Administrator password</Label>
                <Input
                  id="admin-password"
                  type="password"
                  autoComplete="current-password"
                  autoFocus
                  required
                  maxLength={1_024}
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                />
              </div>
              {message ? (
                <p role="alert" className="text-sm text-[#a4463f]">
                  {message}
                </p>
              ) : null}
              <Button
                type="submit"
                className="w-full"
                disabled={loginMutation.isPending || !password}
              >
                {loginMutation.isPending ? "Signing in…" : "Sign in"}
              </Button>
            </form>
          </CardContent>
        </Card>
      </div>
    </main>
  )
}
