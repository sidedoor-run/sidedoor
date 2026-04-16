#!/usr/bin/env node

import { Server } from '@modelcontextprotocol/sdk/server/index.js'
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js'
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js'
import { spawn, ChildProcess } from 'child_process'

// Track active tunnel processes: port -> process
const tunnels = new Map<number, { process: ChildProcess; url: string }>()

const server = new Server(
  { name: 'sidedoor', version: '0.1.0' },
  { capabilities: { tools: {} } }
)

// ---- List tools ----

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: 'share_port',
      description:
        'Share a local port publicly using sidedoor. Returns the public URL. Use this when the user wants to share their app, expose localhost, or give someone a link to their running dev server.',
      inputSchema: {
        type: 'object',
        properties: {
          port: {
            type: 'number',
            description: 'The local port to share (e.g. 3000, 5173, 8080)',
          },
          subdomain: {
            type: 'string',
            description: 'Optional custom subdomain (e.g. "my-app" → my-app.sidedoor.run)',
          },
        },
        required: ['port'],
      },
    },
    {
      name: 'stop_sharing',
      description: 'Stop sharing a port that was previously shared with sidedoor.',
      inputSchema: {
        type: 'object',
        properties: {
          port: {
            type: 'number',
            description: 'The port to stop sharing',
          },
        },
        required: ['port'],
      },
    },
    {
      name: 'list_tunnels',
      description: 'List all currently active sidedoor tunnels.',
      inputSchema: {
        type: 'object',
        properties: {},
      },
    },
  ],
}))

// ---- Handle tool calls ----

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: args } = request.params

  switch (name) {
    case 'share_port': {
      const port = args?.port as number
      const subdomain = args?.subdomain as string | undefined

      if (tunnels.has(port)) {
        const existing = tunnels.get(port)!
        return { content: [{ type: 'text', text: `Port ${port} is already shared at ${existing.url}` }] }
      }

      try {
        const url = await startTunnel(port, subdomain)
        return {
          content: [
            {
              type: 'text',
              text: `Your app is live at ${url}\n\nAnyone with this link can access your local server on port ${port}.`,
            },
          ],
        }
      } catch (err) {
        return {
          content: [{ type: 'text', text: `Failed to start tunnel: ${err instanceof Error ? err.message : 'unknown error'}` }],
          isError: true,
        }
      }
    }

    case 'stop_sharing': {
      const port = args?.port as number
      const tunnel = tunnels.get(port)
      if (!tunnel) {
        return { content: [{ type: 'text', text: `No active tunnel on port ${port}` }] }
      }
      tunnel.process.kill()
      tunnels.delete(port)
      return { content: [{ type: 'text', text: `Stopped sharing port ${port}` }] }
    }

    case 'list_tunnels': {
      if (tunnels.size === 0) {
        return { content: [{ type: 'text', text: 'No active tunnels' }] }
      }
      const lines = Array.from(tunnels.entries())
        .map(([port, t]) => `  port ${port} → ${t.url}`)
        .join('\n')
      return { content: [{ type: 'text', text: `Active tunnels:\n${lines}` }] }
    }

    default:
      return { content: [{ type: 'text', text: `Unknown tool: ${name}` }], isError: true }
  }
})

// ---- Start sidedoor tunnel subprocess ----

function startTunnel(port: number, subdomain?: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const args = [String(port)]
    if (subdomain) args.push('--subdomain', subdomain)

    const proc = spawn('sidedoor', args, { stdio: ['ignore', 'pipe', 'pipe'] })

    let resolved = false
    const timeout = setTimeout(() => {
      if (!resolved) {
        proc.kill()
        reject(new Error('timed out waiting for tunnel URL'))
      }
    }, 15_000)

    const onData = (data: Buffer) => {
      const text = data.toString()
      // Parse the public URL from CLI output
      const match = text.match(/Public\s+(https?:\/\/\S+)/)
      if (match && !resolved) {
        resolved = true
        clearTimeout(timeout)
        const url = match[1]
        tunnels.set(port, { process: proc, url })
        resolve(url)
      }
    }

    proc.stdout?.on('data', onData)
    proc.stderr?.on('data', onData)

    proc.on('error', (err) => {
      if (!resolved) {
        clearTimeout(timeout)
        reject(new Error(`sidedoor not found — run: npm install -g sidedoor\n${err.message}`))
      }
    })

    proc.on('exit', (code) => {
      tunnels.delete(port)
      if (!resolved) {
        clearTimeout(timeout)
        reject(new Error(`sidedoor exited with code ${code}`))
      }
    })
  })
}

// ---- Start MCP server ----

async function main() {
  const transport = new StdioServerTransport()
  await server.connect(transport)
}

main().catch(console.error)
