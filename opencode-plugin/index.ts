import { Plugin } from "@opencode/plugin"

import manifest from "../claude-plugin/.claude-plugin/plugin.json" with { type: "json" }
import mcp from "../claude-plugin/mcp.json" with { type: "json" }

type Server = { type?: string; url?: string; command?: string; args?: string[]; env?: Record<string, string> }

// The Claude Code plugin's servers, in OpenCode's shape. OpenCode hands an
// MCP server the host's environment plus `environment`, and nothing that
// marks it as an agent's: AI_AGENT is what makes a bare `npx tunneld` serve
// MCP on stdio. codemode: false keeps the tools on the model's own tool list,
// where a model calls them by name, rather than behind Code Mode's execute.
export default Plugin.define({
  id: manifest.name,
  async setup(ctx) {
    await ctx.mcp.transform((editor) => {
      for (const [name, server] of Object.entries<Server>(mcp.mcpServers)) {
        if (server.url) {
          editor.set(name, { type: "remote", url: server.url, codemode: false })
        } else if (server.command) {
          editor.set(name, {
            type: "local",
            command: [server.command, ...(server.args ?? [])],
            environment: { AI_AGENT: "opencode", ...server.env },
            codemode: false,
          })
        }
      }
    })
  },
})
