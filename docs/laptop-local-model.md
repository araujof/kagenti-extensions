# Run an agent on a local model

When an inference provider is out — a dead key, a rate limit, an outage — or
when you would rather the traffic never leave the machine, Cortex can send an
agent's new sessions to a model server running locally. This page walks the
whole path: standing the server up, adding it to Cortex, and what changes for
the agent.

The reference setup is [Ollama](https://ollama.com) running `granite4.2:8b`:
an 8.8B model that calls tools, thinks before answering, and runs on a laptop.
Any OpenAI- or Anthropic-compatible server works the same way — LM Studio,
llama.cpp's `llama-server`, vLLM — and is added with the same two commands;
[Local model servers](../cmd/agentop/README.md#local-model-servers) covers
their specifics.

Two things to know before starting:

- **A local model is a lesser model.** `granite4.2:8b` follows instructions
  and calls tools reliably, but it is not a frontier model: expect more
  retries and wrong turns on hard work. It is a fallback that keeps you
  working — and one whose traffic stays on your machine — not a replacement.
- **Only new sessions move.** A conversation already running keeps the server
  it started on, wherever the agent addressed its requests. Switch before
  starting new work, not in the middle of it.

## Stand up the server

| | |
|---|---|
| macOS | the app from [ollama.com/download](https://ollama.com/download) |
| Linux | `curl -fsSL https://ollama.com/install.sh \| sh` |

Start Ollama — the macOS app does this at login; on Linux, `ollama serve` or
the service the installer sets up — and pull the model:

```sh
ollama pull granite4.2:8b      # a 5.3 GB download
```

Check that it answers:

```sh
curl -s localhost:11434/v1/models | jq -r '.data[].id'
```

`granite4.2:8b` should be on the list. Ollama serves both dialects an agent
sends — the OpenAI chat-completions format and the Anthropic Messages format
— so Claude Code and OpenAI-dialect agents route to it alike, with no
translation gateway between.

### Context length

This is the one setting that matters. A fresh Ollama install runs every model
in a **4096-token window**, whatever
the model declares, and does not refuse a prompt too large for the window: it
trims it silently. Claude Code's opening turn can pass 64k tokens, so routed
to an unconfigured Ollama its instructions would be cut down with no error
anywhere — the agent would simply behave strangely, and nothing would say why.

Raise the window to the model's full 131k and restart the server:

```sh
launchctl setenv OLLAMA_CONTEXT_LENGTH 131072    # the macOS app; restart it after
OLLAMA_CONTEXT_LENGTH=131072 ollama serve        # or a server run from a shell
```

Then check what actually loaded. With the model running, `ollama ps` shows
the window in its CONTEXT column — that is the truth, whatever was
configured:

```
NAME             SIZE     CONTEXT
granite4.2:8b    27 GB    131072
```

The cost is memory: the cache scales with the window, and this model measured
27 GB resident at full context. On a smaller machine, bound the window lower
— 32k is comfortable for agents whose turns stay small, Bob's are around 10k
— but never below the agent's largest turn, and know that below 64k Claude
Code's opening turn can be trimmed.

### Thinking

The model thinks before answering, on by default, and Ollama keeps the
thinking out of the answer: an Anthropic-format reply carries it as separate
`thinking` blocks, an OpenAI one as a `reasoning` field. Leave it on. The
separation is why the reference model is granite **4.2** — granite3.3, the
previous release, leaks its reasoning into the answer, and an agent reading
that answer gets the model's deliberations mixed into its work.

## Route the agent to it

Two commands, and nothing restarts:

```sh
echo ollama | agentop server add ollama http://localhost:11434 \
  --main granite4.2:8b --helper granite4.2:8b --key-stdin
agentop server use ollama --agent claude-code
```

- Ollama checks no key, but `add` insists on one, so `echo ollama |` supplies
  a placeholder; it is read from stdin, so it never lands in shell history.
- `--main` and `--helper` are model words. `add` reads the server's model
  list and refuses a word that names none of its models — which is also what
  catches a wrong URL, or a server that is not running. A word names the
  newest model whose name contains it; the full tag pins the exact one.
  `main` is sent for a request with tools, the agent's main work; `helper`
  for one without, such as a title.
- `--agent` takes the name agentop shows the agent — `agentop configure
  status` lists yours. In the TUI, `S` on the agents pane makes the same
  choice.

The agent needs no setting of its own beyond going through Cortex. The full
command reference — removing a server, rotating a key, what each refusal
means — is [Choosing an inference
server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server).

## What happens next

- **New sessions only.** A session is pinned to the server its first request
  reached and stays there, and the pin survives a proxy restart. Start a new
  `claude` after switching — and switch before a `/clear`, not after: the
  conversation `/clear` begins is what gets pinned.
- **The model.** The agent keeps asking for its own model names — Claude
  Code's, say — which the local server does not have, and refuses. Before
  the agent sees the refusal, the router sends the request again with the
  word's model: one refused round trip, then the substitute is remembered
  for an hour and sent from the start, and the session keeps the model it
  was given.
- **Where it shows.** agentop's sessions table carries a `SERVER` column
  naming where each session's inference went; the detail pane's `redirected:`
  and `model:` lines name the host and the model the agent asked for when
  they were others.
- **What it costs.** Tokens are counted as for any server, and the model
  reports unpriced until you give it a rate ([pricing](pricing.md)) — a
  local model's marginal cost is electricity.
- **Switching back.** `agentop server use <name> --agent <agent>` routes new
  sessions to the other server; conversations running on the local model
  finish where they are.
- **Nothing fails over.** If the local server is down, its sessions' requests
  fail. Route the agent elsewhere and start a new session; Cortex moves no
  conversation on its own.

## Picking a different model

The reference model was picked by criteria, not brand. Check any candidate
the same way:

1. **Tool use** — the make-or-break: a model that cannot call tools cannot
   drive a coding agent. `ollama show <model>` lists it under capabilities.
2. **Separated thinking** — reasoning that leaks into the answer confuses the
   agent reading it. Ask the model something trivial and check the thinking
   arrives in its own field, not in the answer.
3. **Context window** — at least the agent's largest turn; see
   [Context length](#context-length).
4. **Memory** — the weights, plus the cache at the window you run. The pull
   size is only the weights.

Then give it a small real task through the agent before trusting it with a
large one.

## See also

- [Choosing an inference server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server) —
  every `agentop server` command, and the refusal wordings it reads
- [Local model servers](../cmd/agentop/README.md#local-model-servers) —
  vLLM, llama.cpp and LM Studio specifics
- [The inference-router](inference-router-plugin.md) — the routing rules,
  pins and substitution in full
- [Your agent's guide](agents/) — each has a switching section with its
  agent's own caveats
- [Pricing](pricing.md) — rates, and what reports unpriced
