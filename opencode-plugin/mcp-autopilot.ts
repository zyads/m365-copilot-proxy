// mcp-autopilot — OpenCode plugin. Keeps the MCP tool list small and relevant
// for ANY model (Opus, Copilot via proxy, whatever) by connecting/disconnecting
// MCP servers per turn:
//
//   * before each of your messages: servers matched by your text (server-name
//     tokens + your regex rules) and the `always` list are connected; servers
//     idle for `idleTurns` are disconnected. 258 tools → the 20 you need.
//   * the model gets mcp_list / mcp_enable / mcp_disable so it can pull a
//     server in itself mid-task ("I need github for this").
//   * a server whose tools were actually called stays connected.
//
// Install: copy to ~/.config/opencode/plugins/mcp-autopilot.ts (the
// m365-copilot-proxy launcher/installer does this). Config (optional):
// ~/.config/opencode/mcp-autopilot.json — see mcp-autopilot.example.json.

import type { Plugin } from "@opencode-ai/plugin"
import { tool } from "@opencode-ai/plugin"
import { readFileSync, existsSync } from "node:fs"
import { join } from "node:path"
import { homedir } from "node:os"

type Cfg = {
  enabled?: boolean
  always?: string[] // servers that are never disconnected
  never?: string[] // servers autopilot must not touch (managed by you)
  rules?: Record<string, string[]> // regex (case-insensitive) → servers to connect
  idleTurns?: number // disconnect a non-always server after this many unused turns
  disableUnmatched?: boolean // on the first turn, disconnect everything not wanted
}

const CFG_PATH = join(process.env.XDG_CONFIG_HOME ?? join(homedir(), ".config"), "opencode", "mcp-autopilot.json")

function loadCfg(): Required<Cfg> {
  let c: Cfg = {}
  try {
    if (existsSync(CFG_PATH)) c = JSON.parse(readFileSync(CFG_PATH, "utf8").replace(/^\s*\/\/.*$/gm, ""))
  } catch {}
  return {
    enabled: c.enabled ?? true,
    always: c.always ?? [],
    never: c.never ?? [],
    rules: c.rules ?? {},
    idleTurns: c.idleTurns ?? 8,
    disableUnmatched: c.disableUnmatched ?? true,
  }
}

const tokens = (s: string) => s.toLowerCase().split(/[^a-z0-9]+/).filter((t) => t.length >= 3)

export const McpAutopilot: Plugin = async ({ client }) => {
  const cfg = loadCfg()
  let turn = 0
  const lastUsed = new Map<string, number>()
  const tried = new Map<string, number>() // failed/needs_auth servers: don't hammer
  let initialised = false

  const statuses = async (): Promise<Record<string, { status: string }>> => {
    const r = await client.mcp.status()
    return (r.data ?? {}) as any
  }
  const connected = (st: Record<string, { status: string }>, n: string) => st[n]?.status === "connected"

  // Which servers does this text call for?
  const match = (text: string, names: string[]): Set<string> => {
    const low = text.toLowerCase()
    const want = new Set<string>()
    for (const n of names) {
      const ln = n.toLowerCase()
      if (low.includes(ln)) want.add(n)
      else {
        const tk = tokens(n)
        if (tk.length && tk.every((t) => low.includes(t))) want.add(n)
      }
    }
    for (const [re, servers] of Object.entries(cfg.rules)) {
      try {
        if (new RegExp(re, "i").test(text)) servers.forEach((s) => names.includes(s) && want.add(s))
      } catch {}
    }
    return want
  }

  const reconcile = async (want: Set<string>, reason: string) => {
    const st = await statuses()
    const names = Object.keys(st)
    const log: string[] = []
    for (const n of names) {
      if (cfg.never.includes(n)) continue
      const isAlways = cfg.always.includes(n)
      const recentlyUsed = turn - (lastUsed.get(n) ?? -Infinity) < cfg.idleTurns
      const keep = isAlways || want.has(n) || recentlyUsed
      const s = st[n]?.status
      if (keep && s !== "connected") {
        if ((s === "failed" || s === "needs_auth" || s === "needs_client_registration") && turn - (tried.get(n) ?? -Infinity) < 20) continue
        tried.set(n, turn)
        try {
          await client.mcp.connect({ path: { name: n } })
          log.push(`+${n}`)
        } catch {}
      } else if (!keep && s === "connected" && (initialised || cfg.disableUnmatched)) {
        try {
          await client.mcp.disconnect({ path: { name: n } })
          log.push(`-${n}`)
        } catch {}
      }
    }
    initialised = true
    // stderr lands inside the TUI; stay quiet unless asked.
    if (log.length && process.env.MCP_AUTOPILOT_DEBUG) console.error(`[mcp-autopilot] ${reason}: ${log.join(" ")}`)
  }

  if (!cfg.enabled) return {}

  return {
    "chat.message": async (_input, output) => {
      turn++
      const text = (output.parts ?? [])
        .filter((p: any) => p.type === "text" && typeof p.text === "string")
        .map((p: any) => p.text)
        .join("\n")
      const names = Object.keys(await statuses())
      const want = match(text, names)
      cfg.always.forEach((a) => want.add(a))
      want.forEach((n) => lastUsed.set(n, turn))
      await reconcile(want, `turn ${turn}`)
    },

    "tool.execute.after": async (input) => {
      // OpenCode names MCP tools "<server>_<tool>".
      const i = input.tool.indexOf("_")
      if (i > 0) lastUsed.set(input.tool.slice(0, i), turn)
    },

    tool: {
      mcp_list: tool({
        description:
          "List MCP servers and whether they are connected. Disconnected servers' tools are not in your tool list; call mcp_enable to bring one in.",
        args: {},
        async execute() {
          const st = await statuses()
          return Object.entries(st)
            .map(([n, s]) => `${n}: ${s.status}${cfg.always.includes(n) ? " (always)" : ""}`)
            .join("\n") || "no MCP servers configured"
        },
      }),
      mcp_enable: tool({
        description: "Connect an MCP server so its tools become available on your NEXT turn. Use mcp_list to see names.",
        args: { server: tool.schema.string().describe("server name as shown by mcp_list") },
        async execute({ server }) {
          const st = await statuses()
          if (!(server in st)) return `unknown server "${server}". known: ${Object.keys(st).join(", ")}`
          lastUsed.set(server, turn)
          if (connected(st, server)) return `${server} already connected`
          await client.mcp.connect({ path: { name: server } })
          return `${server} connected — its tools are available from your next turn; continue.`
        },
      }),
      mcp_disable: tool({
        description: "Disconnect an MCP server you no longer need (keeps the tool list small).",
        args: { server: tool.schema.string() },
        async execute({ server }) {
          if (cfg.always.includes(server) || cfg.never.includes(server)) return `${server} is pinned by config`
          await client.mcp.disconnect({ path: { name: server } })
          lastUsed.delete(server)
          return `${server} disconnected`
        },
      }),
    },
  }
}

export default McpAutopilot
