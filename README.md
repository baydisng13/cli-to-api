# cli-to-api

Serve local agentic CLIs behind an OpenAI-compatible API. Single Go binary, no dependencies, no database.

Point any OpenAI client at `http://localhost:8080/v1` and the text comes from whatever CLI you have installed — Antigravity (`agy`), any shell command, or an upstream API.

Each API key is scoped to **one backend and an explicit list of models**. Keys survive restarts, and only their hash is stored.

```bash
go build -o cli-to-api ./cmd/cli-to-api
./cli-to-api key create     # wizard: pick a CLI, pick models, get a key
./cli-to-api start
```

## The wizard

```
$ cli-to-api key create
? Which CLI should this key use?
   1) agy       Antigravity CLI  found: ~/.local/bin/agy
   2) echo      Echo (testing)  found: builtin
   3) shellcmd  Custom shell command  configure below
   4) upstream  Upstream OpenAI-compatible API  configure below
  number > 1

Configure Antigravity CLI:
?   Path to the agy binary: [agy]
?   Extra flags for every invocation:
?   Working directory (blank = a fresh temp dir per request):

Asking agy what it can run...
? Which models may this key use?
   1) [x] gemini-3.8-flash-high    Gemini 3.8 Flash (High)
   2) [ ] gemini-3.8-flash-medium  Gemini 3.8 Flash (Medium)
   3) [ ] gemini-3.8-flash-low     Gemini 3.8 Flash (Low)
   4) [ ] claude-sonnet-4-6        Claude Sonnet 4.6 (Thinking)
  numbers, ranges (1-3), "all", or "none"
  select > 3
? Name this key: [agy-key] my-app

ok key c07888e4 created

  cta_live_c07888e4_EXAMPLEKEYdoNOTuseTHISvalueXXXXXXXXXXXXXXX
  ^ copy this now; only its hash is stored, so it cannot be shown again
```

It then prints a ready-to-paste `curl` for that key.

## Commands

```
cli-to-api start [--addr :8080] [--timeout 5m]
cli-to-api key create
cli-to-api key list
cli-to-api key revoke <id>      # takes effect on the running server immediately
cli-to-api key delete <id>
cli-to-api doctor               # which backends are installed and answering
cli-to-api version
```

Config lives at `$CLI_TO_API_HOME`, else `$XDG_CONFIG_HOME/cli-to-api`, else `~/.config/cli-to-api`. The file is `0600`.

## Endpoints

| Path | Auth | Notes |
|---|---|---|
| `POST /v1/chat/completions` | yes | `stream: true` → SSE deltas + `data: [DONE]`; `conversation_id` to resume |
| `GET /v1/models` | yes | only the models that key is allowed to use |
| `GET /healthz` | no | status + version |

Also served without the `/v1` prefix. Credentials are read from `Authorization: Bearer`, `X-Api-Key`, or `?api_key=`. Unknown paths log the method, path, and a truncated body, so you can see what an unfamiliar client is probing for.

Error codes: `invalid_api_key` (401), `api_key_revoked` (401), `model_not_allowed` (403 — the message lists the models that key *can* use), `conversation_not_found` (404), `sessions_unsupported` (400).

## Backends

| id | What it runs |
|---|---|
| `agy` | Antigravity CLI, via `-p <prompt> --output-format stream-json` |
| `shellcmd` | Any shell command — `{{PROMPT}}` substitution, else prompt on stdin |
| `echo` | Built-in; streams the prompt back. For testing a client's SSE parsing |
| `upstream` | Any OpenAI-compatible HTTP API (OpenRouter, etc.) |

`agy` also supports conversation resume (`--conversation`); the others are stateless. `agy` is parsed at the event level: only `agent_response` text deltas become assistant content, so tool calls and thinking steps never leak into the message, and token counts are agy's own rather than an estimate.

### Adding a backend

One file in `internal/adapter/`, one `Register` call in its `init`:

