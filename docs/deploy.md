# Deploying monitor-hub on the Pi

This runbook moves two services onto the hub: sei-monitor (the government process monitor) and diario-alerr (the gazette monitor). Each keeps its own database. The hub gets a new one. Run the steps in order and do not skip a check. Every command runs in `/opt/stacks/monitor-hub` unless it says otherwise.

Names in angle brackets, like `<postgres-container>`, are yours to fill in.

## The files

`compose.yaml` runs one service, `monitor-hub`. `.env.example` lists every variable the hub and both plugins read. Copy both to `/opt/stacks/monitor-hub/` and rename the second to `.env`.

The compose file differs from the old stacks in three limits, and each has a reason:

- Memory is 768m, not the 384m planned earlier. The gazette plugin downloads PDFs of up to 256 MiB into `/tmp`, and a tmpfs counts against the container's memory.
- `/tmp` is a 320m tmpfs: 256 MiB for the largest PDF plus room for the plugins' sockets. It is `noexec`, because sockets do not need exec.
- `pids_limit` is 256. The hub starts the plugin processes (and `pdftotext`), and threads count as pids.

The image's own healthcheck stays in force. The reverse-proxy labels are copied from diario-alerr's compose file (`caddy` and `caddy.reverse_proxy`) with `hub.lan` as the address.

## 1. Before you start

1. The three images must exist in the registry: `ghcr.io/itztas/monitor-hub`, and the sei-monitor and diario-alerr plugin images. At the time of writing `plugins.json` in the hub repository lists no plugins. The hub image has to be built after the two plugin images are published and listed there, otherwise it starts with no plugins.

2. Back up both databases. Find where each one runs first. The old compose files at `v1.0.0` keep Postgres inside each stack, but yours may use a shared server.

   ```bash
   docker ps --format '{{.Names}}\t{{.Image}}\t{{.Networks}}'
   docker exec <postgres-container> pg_dump -U sei_monitor -Fc sei_monitor > ~/sei_monitor-$(date +%F).dump
   docker exec <postgres-container> pg_dump -U diario_alerr -Fc diario_alerr > ~/diario_alerr-$(date +%F).dump
   ls -l ~/*.dump
   ```

   Check: both files are larger than a few kilobytes.

3. Write down the image each old stack runs, for the rollback.

   ```bash
   docker ps --format '{{.Names}}\t{{.Image}}' | grep -E 'sei-monitor|diario-alerr'
   docker image inspect --format '{{index .RepoDigests 0}}' <old-image>
   ```

   The Go versions are tagged `v1.0.0` in both repositories. The old sei-monitor compose uses the tag `latest`, so keep the digest too.

4. Pull the hub image now, so the downtime later is short.

   ```bash
   docker compose pull
   ```

## 2. Create the hub's database

Run this as the Postgres admin user on the server that will hold the hub's database. Use a real password and keep it for step 3.

```sql
CREATE ROLE monitor_hub LOGIN PASSWORD 'change-me';
CREATE DATABASE monitor_hub OWNER monitor_hub;
```

```bash
docker exec -i <postgres-container> psql -U <admin-user> -c '\l' | grep monitor_hub
```

Check: one line with `monitor_hub` as owner. The hub creates its own tables on first start.

## 3. Write `.env`

Copy `.env.example` to `.env` and fill it in. Both databases for the plugins are the existing ones, so nothing they already saw is announced again.

```bash
cp .env.example .env
chmod 600 .env
```

The hub has one Telegram channel, one ntfy channel and so on for both plugins. If the two old services used different bots, chats or topics, pick one for each channel now. Do not put `$` or quotes in values.

### From the old sei-monitor `.env`

