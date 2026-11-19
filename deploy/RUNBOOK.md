# Forge Forum runbook

How to run forge-forum on its exe.dev VM and connect it to forge-proxy, forge-bot and Claude.

The forum is one Go binary under systemd. It keeps its data in SQLite at `/var/lib/forge-forum/forum.db` and reads its settings from `/etc/forge-forum.env`. Members never reach it directly: forge-proxy signs them in and forwards requests over the Forge tailnet to port 8080. The forum holds no Slack token: forge-bot, the service that connects to Slack for every Forge service, forwards Slack interactions to the forum and passes the forum's Slack API calls on, also over the tailnet.

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
| 7 | Connect the forum to forge-bot (below). | `FORGE_BOT_URL`, `FORGE_BOT_TOKEN`, `SLACK_ANNOUNCE_CHANNEL` |
| 8 | Create an Anthropic API key for the forum. | `ANTHROPIC_API_KEY` |

The forum starts with only steps 1 to 3 done. Without the two forge-bot settings it runs with announcements, reply direct messages and "Move to forum" off. Without an Anthropic key only "Move to forum" is off.

### Step 4: join the tailnet

On the forum VM, with an auth key whose tag forge-proxy's ACL grant covers:

```sh
sudo tailscale up --authkey=<key> --hostname=forge-forum --advertise-tags=<tag>
```

Check from the forge-proxy VM that `curl http://forge-forum:8080/healthz` prints `ok`. The Tailscale ACL should allow only forge-proxy and forge-bot to reach port 8080 on this node, and must allow this node to reach port 8080 on forge-bot (step 7).

### Step 5: register with forge-proxy

On the forge-proxy VM, append the forum to `UPSTREAMS` in its env file and restart it:

```text
UPSTREAMS=<existing entries>;forum.<base domain>=http://forge-forum:8080
```

Add `|role1,role2` after the URL only if the forum should be limited to members holding one of those roles. With no role list, every signed-in workspace member gets in.

### Step 7: connect to forge-bot

The Slack app belongs to forge-bot. The forum is one of the services registered in it.

1. Deploy forge-bot by its own runbook, in the forge-bot repository. Its Slack app needs a message shortcut named "Move to forum" with the callback ID `move_to_forum`, and these bot scopes for what the forum does: `commands`, `chat:write`, `im:write`, `channels:history`, `channels:read`, `channels:join` and `users:read`.
2. Generate the forum's service token, at least 32 characters: `openssl rand -hex 32`.
3. On the forge-bot VM, add the forum to `SERVICES` and `SERVICE_TOKENS` in the bot's env file and restart the bot. Entries are separated by `;`:

   ```text
   SERVICES=<existing entries>;forum=http://forge-forum:8080/internal/slack/interactions|move_to_forum,move_to_forum_submit
   SERVICE_TOKENS=<existing entries>;forum=<the service token>
   ```

4. On the forum VM, set `FORGE_BOT_URL=http://forge-bot:8080` (no trailing slash) and `FORGE_BOT_TOKEN` to the same service token. Set both or neither.
5. Check the tailnet lets the two reach each other on port 8080. From the forum VM, `curl http://forge-bot:8080/healthz` prints `ok`. From the forge-bot VM, `curl http://forge-forum:8080/healthz` prints `ok`.
6. Pick the channel that should hear about new threads, invite the bot to it, and put its channel ID (starts `C`) in `SLACK_ANNOUNCE_CHANNEL`.
7. Restart the forum: `sudo systemctl restart forge-forum`.

## Check it works

Run these after first setup and after any change to the proxy, forge-bot or keys.

1. `systemctl is-active forge-forum` prints `active`, and `curl http://127.0.0.1:8080/healthz` on the VM prints `ok`.
2. `curl -i http://127.0.0.1:8080/` on the VM returns 401. Nothing is served without the proxy secret.
3. `sudo journalctl -u forge-forum -n 20` shows a `listening` line with `"slack":true,"move":true`. `false` means a forge-bot setting or the Anthropic key is missing.
4. `curl -i -X POST http://127.0.0.1:8080/internal/slack/interactions` on the VM returns 401: the endpoint is there and refuses a caller without the service token. A 404 means "Move to forum" is off.
5. Open the forum's hostname in a browser. You are sent to Slack sign-in and land on the feed under your Slack name.
6. Start a thread. It appears in the feed and is announced in the announcement channel.
7. From a second account, reply to that thread. The first account gets a direct message from the bot.
8. In a public Slack channel, open the message menu on a thread and choose "Move to forum". Confirm the title and tags. A forum thread appears with the original post and its direct replies, and the bot posts the link in the Slack thread.
9. Try "Move to forum" in a private channel. It is refused.

## Operations

- **Logs:** `sudo journalctl -u forge-forum -f`. Logs are JSON and identify members by forum member ID, never by email.
- **Moderators:** anyone whose forge-proxy roles include `ADMIN_ROLE` (default `admin`) can remove posts and manage tags. Grant it with `forge-proxy admin set-roles`.
- **Sign-out:** forge-proxy owns sessions. The forum's "sign out" link goes to `AUTH_PORTAL_URL`.
- **Secret rotation:** set `PROXY_SECRET=new,old` on the forum and restart it, roll the proxy to the new secret, then remove the old value.
- **Backups:** `deploy/litestream.yml` replicates the database to Cloudflare R2 the same way forge-proxy does. It is optional and not installed by `make deploy`.
- **Service token rotation:** generate a new token, set it as the forum's entry in forge-bot's `SERVICE_TOKENS` and as `FORGE_BOT_TOKEN` on the forum, then restart both. Until both have restarted, Slack messages wait in the outbox and "Move to forum" does not work.
- **Slack or forge-bot outages:** posts never wait on Slack. Undelivered messages retry with backoff and are marked failed after five attempts; look for `status=failed` rows in the `notifications` table. "Move to forum" needs forge-bot running.
- **Slack calls failing:** a 401 from forge-bot in the forum's log means `FORGE_BOT_TOKEN` does not match the bot's `SERVICE_TOKENS` entry. A connection error means the tailnet does not let the forum reach forge-bot on port 8080.
- **"Move to forum" does nothing:** check forge-bot's log first. If the bot reports a 401 from the forum, the tokens differ; a 404 means the forum is running with "Move to forum" off; a connection error means the tailnet does not let the bot reach the forum on port 8080.
- **Missed shortcuts:** if the forum is restarting when someone uses "Move to forum", forge-bot tells them it did not work. They can try again.