```go
type Adapter interface {
    ID() string
    DisplayName() string
    Detect(opts map[string]string) (path string, ok bool)
    Models(ctx context.Context, opts map[string]string) ([]Model, error)
    Generate(ctx context.Context, req Request, emit Emit) (Result, error)
    OptionSpec() []Option
}
```
`OptionSpec` is what the wizard asks about. An option marked `Secret` is encrypted before it touches disk and decrypted only in memory at request time.

## Conversations

By default each request is stateless: your client sends `messages[]`, we flatten it, the CLI starts fresh. That already gives continuity, because the client is carrying the history.

Native resume gives you the *agent's own* state — files it created, its task memory — and means you don't resend history. Opt in by sending back the id you were given:

```bash
# turn 1 — the response carries conversation_id
curl .../v1/chat/completions -H "Authorization: Bearer $KEY" \
  -d '{"messages":[{"role":"user","content":"Remember this number: 8231."}]}'
# -> {"choices":[...], "conversation_id":"e48fa0db-..."}

# turn 2 — send only the NEW turn
curl .../v1/chat/completions -H "Authorization: Bearer $KEY" \
  -d '{"conversation_id":"e48fa0db-...","messages":[{"role":"user","content":"What number?"}]}'
# -> "8231"
```

- Send the id as `conversation_id` in the body, or as the `X-Conversation-Id` header for clients that strip unknown body fields.
- You get it back in the response body (`conversation_id`), in the `X-Conversation-Id` header on the resume path, and on the final SSE chunk when streaming — the id is not known until the backend speaks, and headers have left by then.
- **When resuming, send only the newest user message.** The backend already holds the history; the server trims to the latest user turn so the model does not read the conversation twice.
- Supported where the backend supports it: `agy` today. Others return 400 `sessions_unsupported`.
- Bindings live in `sessions.json` (0600) beside the config, so they survive restarts. 24h TTL since last use, 5000-record LRU cap.

**A conversation id is not a credential.** Every id is bound to the key that created it; another key presenting it gets 404 `conversation_not_found` — byte-identical to the response for an id that never existed, so a guessed id cannot even be confirmed as real. This is the failure mode that bites similar tools: without the binding, anyone holding an id joins someone else's thread.

Resume is not a token saving — the backend re-reads the history on its side (observed `input_tokens` growing, `cache_read_tokens` 0). It buys state and convenience, not cost.

## Security notes

- **Keys are hashed, not encrypted.** Format `cta_live_<id>_<secret>`: the id is a public lookup handle, the secret is 32 random bytes stored as `pbkdf2$sha256$10000$<salt>$<hash>`. Compared in constant time. A leaked config file leaks no usable key.
- **Secret adapter options** (an upstream provider key) *are* encrypted — AES-256-GCM, data key in the macOS Keychain, falling back to a `0600` key file.
- **Conversation ids are scoped to their key**, and an id belonging to another key is reported as not existing. See [Conversations](#conversations).
- **Revocation is immediate.** The server notices an external write to the config and reloads before the next lookup.
- **Each request runs in a fresh temp directory** by default. These CLIs are autonomous agents that can read and write files; pointing one at a real project is an explicit opt-in via the `workdir` option.
- **No TLS, no multi-user model.** This binds a local port for local clients. Do not expose it to a network.

## Known limits

- **Requests are stateless unless you pass a `conversation_id`** — see [Conversations](#conversations). Without one, the whole `messages[]` array is flattened into a single prompt.
- **Latency is agent latency** — roughly 7–15s per request for `agy`, and deltas arrive in lumps rather than per token. Streaming UIs will look chunky.
- **Concurrency defaults to 1 per key**, because each request can spawn a large subprocess. Raise `concurrency` in the key's profile if your backend tolerates it.

## Tests

```bash
go test -race ./...
```
Hermetic: no network, no Keychain, no real CLI needed.
