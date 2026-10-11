import {
  IconAlertCircle,
  IconArrowUpRight,
  IconPlugConnected,
} from "@tabler/icons-react"
import { Link } from "@tanstack/react-router"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"

export function ConnectionBanner({
  error,
  connected = false,
}: {
  error?: string
  connected?: boolean
}) {
  return (
    <Alert className="mb-6 border-[#ead9ad] bg-[#fffaf0] text-[#6f561c] [&>svg]:text-[#a77e1e]">
      <IconAlertCircle />
      <AlertTitle className="text-sm font-semibold">
        {connected
          ? "Some Harbor data is unavailable"
          : "Harbor is not connected"}
      </AlertTitle>
      <AlertDescription className="flex flex-col gap-2 text-[#806d43] sm:flex-row sm:items-center sm:justify-between">
        <span>
          {error ||
            (connected
              ? "Some sections may be incomplete."
              : "Start the Harbor daemon to view projects and deploy.")}
        </span>
        <Link
          to="/settings"
          className="inline-flex shrink-0 items-center gap-1 text-xs font-semibold text-[#6f561c] hover:underline"
        >
          <IconPlugConnected size={14} /> Connection settings{" "}
          <IconArrowUpRight size={13} />
        </Link>
      </AlertDescription>
    </Alert>
  )
}