| Old name | New name |
|---|---|
| `DATABASE_URL` | `SEI__DATABASE_URL` (same database) |
| `SEI_URL`, `SEI_API_URL`, `SEI_USERNAME`, `SEI_ORG`, `SEI_UNITS`, `SEI_TIMEZONE`, `SEI_CONCURRENCY` | the same name with `SEI__` in front |
| `SEI_PASSWORD`, `SEI_PASSWORD_FILE`, `SEI_PASSWORD_CMD` | `SEI__SEI_PASSWORD` and the same for the other two. Set one only. |
| `POLL_INTERVAL`, `NOTIFICATION_DETAIL`, `RETURN_WARNING_DAYS`, `RETENTION_DAYS` | the same name with `SEI__` in front |
| `LOG_LEVEL` | `LOG_LEVEL` for the hub and `SEI__LOG_LEVEL` for the plugin |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM`, `SMTP_TO`, `SMTP_TLS` | unchanged |
| `TELEGRAM_TOKEN`, `TELEGRAM_CHAT_ID` | unchanged |
| `NTFY_URL`, `NTFY_TOKEN` | unchanged |
| `WEBHOOK_URL`, `WEBHOOK_SECRET` | unchanged |
| `AMQP_URL` | unchanged |
| `AMQP_EXCHANGE` | unchanged name, new default `monitor-hub`. Set it to `sei-monitor` only if you want to keep the old exchange name. |
| `WHATSAPP_RECIPIENTS` | unchanged |
| `API_TOKEN`, `FEED_TOKEN`, `PUBLIC_URL`, `HTTP_ADDRESS` | unchanged, now read by the hub |
| `API_DETAIL`, `POSTGRES_PASSWORD` | not used. Leave them out. |

The plugin does not read `PATH`, and the hub never passes `SEI__PATH` on.

### From the old diario-alerr `.env`

| Old name | New name |
|---|---|
| `DATABASE_URL` | `DIARIO__DATABASE_URL` (same database) |
| `WATCH_TERMS`, `START_AFTER`, `CHECK_INTERVAL_MINUTES` | the same name with `DIARIO__` in front |
| `TELEGRAM_BOT_TOKEN` | `TELEGRAM_TOKEN` |
| `TELEGRAM_CHAT_ID` | unchanged |
| `NTFY_URL` and `NTFY_TOPIC` | one variable, `NTFY_URL`, which must end with the topic: the old URL, a slash, the old topic |
| `NTFY_TOKEN` | unchanged |
| `TZ` (in the old compose file) | `TZ` in `.env`, optional |
| `PUBLIC_HOST` | not used. `PUBLIC_URL` is optional. |
| `POSTGRES_PASSWORD`, `STATE_FILE` | not used |

`GAZETTE_API_URL` is new and optional: `DIARIO__GAZETTE_API_URL`. The plugin has a built-in default.

### New

`DATABASE_URL` (the hub's database from step 2), `API_TOKEN`, `FEED_TOKEN` (the feed is open to anyone if you leave it out), and the compose settings at the bottom of `.env.example`.

### If a database is not on the `postgres` network

The compose file joins the external network `postgres`. If the sei or diario database lives on another network, for example the one the old stack created, add that network to the service and to the `networks:` section of `compose.yaml` as `external: true`, and use the database container's name as the host in `SEI__DATABASE_URL` or `DIARIO__DATABASE_URL`. Fill this in once you have looked at `docker inspect <postgres-container>`.

Check the file before moving on:

```bash
docker compose config --quiet && echo ok
```

## 4. Stop the old stacks

Running old and new at once doubles every notification. Stop the apps but keep the databases and volumes.

If an old stack has its own Postgres service (the `v1.0.0` compose files do), `docker compose down` would stop that database too and the hub needs it. In that case stop only the app service:

```bash
cd /opt/stacks/sei-monitor && docker compose stop sei-monitor
cd /opt/stacks/diario-alerr && docker compose stop diario-alerr
```

If the databases live elsewhere, `docker compose down` in each folder is fine. Never add `-v`.

```bash
docker ps --format '{{.Names}}' | grep -E 'sei-monitor|diario-alerr'
```

Check: no app container is left. A Postgres container may remain.

## 5. Start the hub and check it

```bash
cd /opt/stacks/monitor-hub
docker compose up -d
docker compose logs --tail 50 monitor-hub
```

Check the log: a `monitor-hub starting` line listing both plugins and your channels. An `invalid configuration` error lists every problem at once. A plugin with bad settings exits, and the hub restarts it after 1 s, 5 s, 30 s, 2 min and then every 5 min. So look at `running` below, not only at whether the container is up.

Put your API token in a shell variable without writing it into history:

```bash
API_TOKEN=$(grep '^API_TOKEN=' .env | cut -d= -f2-)
```

1. Health, with no token:

   ```bash
   curl -s http://127.0.0.1:8090/healthz
   ```

   Expected: `{"ok":true}`.

2. Both plugins running:

   ```bash
   curl -s -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/plugins
   ```

   Expected: `{"items":[...]}` with `diario` first and `sei` second, each with `"running":true` and `"state"` set to `idle` or `checking`. The `sei` plugin may show another state until its first login finishes. A 401 means the token is wrong.

3. No notification arrives. The first read of each plugin is a baseline: the hub stores what is already there and announces nothing. Watch your channels for a few minutes. Two consequences: alerts the old gazette service had not yet delivered when you stopped it are absorbed and never announced, and the `sei` baseline waits for the plugin's first completed cycle (up to `POLL_INTERVAL`).

4. Ask the gazette plugin to check now:

   ```bash
   curl -s -X POST -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/plugins/diario/check
   ```

   Expected: `{"accepted":true,"message":"..."}`. A 404 means the plugin name is wrong, a 503 or 502 that the plugin is not reachable. `accepted:false` carries the plugin's reason in `message`.

5. The stored events and the feed:

   ```bash
   curl -s -H "Authorization: Bearer $API_TOKEN" 'http://127.0.0.1:8090/api/events?limit=5'
   curl -s "http://127.0.0.1:8090/feed.xml?token=<FEED_TOKEN>" | head -5
   ```

6. No RPC errors after a few poll rounds (the hub polls every 15 seconds):

   ```bash
   docker compose logs --since 10m monitor-hub | grep -iE 'error|warn'
   ```

   Expected: no output, or only lines you can explain. Leave `LOG_LEVEL=debug` off unless you are chasing something.

Finally open `http://hub.lan` if the proxy is set up.

