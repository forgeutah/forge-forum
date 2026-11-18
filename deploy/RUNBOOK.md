# Forge Forum runbook

How to run forge-forum on its exe.dev VM and connect it to forge-proxy, Slack and Claude.

The forum is one Go binary under systemd. It keeps its data in SQLite at `/var/lib/forge-forum/forum.db` and reads its settings from `/etc/forge-forum.env`. Members never reach it directly: forge-proxy signs them in and forwards requests over the Forge tailnet to port 8080.

## Deploy or upgrade the binary

From a checkout, with Go installed:

```sh
FORUM_SSH="ssh forge-forum.exe.xyz" make deploy
```

`FORUM_SSH` is whatever command runs a shell command on the VM. The script builds a static linux/amd64 binary, installs it with the systemd unit, restarts the service, and checks `/healthz`. It creates `/etc/forge-forum.env` from the example on first run and never overwrites it afterwards.

## First-time setup

Each step fills one or more settings in `/etc/forge-forum.env`. Edit it with `sudoedit /etc/forge-forum.env`, then `sudo systemctl restart forge-forum`.

| Step | What to do | Setting it fills |
|---|---|---|
| 1 | Copy the shared proxy secret from forge-proxy's env file. | `PROXY_SECRET` |
| 2 | Copy the Slack workspace ID from forge-proxy's env file. | `SLACK_TEAM_ID` |
| 3 | Decide the forum's hostname, for example `forum.<base domain>`. | `PUBLIC_BASE_URL`, `AUTH_PORTAL_URL` |
| 4 | Join the VM to the Forge tailnet (below). | none |
| 5 | Add the forum to forge-proxy's `UPSTREAMS` and restart forge-proxy (below). | none, on the proxy VM |
| 6 | Point DNS for the forum's hostname at the forge-proxy VM. | none |
| 7 | Create the forum's Slack app (below). | `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`, `SLACK_ANNOUNCE_CHANNEL` |
| 8 | Create an Anthropic API key for the forum. | `ANTHROPIC_API_KEY` |

The forum starts with only steps 1 to 3 done. Without Slack tokens it runs with announcements, reply direct messages and "Move to forum" off. Without an Anthropic key only "Move to forum" is off.

### Step 4: join the tailnet

On the forum VM, with an auth key whose tag forge-proxy's ACL grant covers:

```sh
sudo tailscale up --authkey=<key> --hostname=forge-forum --advertise-tags=<tag>
```

Check from the forge-proxy VM that `curl http://forge-forum:8080/healthz` prints `ok`. The Tailscale ACL should allow only `tag:forge-proxy` to reach port 8080 on this node.

### Step 5: register with forge-proxy

On the forge-proxy VM, append the forum to `UPSTREAMS` in its env file and restart it:

```text
UPSTREAMS=<existing entries>;forum.<base domain>=http://forge-forum:8080
```

Add `|role1,role2` after the URL only if the forum should be limited to members holding one of those roles. With no role list, every signed-in workspace member gets in.

### Step 7: create the Slack app

1. At https://api.slack.com/apps choose "Create New App", "From a manifest", pick the Forge Utah workspace, and paste `deploy/slack-app-manifest.yml`.
2. Under "Basic Information", "App-Level Tokens", generate a token with the `connections:write` scope. That is `SLACK_APP_TOKEN` (starts `xapp-`).
3. Install the app to the workspace. The "Bot User OAuth Token" is `SLACK_BOT_TOKEN` (starts `xoxb-`).
4. Pick the channel that should hear about new threads, invite the bot to it (`/invite @forge forum`), and put its channel ID (starts `C`) in `SLACK_ANNOUNCE_CHANNEL`.

Keep the app internal to the workspace. A distributed app gets much lower Slack rate limits for reading threads.

## Check it works

Run these after first setup and after any change to the proxy, Slack app or keys.

1. `systemctl is-active forge-forum` prints `active`, and `curl http://127.0.0.1:8080/healthz` on the VM prints `ok`.
2. `curl -i http://127.0.0.1:8080/` on the VM returns 401. Nothing is served without the proxy secret.
3. Open the forum's hostname in a browser. You are sent to Slack sign-in and land on the feed under your Slack name.
4. Start a thread. It appears in the feed and is announced in the announcement channel.
5. From a second account, reply to that thread. The first account gets a direct message from the bot.
6. In a public Slack channel, open the message menu on a thread and choose "Move to forum". Confirm the title and tags. A forum thread appears with the original post and its direct replies, and the bot posts the link in the Slack thread.
7. Try "Move to forum" in a private channel. It is refused.

## Operations

- **Logs:** `sudo journalctl -u forge-forum -f`. Logs are JSON and identify members by forum member ID, never by email.
- **Moderators:** anyone whose forge-proxy roles include `ADMIN_ROLE` (default `admin`) can remove posts and manage tags. Grant it with `forge-proxy admin set-roles`.
- **Sign-out:** forge-proxy owns sessions. The forum's "sign out" link goes to `AUTH_PORTAL_URL`.
- **Secret rotation:** set `PROXY_SECRET=new,old` on the forum and restart it, roll the proxy to the new secret, then remove the old value.
- **Backups:** `deploy/litestream.yml` replicates the database to Cloudflare R2 the same way forge-proxy does. It is optional and not installed by `make deploy`.
- **Slack outages:** posts never wait on Slack. Undelivered messages retry with backoff and are marked failed after five attempts; look for `status=failed` rows in the `notifications` table.
- **Missed shortcuts:** if the forum is restarting when someone uses "Move to forum", Slack shows them an error. They can try again.
