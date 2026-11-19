# forge-forum

The Forge Utah forum: a members-only place for long-form discussion, with replies that nest under what they answer and rise by upvotes. It is one Go binary with a SQLite database, and it runs behind [forge-proxy](https://github.com/forgeutah/forge-proxy).

## How it fits together

- **Sign-in is forge-proxy's job.** Every member request must carry the proxy's shared secret and `X-Forge-*` identity headers. Anything else gets a 401. Two routes sit outside that check: `GET /healthz`, and `POST /internal/slack/interactions`, which only forge-bot may call.
- **Members are keyed on Slack user ID.** That lets the forum credit the author of an imported Slack message who has never opened the forum.
- **Slack is the front door.** With Slack configured, the forum announces new threads in a channel, sends a direct message when someone replies to you, and offers a "Move to forum" action on Slack threads. It does all of this through forge-bot, the service that holds Forge's Slack connection and bot token. The forum holds no Slack token and needs no public URL.
- **forge-bot and the forum share one service token.** The bot posts the "Move to forum" shortcut and its dialog to the forum's interactions endpoint with that token as a bearer token. The forum calls Slack's Web API at the bot's `/slack/api/<method>` with the same token, and the bot swaps in the real one. Both directions run over the Forge tailnet, as set out in forge-bot's service contract (`docs/service-contract.md` in the forge-bot repository).
- **"Move to forum" uses Claude** to pick out which Slack replies answer the original post directly. Those are imported; the rest stay in Slack.

Without the forge-bot settings (`FORGE_BOT_URL` and `FORGE_BOT_TOKEN`) the forum still works, with those three features off. The decisions behind all of this are in [docs/plans/2026-11-17-001-feat-forge-forum-plan.md](docs/plans/2026-11-17-001-feat-forge-forum-plan.md).

## Run it locally

You need Go 1.26 or newer.

```sh
make check        # build, vet, and test
```

To click around without forge-proxy, start the server with a throwaway secret and send the identity headers yourself:

```sh
PROXY_SECRET=local-development-secret-0123456789 \
SLACK_TEAM_ID=TLOCAL \
PUBLIC_BASE_URL=http://localhost:8080 \
go run ./cmd/forge-forum
```

```sh
curl -H 'X-Forge-Proxy-Secret: local-development-secret-0123456789' \
     -H 'X-Forge-Contract-Version: 1' \
     -H 'X-Forge-User-Id: 1' \
     -H 'X-Forge-Name: Ada' \
     -H 'X-Forge-Roles: admin' \
     -H 'X-Forge-Slack-User-Id: U0LOCAL' \
     -H 'X-Forge-Slack-Team-Id: TLOCAL' \
     http://localhost:8080/
```

A browser extension that adds request headers does the same job for clicking through pages.

## Configuration

Settings come from environment variables only. [deploy/forge-forum.env.example](deploy/forge-forum.env.example) lists every one with a comment. `PROXY_SECRET`, `SLACK_TEAM_ID` and `PUBLIC_BASE_URL` are required; the forge-bot and Anthropic settings are optional.

## Layout

| Path | What it holds |
|---|---|
| `cmd/forge-forum` | `main` and the wiring of every package |
| `internal/config` | environment loading and validation |
| `internal/db` | SQLite pools and the migration runner; `migrations/*.sql` is the schema |
| `internal/identity` | the forge-proxy contract check and member records |
| `internal/forum` | threads, replies, votes, tags, the reply tree, Slack import |
| `internal/markdown` | Markdown rendering and Slack text conversion |
| `internal/notify` | the outbox of Slack messages and the worker that delivers them |
| `internal/slackx` | the Slack client (which calls Slack through forge-bot), its test fake, and the endpoint forge-bot posts interactions to |
| `internal/move` | "Move to forum": dialog, background job, reply classifier |
| `internal/web` | HTTP handlers, templates, and CSS |
| `deploy` | systemd unit, env example, deploy script, runbook |

To change the schema, add a new numbered file under `internal/db/migrations/`. Migrations are applied in filename order at startup and are forward-only.

## Contributing

Forge Forum takes issues, not code. Nobody outside the owners opens pull requests, and pull requests that do not come from the process below are closed without review. If you want something added, changed, or fixed, describe it well in an issue and an AI agent does the building.

How a change moves:

| Step | What happens | Label on the issue |
|---|---|---|
| 1. Propose | You open an issue with one of the forms. The forms ask for the problem, what a member should see and do, a walk-through, and a checklist of when it is done. | `proposed` |
| 2. Accept | An owner reads it and accepts or declines it. Owners may ask you to sharpen it first. | `accepted` or `declined` |
| 3. Design | An agent produces the UI and UX design for it in the Forge design system and posts it on the issue. Owners and the person who asked comment there. | `design-review` |
| 4. Approve the design | An owner approves the design. No code is written before this. | `design-approved` |
| 5. Build | An agent builds it against the approved design and the issue's checklist, with tests, and opens a pull request linked to the issue. | `building` |
| 6. Merge | An owner reviews the pull request and merges it. The issue closes. | closed |

Changes with nothing to look at, such as a bug fix or a speed-up, skip steps 3 and 4.

What makes an issue buildable:

- **Say what a member sees and does**, not how to code it. The agent decides the how.
- **Give a real example** of the problem.
- **Write the "done when" list so anyone could check it** without reading code.
- **Keep it to one thing.** Two ideas are two issues.
- **Say what is out.** It keeps the design and the build small.

Questions and half-formed ideas belong on [the forum](https://forum.forgeut.dev) or in Forge Utah Slack until they are ready to be an issue.

## Deploying

[deploy/RUNBOOK.md](deploy/RUNBOOK.md) covers deploying to the exe.dev VM, registering the forum with forge-proxy and forge-bot, and the checks to run afterwards.
