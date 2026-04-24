# sidedoor

Share a local port publicly in one command.

```
npx @sidedoor/cli 3000
```

```
  Local    http://localhost:3000
  Public   https://papita.sidedoor.pink

  ctrl+c to stop
```

---

## Install

```bash
npm install -g @sidedoor/cli
```

## Usage

```bash
# Authenticate once
sidedoor auth

# Share a local port
sidedoor 3000

# Stop
ctrl+c
```

Any port works — dev servers, APIs, whatever is running locally.

---

## How it works

sidedoor connects your machine to a relay over a persistent WebSocket. Incoming HTTPS traffic is tunneled directly to your local port — no polling, no delays. The connection auto-reconnects if it drops.

---

## Status dashboard

When a tunnel is running, a local dashboard is available at `http://localhost:{port+10000}`:

```
sidedoor 3000  →  http://localhost:13000
sidedoor 8080  →  http://localhost:18080
```

It shows live connection state, public URL, uptime, and a probe history so you can verify the tunnel is actually routing traffic end-to-end.

---

## MCP — AI assistant integration

sidedoor ships an MCP server so AI coding assistants (Claude, Cursor, etc.) can open and manage tunnels automatically.

```bash
npm install -g @sidedoor/mcp
```

Add to your MCP config:

```json
{
  "mcpServers": {
    "sidedoor": {
      "command": "sidedoor-mcp"
    }
  }
}
```

The assistant can then share ports, start servers and expose them in one step, and show public URLs without you leaving the chat.

---

## macOS app

A menu bar app shows all running tunnels — whether started from the terminal, MCP, or the app itself. It reflects live connection state including reconnects.

→ [sidedoor-run/desktop](https://github.com/sidedoor-run/desktop)

---

## Auth

sidedoor uses device-flow OAuth. Run `sidedoor auth`, approve in the browser, and your token is saved to the system keychain (macOS Keychain / Linux secret-tool). Run `sidedoor logout` to remove it.

---

## License

MIT
