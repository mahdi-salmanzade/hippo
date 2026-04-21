# Installing hippo

hippo ships as a single CGO-free binary. Pick the path that matches
your setup.

## From source (any platform)

Requires **Go 1.23 or newer** on your PATH.

```bash
go install github.com/mahdi-salmanzade/hippo/cmd/hippo@latest
```

The binary lands at `$(go env GOBIN)/hippo` (defaults to
`$HOME/go/bin`). Add that directory to your `PATH` if it isn't already.

## From a prebuilt release (no Go required)

Releases publish tarballs for Linux (amd64, arm64), macOS (amd64, arm64),
and Windows (amd64). Grab the archive matching your platform from the
[Releases page](https://github.com/mahdi-salmanzade/hippo/releases/latest),
verify its SHA256 against the published `SHA256SUMS` file, extract, and
put the `hippo` binary somewhere on your PATH.

```bash
# Linux x86_64 example
curl -L -o hippo.tar.gz \
    "https://github.com/mahdi-salmanzade/hippo/releases/latest/download/hippo-linux-amd64.tar.gz"
tar -xzf hippo.tar.gz
install -m 0755 hippo /usr/local/bin/hippo
```

## First-run walkthrough

```bash
hippo init                 # creates ~/.hippo/config.yaml (mode 0600)
hippo serve --open         # starts the web UI on http://127.0.0.1:7844
```

The config page opens in your browser. Paste API keys for the providers
you want to use, flip the **Enabled** toggle, and click **Save**. The
chat page becomes reachable as soon as at least one provider is wired
up.

## Configuring providers

### Anthropic

1. Create a key at [console.anthropic.com/settings/keys](https://console.anthropic.com/settings/keys).
2. Paste it into the Anthropic card on `/config`. Default model:
   `claude-haiku-4-5` (cheap and fast). Override per `Call.Model` or
   per task in the routing policy.

### OpenAI

1. Create a key at [platform.openai.com/api-keys](https://platform.openai.com/api-keys).
2. Paste it into the OpenAI card. Default model: `gpt-5-nano`.

### Ollama (local inference)

1. Install the daemon:

   ```bash
   # macOS
   brew install ollama
   # or see https://ollama.com/download for Linux/Windows
   ```

2. Start it: `ollama serve` (runs on `http://localhost:11434`).
3. Pull a model: `ollama pull llama3.3:70b` (or a smaller one - see
   [ollama.com/library](https://ollama.com/library)).
4. In the hippo UI, enable the Ollama card. The Base URL defaults to
   `http://localhost:11434`.

Ollama inference is free for hippo's budget tracker - the pricing table
marks it `zero_cost`.

## MCP servers (optional)

Model Context Protocol servers expose tools to the chat. Add them on the
config page under **MCP Servers**:

- **stdio**: paste the command that launches the server
  (e.g. `npx -y @scope/your-mcp-server`).
- **http**: paste the URL of a Streamable HTTP MCP endpoint.

Click **Test** to verify the server handshakes and reports tools
before saving.

## Troubleshooting

**"port 7844 already in use"** - Another process holds the port. Either
stop it or start hippo on a different port:

```bash
hippo serve --addr 127.0.0.1:7745
```

**"permission denied on ~/.hippo"** - The directory is created with
mode 0700 on first init. If it was created by a different user (say,
`sudo hippo init`), chown it back:

```bash
sudo chown -R "$USER" ~/.hippo
```

**"auth token required for non-localhost bind"** - Deliberate: binding
to `0.0.0.0` or a real interface without a token would expose your API
keys to the network. Set `server.auth_token` in the config, or pass
`--auth-token <secret>`.

**"ollama: no route to host"** - The daemon isn't running. Start it with
`ollama serve` and confirm `curl http://localhost:11434/api/version`
replies.

**"mcp: initialize: context deadline exceeded"** - The MCP server took
more than 15s to respond to its first request. Either the command line
is wrong, or the server is broken. Check the command runs standalone;
`stderr` output appears in hippo's logs at `--log-level debug`.

**"staticcheck / vet errors on Go 1.25"** - hippo pins deps that
compile on Go 1.23+. If you're on a newer toolchain the test suite
should pass anyway; the pin exists so the CI matrix stays green.