## 6. Pair WhatsApp (only if you use it)

Skip this if `WHATSAPP_RECIPIENTS` is empty. Pair with the hub stopped, from the same image and `.env`:

```bash
docker compose stop monitor-hub
docker compose run --rm monitor-hub whatsapp-pair 5500000000000
```

Replace the number with the phone that will send the messages, with country and area code and digits only. The command prints `Pairing code: <code>`. On that phone open WhatsApp, Linked devices, Link a device, then "Link with phone number instead", and type the code. You have 5 minutes. On success it prints `Paired. Restart monitor-hub so it starts sending WhatsApp messages.`

```bash
docker compose up -d
docker compose logs --tail 20 monitor-hub | grep -i whatsapp
```

Check: no "not paired" warning. If you paired while the hub was running, restart it with `docker compose restart monitor-hub`. A database that already has a paired number refuses a second pairing; the error text says how to unlink first.

## 7. Roll back

Use this if the hub misbehaves and you want the old services back.

1. Stop the hub:

   ```bash
   cd /opt/stacks/monitor-hub && docker compose down
   ```

2. Gazette database. The hub already delivered the alerts the plugin stored, but the old Go program tracks delivery in its own `alert_deliveries` table (channels `telegram` and `ntfy`), which the hub never writes. Started as is, the old program announces every stored alert again. Mark them delivered first with the SQL file in the gazette repository, `sql/rollback/mark-alerts-delivered.sql`, and run it against the diario database before you start the old stack. That file did not exist in the repository when this was written. If it is still missing, see the gazette repository's rollback SQL, and do not start the old stack until you have one. The statement it needs is a one-row-per-alert insert of each alert for each channel the old program uses. The schema is in `sql/schema/00001_monitor.sql` of that repository. I did not run such a statement against a database.

3. Schema. The gazette plugin added one column, `monitor_state.acked_seq`, with default `0`. The old program ignores it, and the gazette ledger records a test that runs the `v1.0.0` program against a migrated database. The hub's database is not touched by the old services and can stay.

4. SEI database. The plugin adds no table or column. In the design, the plugin keeps its acknowledged position in memory only and writes no delivery rows, and the old program creates its delivery rows when it saves an event. So events stored while the hub ran are not announced by the old program. Two things follow. Deliveries the old program still had pending when you stopped it in step 4 are sent after the rollback. And the plugin prunes old events by `RETENTION_DAYS`, as the old program did, so what it deleted stays deleted. The design does not spell out a rollback for SEI, and the Rust plugin's repository code was not in the tree when I wrote this, so check one thing after rollback: no burst of old notifications in the first minutes. If you see one, stop the sei stack again and ask before restarting it.

5. Start the old stacks at the tags or digests you noted in step 1:

   ```bash
   cd /opt/stacks/sei-monitor && docker compose up -d
   cd /opt/stacks/diario-alerr && docker compose up -d
   ```

   Check each with its old `/healthz` and logs.

## 8. Operating rules

- **Resetting a plugin's database.** Do it only together with the hub's stored position for that plugin, and with the hub stopped. Reset alone, the plugin's numbering restarts below the hub's position, new alerts are never delivered, and they are pruned unread. Reset the hub's side too (the table and columns are from `sql/schema/00001_hub.sql`; I have not run these):

  ```sql
  DELETE FROM events WHERE plugin = 'diario';
  UPDATE plugins SET cursor = 0, baselined_at = NULL WHERE name = 'diario';
  ```

  The delete is needed because the hub refuses to store an event id it already has. The next read is a new baseline, so nothing is announced.

- **No retention in the hub.** The hub never deletes events or deliveries. Rows for a channel that is never paired or never works pile up. Prune them by hand if the database grows.
- **AMQP changed.** The exchange is `monitor-hub`, the routing key is `<plugin>.<kind>` for one event and `<plugin>.batch` for several, and the body is `{"events":[...]}` instead of one bare event per message. Delivery is at-least-once, and each message carries a message id derived from its body so a consumer can drop a repeat. Rebind consumers of the old exchange.
- **Gazette client limits.** The plugin stops with a clear error on a gazette listing of more than 100 pages, a listing page over 4 MiB, a PDF over 256 MiB, or more than 64 MiB of text from one PDF. The 320m tmpfs is sized for the PDF limit.
- **What the SEI service lost.** Its web page (`/ui/`) and its domain routes (`/processes`, `/status` with SEI details) have no replacement yet. The API is the hub's: `/api/plugins`, `/api/plugins/{name}/check`, `/api/events`, with a different response shape and the header `Authorization: Bearer <API_TOKEN>`. The feed moved from `/feed/<FEED_TOKEN>.xml` to `/feed.xml?token=<FEED_TOKEN>`, optionally with `&plugin=<name>`.
- **A plugin that never starts.** If `running` stays false, the plugin's configuration error is in the hub log. A misspelled `SEI__` or `DIARIO__` name is not an error: the plugin never sees it and uses the default or reports the variable missing.
- **Passwords from files.** `_FILE` variables need the file inside the container. The compose file mounts none, so add a volume before using one.
