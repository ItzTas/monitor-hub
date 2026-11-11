# Deploying monitor-hub on the Pi

This runbook moves two services onto the hub: sei-monitor (the government process monitor) and diario-alerr (the gazette monitor). Each keeps its own database. The hub gets a new one. Run the steps in order and do not skip a check. Every command runs in `/opt/stacks/monitor-hub` unless it says otherwise.

Placeholders are written in angle brackets and are yours to fill in:

- `<postgres-container>`: the name of the shared Postgres container, from `docker ps`.
- `<hub-db-password>`: the password you choose for the hub's database role.
- `<old-stack-network>`: the Docker network of an old stack, only when its database is still bundled in that stack (see step 1).
- `<plugin>`: `sei` or `diario`.
- `<FEED_TOKEN>`: the value in your `.env`.

## The files

`compose.yaml` runs one service, `monitor-hub`. `.env.example` lists every variable the hub and both plugins read. Copy both to `/opt/stacks/monitor-hub/` and rename the second to `.env`.

The compose file sets three limits. They are estimates, not measurements, and the reasons are:

- `mem_limit: 768m`. The gazette plugin downloads PDFs of up to 256 MiB into `/tmp`, and a tmpfs counts against the container's memory.
- `/tmp` is a 320m tmpfs: room for one PDF at the plugin's 256 MiB limit, plus the plugins' sockets. The plugin scans two editions at once, so two PDFs that large do not fit together. Real editions are a few megabytes. Holding two at the limit would take 512m of the 768m memory limit and leave 256m for the hub, both plugins and two `pdftotext` runs, so the tmpfs stays at one. It is `noexec` because sockets do not need exec.
- `pids_limit: 256`. The hub starts the plugin processes and `pdftotext`, and threads count as pids.

Watch them for a few days with `docker stats monitor-hub`. If the container is killed during a gazette check, raise the memory limit first. If a PDF download fails with `No space left on device`, raise the tmpfs size and the memory limit by the same amount. The pids limit should not need to change.

The image's own healthcheck stays in force. The reverse-proxy labels (`caddy` and `caddy.reverse_proxy`) and the `x-arcane` block follow the gazette's old compose file, with `hub.lan` as the address.

## 1. Before you start

### Pin the old stacks to their Go images

Do this first, for two reasons.

- Both old compose files follow the tag `latest`. When a Rust version reaches the default branch of its repository, CI publishes it as `latest`. The Rust image is a plugin and does not run on its own: started alone, it prints that the hub did not launch it and exits at once, and `restart: unless-stopped` turns that into a restart loop while nothing watches the gazette or SEI. Anything that pulls on the board would swap the working program for that loop.
- A rollback must start the Go programs, so the old stacks have to point at the Go images.

For diario-alerr this sets the order of the next two moves. The Rust version is released as `2.0.0`, but its pull request into `main`, https://github.com/ItzTas/diario-alerr/pull/1, is left open on purpose, because merging it moves `latest`. Pin first, then merge. The same holds for sei-monitor once its Rust version is released: its old compose file names `:latest` directly.

The Go release `v1.0.0` of each repository published its image as `ghcr.io/itztas/<name>:1.0.0` (the tag has no `v`).

1. sei-monitor: its compose file has a literal image line. Change it in `/opt/stacks/sei-monitor/compose.yaml`:

   ```yaml
   image: ghcr.io/itztas/sei-monitor:1.0.0
   ```

2. diario-alerr: its compose file reads the tag from a variable. Add this line to `/opt/stacks/diario-alerr/.env`:

   ```
   DIARIO_ALERR_TAG=1.0.0
   ```

   The files on your host may differ from the `v1.0.0` ones: edit the image line or variable you actually have.

3. Restart each stack and check the image:

   ```bash
   cd /opt/stacks/sei-monitor && docker compose pull && docker compose up -d
   cd /opt/stacks/diario-alerr && docker compose pull && docker compose up -d
   docker ps --format '{{.Names}}\t{{.Image}}' | grep -E 'sei-monitor|diario-alerr'
   ```

   Check: both app containers show a `:1.0.0` image. Also note the image digest of each, in case a tag is ever overwritten:

   ```bash
   docker image inspect --format '{{index .RepoDigests 0}}' ghcr.io/itztas/sei-monitor:1.0.0
   ```

4. Only now merge https://github.com/ItzTas/diario-alerr/pull/1. Once its CI run has finished, pull again on the board:

   ```bash
   cd /opt/stacks/diario-alerr && docker compose pull && docker compose up -d
   docker ps --format '{{.Names}}\t{{.Image}}' | grep diario-alerr
   ```

   Check: the container still shows `ghcr.io/itztas/diario-alerr:1.0.0` and stays up.

### The images for the hub

The hub image carries the plugins listed in `plugins.json` in the hub repository. Today that file lists one, `diario`, at `ghcr.io/itztas/diario-alerr:2.0.0`, and the published `ghcr.io/itztas/monitor-hub:latest` bundles it. The `sei` entry is added when the sei-monitor plugin image is published, and the next hub image then carries both.

Pull the image now so the downtime later is short, and list the plugins inside it:

```bash
docker compose pull
docker run --rm --entrypoint ls ghcr.io/itztas/monitor-hub:latest /opt/monitor-hub/plugins
```

Check: the list shows `diario` and `sei`. While it shows only `diario`, the hub cannot take over sei-monitor yet.

### Where the databases are

These steps assume the shared Postgres on the external Docker network `postgres`. Check that it holds:

```bash
docker network ls --filter name=^postgres$
docker network inspect postgres --format '{{range .Containers}}{{.Name}}{{"\n"}}{{end}}'
```

Check: the first command lists a network named `postgres`, and the second lists the shared Postgres container (that name is your `<postgres-container>`) and, if both old databases are on it, nothing else you did not expect. `docker compose config` does not catch a missing external network: the hub would fail at `up` with "network postgres not found".

If a database still lives in an old stack's own bundled Postgres (the `v1.0.0` compose files of both repositories bundle one), the hub must join that stack's network too:

1. Find the network. It is named after the stack's folder, for example `sei-monitor_default`:

   ```bash
   docker network ls
   ```

2. In `compose.yaml`, uncomment the two `<old-stack-network>` lines and put that network's name in both. Add one such pair per stack that needs it.
3. Both old stacks name their database service `postgres`, so the alias `postgres` is ambiguous. In the database URL use the container name of that stack's Postgres, from `docker ps`.
4. Do not use `docker compose down` on that stack in step 4: stop only its app service.

### Back up both databases

```bash
docker exec <postgres-container> pg_dump -U sei_monitor -Fc sei_monitor > ~/sei_monitor-$(date +%F).dump
docker exec <postgres-container> pg_dump -U diario_alerr -Fc diario_alerr > ~/diario_alerr-$(date +%F).dump
ls -l ~/*.dump
```

Use the role and database names of your stacks. Check: both files are larger than a few kilobytes.

## 2. Create the hub's database

Run this in the shared Postgres as its admin user (`postgres` by default). Use a real password and keep it for step 3.

```bash
docker exec -i <postgres-container> psql -U postgres <<'SQL'
CREATE ROLE monitor_hub LOGIN PASSWORD '<hub-db-password>';
CREATE DATABASE monitor_hub OWNER monitor_hub;
SQL
docker exec -i <postgres-container> psql -U postgres -c '\l' | grep monitor_hub
```

Check: one line with `monitor_hub` as owner. The hub creates its own tables on first start.

## 3. Write `.env`

Copy `.env.example` to `.env` and fill it in.

```bash
cp .env.example .env
chmod 600 .env
```

Do not put `$` or quotes in values.

### The two plugin database URLs

Both point at the existing databases, so nothing they already saw is announced again. The old `.env` files may not hold a ready `DATABASE_URL`.

- `SEI__DATABASE_URL`: the old sei-monitor compose file builds it from `POSTGRES_PASSWORD`. Its shape is
  `postgres://sei_monitor:<password>@<host>:5432/sei_monitor?sslmode=disable`, with user and database `sei_monitor` and host `postgres` in the `v1.0.0` file.
- `DIARIO__DATABASE_URL`: diario-alerr keeps `DATABASE_URL` in its `.env`. The `v1.0.0` example is
  `postgres://diario_alerr:<password>@postgres:5432/diario_alerr?sslmode=disable`.

Take the real user, password, host and database from the running stack's own compose file and `.env` on the host. A password with special characters must be percent-encoded in the URL. The host is `postgres` on the shared network and the container name for a bundled database (see step 1).

The gazette plugin is stricter about its database than the Go program was. A URL and a role that worked for Go can fail here:

- The role in `DIARIO__DATABASE_URL` must own the tables and the sequence `alerts_seq_seq`. On its first start the plugin applies a migration that alters a table, and every `Events` call reads that sequence. This matters if the database was ever moved by dump and restore under another role.
- Connect straight to Postgres. A pooler in transaction mode breaks the session lock the plugin holds and refuses the `options` start-up parameter it sends.
- The plugin connects without TLS. Use `sslmode=disable` or `sslmode=prefer` and no other TLS parameter. `sslmode=verify-full`, or a pgx pool parameter such as `pool_max_conns`, makes it exit with `DATABASE_URL is not a valid Postgres address`.
- The role must have no `idle_session_timeout`. The plugin keeps one connection idle for as long as it runs, to hold its lock. If the server closes it, the plugin exits and the hub restarts it, over and over.

Check the role:

```bash
docker exec <postgres-container> psql -U diario_alerr -d diario_alerr -c '\dt' -c '\ds' -c 'SHOW idle_session_timeout'
```

Check: the `Owner` of every table and of `alerts_seq_seq` is the role in your URL, and `idle_session_timeout` is `0`. Use your own role and database names.

### From the old sei-monitor `.env`

| Old name | New name |
|---|---|
| `DATABASE_URL` (or built from `POSTGRES_PASSWORD`) | `SEI__DATABASE_URL`, as above |
| `SEI_URL`, `SEI_API_URL`, `SEI_USERNAME`, `SEI_ORG`, `SEI_UNITS`, `SEI_TIMEZONE`, `SEI_CONCURRENCY` | the same name with `SEI__` in front |
| `SEI_PASSWORD`, `SEI_PASSWORD_FILE`, `SEI_PASSWORD_CMD` | `SEI__SEI_PASSWORD` and the same for the other two. Set one only. |
| `POLL_INTERVAL`, `NOTIFICATION_DETAIL`, `RETURN_WARNING_DAYS`, `RETENTION_DAYS` | the same name with `SEI__` in front |
| `LOG_LEVEL` | `LOG_LEVEL` for the hub and `SEI__LOG_LEVEL` for the plugin |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM`, `SMTP_TO`, `SMTP_TLS` | unchanged |
| `TELEGRAM_TOKEN`, `TELEGRAM_CHAT_ID` | unchanged |
| `NTFY_URL`, `NTFY_TOKEN` | unchanged |
| `WEBHOOK_URL`, `WEBHOOK_SECRET` | unchanged |
| `AMQP_URL` | unchanged |
| `AMQP_EXCHANGE` | unchanged name, new default `monitor-hub`. Set it to `sei-monitor` only to keep the old exchange name. |
| `WHATSAPP_RECIPIENTS` | unchanged |
| `API_TOKEN`, `FEED_TOKEN`, `PUBLIC_URL` | unchanged. The old value carries over if it was set. |
| `HTTP_ADDRESS` | do not copy. Leave it unset: the hub must listen on all interfaces inside the container. An old `127.0.0.1:8080` leaves the published port dead while the healthcheck passes. |
| `API_DETAIL`, `POSTGRES_PASSWORD` | not used. Leave them out. |

The sei plugin reads `PATH` to find the password command, and it receives the hub's `PATH`. A `SEI__PATH` variable is dropped by the hub.

### From the old diario-alerr `.env`

| Old name | New name |
|---|---|
| `DATABASE_URL` | `DIARIO__DATABASE_URL`, as above |
| `WATCH_TERMS`, `START_AFTER`, `CHECK_INTERVAL_MINUTES` | the same name with `DIARIO__` in front |
| `GAZETTE_API_URL` | `DIARIO__GAZETTE_API_URL`, optional. The plugin has a built-in default. |
| `TELEGRAM_BOT_TOKEN` | `TELEGRAM_TOKEN` |
| `TELEGRAM_CHAT_ID` | unchanged |
| `NTFY_URL` and `NTFY_TOPIC` | one variable, `NTFY_URL`, which must end with the topic: the old URL, a slash, the old topic. When `NTFY_TOPIC` was unset the old topic was `diario-alerr`. |
| `NTFY_TOKEN` | unchanged |
| `TZ` (in the old compose file) | `TZ` in `.env`, optional |
| `PUBLIC_HOST`, `PUBLIC_URL` | `PUBLIC_URL` is optional in the hub (the feed's link). `PUBLIC_HOST` is not used. |
| `HTTP_PORT`, `TELEGRAM_API_URL`, `STATE_FILE`, `POSTGRES_PASSWORD` | gone. The hub has no equivalent. |

Compose settings changed too:

| Old | New |
|---|---|
| `APP_PORT`, default 8087 | `APP_PORT`, default 8090 |
| `BIND_ADDRESS`, default 127.0.0.1 | the same |
| `DIARIO_ALERR_TAG` | `MONITOR_HUB_TAG` |

The hub has one Telegram channel, one ntfy channel and so on for both plugins. If the two old services used different bots, chats or topics, pick one for each channel now.

### New

`DATABASE_URL` (the hub's database from step 2, with `<hub-db-password>`), `API_TOKEN`, `FEED_TOKEN` (the feed is open to anyone if you leave it out), and the compose settings at the bottom of `.env.example`.

Check the file before moving on:

```bash
docker compose config --quiet && echo ok
docker network inspect postgres --format '{{.Name}}'
```

The second command prints `postgres`. Run `docker network inspect` for every extra network you added as well.

## 4. Stop the old stacks

Running old and new at once doubles every notification. Stop the apps but keep the databases and volumes. Both must be stopped before the hub starts in step 5. The gazette plugin takes a lock in its database that keeps out a second copy of the plugin, but the Go program never takes that lock: with both running, every gazette alert is announced twice.

First make sure the old gazette program has nothing left to send. The hub never announces an alert that Go stored and did not deliver (step 5 explains why). Look at its log for the last check interval, 30 minutes unless you set `CHECK_INTERVAL_MINUTES`:

```bash
docker logs --since 35m diario-alerr 2>&1 | grep 'delivery failed'
```

Check: no output. A `delivery failed, retrying next cycle` line means an alert is still waiting for that channel. Fix the channel and wait for the next cycle, or note the alert and pass it on yourself. Use the container name from `docker ps` if yours differs.

If an old stack has its own Postgres service, `docker compose down` would stop that database too and the hub needs it. In that case stop only the app service:

```bash
cd /opt/stacks/sei-monitor && docker compose stop sei-monitor
cd /opt/stacks/diario-alerr && docker compose stop diario-alerr
```

If the databases live on the shared Postgres, `docker compose down` in each folder is fine. Never add `-v`.

```bash
docker ps --format '{{.Names}}' | grep -E 'sei-monitor|diario-alerr'
```

Check: no app container is left. A Postgres container may remain.

## 5. Start the hub and check it

The hub listens on port 8090 on the host, or on your `APP_PORT`. The commands below use 8090.

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

2. Both plugins running. The first poll happens up to 15 seconds after start, so a plugin may show `unknown` at first. Repeat after half a minute.

   ```bash
   curl -s -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/plugins
   ```

   Expected: `{"items":[...]}` with `diario` first and `sei` second, each with `"running":true` and `"state"` set to `idle` or `checking`. The `sei` plugin may show another state until its first login finishes. A 401 means the token is wrong.

3. The first read of each plugin is a baseline: the hub stores what the plugin already holds and announces none of it. That read comes on the hub's first poll after the plugin is up, within about 15 seconds. Watch your channels for a few minutes.
   - The gazette plugin holds back the alerts its own first cycle records until that read has ended. They reach the hub on the next poll and are announced as usual. So a notification in the first minute is normal when an edition the Go program had not scanned yet matches.
   - Alerts the Go program had stored and not delivered are part of the baseline, and nobody announces them. Step 4 looked for them before the stop, and item 4 below looks again.
   - On a new gazette database, not this take-over, the welcome and the first cycle's alerts are announced on the hub's second read. The take-over sends no welcome, because the database already has its state.
   - The `sei` baseline waits for the plugin's first completed cycle (up to `POLL_INTERVAL`).

   Do not restart the hub or the stack until the hub has logged the end of the gazette baseline (next item). A restart before that line can make the baseline take in the first cycle's alerts too.

4. The end of the gazette baseline, and what nobody announced:

   ```bash
   docker compose logs monitor-hub | grep 'msg="baseline complete" plugin=diario'
   ```

   Expected: one line, usually within a minute of the start. Wait one more minute after it, then look in the plugin's database for alerts that nobody was told about:

   ```bash
   docker exec -i <postgres-container> psql -U diario_alerr -d diario_alerr <<'SQL'
   SELECT a.seq, a.id, a.title, a.link, a.published_at, s.seen_at,
          (SELECT string_agg(d.channel, ',' ORDER BY d.channel)
             FROM alert_deliveries d WHERE d.alert_id = a.id) AS sent_by_go
   FROM alerts a
   LEFT JOIN seen_editions s ON a.id = 'edition-' || s.edition_id
   WHERE a.seq <= (SELECT acked_seq FROM monitor_state)
   ORDER BY a.seq DESC
   LIMIT 20;
   SQL
   ```

   Each row is an alert the hub has confirmed it stored. `seen_at` is when the edition was recorded (empty for the welcome), and `sent_by_go` lists the channels the Go program delivered it on.
   - A row recorded before you stopped the Go container, with `sent_by_go` empty or missing a channel the old stack used, was announced by nobody. Read it there and pass it on yourself.
   - A row recorded after the hub started comes from the plugin's first cycle and went out through the hub.
   - If the hub or the stack restarted before the `baseline complete` line, run the query anyway: the rows recorded around the restart may have been taken in by the baseline. Look for each of them in your channels.

   Both `seen_at` and the hub's log times carry their UTC offset, so compare them with that in mind. Use your own role and database names.

5. The PDF extractor in the image:

   ```bash
   docker exec monitor-hub pdftotext -v
   ```

   Expected: a `pdftotext version` line. The gazette plugin runs the same check before it starts and refuses to start without it. The hub log then shows a `fatal` line with `plugin=diario` and an `error` that starts with `checking pdftotext:`, followed by `plugin failed to start`, and `/api/plugins` shows `diario` with `"running":false`.

6. Ask the gazette plugin to check now. A whole cycle also proves that the arm64 build runs on the board: it resolves and reaches the gazette over TLS, reads the listing and writes to its database.

   ```bash
   curl -s -X POST -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/plugins/diario/check
   ```

   Expected: `{"accepted":true,"message":"..."}`. A 404 means the plugin name is wrong, a 503 or 502 that the plugin is not reachable. `accepted:false` carries the plugin's reason in `message`. With `a check is already running`, the cycle the plugin starts with has not ended: ask again in a minute.

   Then read the plugin list after half a minute:

   ```bash
   curl -s -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/plugins
   ```

   Expected for `diario`: `"state":"idle"` and a `last_success_at` later than the moment you asked (the API gives times in UTC). `checking` means the cycle is still running, so repeat. `failing` gives the reason in `message`.

7. The stored events and the feed:

   ```bash
   curl -s -H "Authorization: Bearer $API_TOKEN" 'http://127.0.0.1:8090/api/events?limit=5'
   curl -s "http://127.0.0.1:8090/feed.xml?token=<FEED_TOKEN>" | head -5
   ```

8. No RPC errors after a few poll rounds (the hub polls every 15 seconds):

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

Use this if the hub misbehaves and you want the old services back. The old stacks are already pinned to their Go images (step 1).

1. See what the hub has not delivered yet. Nobody sends these after the rollback: the old programs do not read the hub's tables.

   ```bash
   docker exec -i <postgres-container> psql -U monitor_hub -d monitor_hub -c "SELECT e.plugin, d.channel, d.status, count(*) FROM deliveries d JOIN events e ON e.seq = d.event_seq WHERE d.status = 'pending' GROUP BY 1, 2, 3 ORDER BY 1, 2;"
   ```

   Each row is a plugin, a channel and how many notifications are still waiting. Read the events behind them with `/api/events` if you need them.

2. Stop the hub:

   ```bash
   cd /opt/stacks/monitor-hub && docker compose down
   ```

3. Gazette database. The old Go program announces every stored alert that has no row in `alert_deliveries` for one of its channels, `telegram` and `ntfy`. The plugin never writes that table, so every alert it stored would be announced again. Before you start the old gazette stack, run the plugin's rollback script, `sql/rollback/mark-alerts-delivered.sql` in the diario-alerr repository at tag `v2.0.0`:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/ItzTas/diario-alerr/v2.0.0/sql/rollback/mark-alerts-delivered.sql -o ~/mark-alerts-delivered.sql
   cat ~/mark-alerts-delivered.sql
   docker exec -i <postgres-container> psql -U diario_alerr -d diario_alerr < ~/mark-alerts-delivered.sql
   ```

   Check: `cat` shows a single `INSERT INTO alert_deliveries` statement, and `psql` answers `INSERT 0` followed by a count. Use the role and database names of your stack.

   What it costs: the script marks every stored alert as delivered on both channels, including any alert the hub had stored and not delivered yet (step 1 lists those). After the rollback nobody sends them. Rows for a channel the old stack never enabled are ignored by the old program, and the script is safe to run twice. The plugin's test `go_sends_nothing_from_the_rust_era_after_rollback` (`tests/go_compat.rs`) runs it and then the Go `v1.0.0` program on the same database. It has not been run against your database.

4. Schema. The gazette plugin added one column, `monitor_state.acked_seq`, with default `0`. The old program ignores it: the plugin's `tests/go_compat.rs` starts the `v1.0.0` program on a migrated database. The hub's database is not touched by the old services and can stay.

5. SEI database. The plugin adds no table or column. In the design, the plugin keeps its acknowledged position in memory only and writes no delivery rows, and the old program creates its delivery rows when it saves an event (`internal/monitor/repository.go` at `v1.0.0`). So events stored while the hub ran are not announced by the old program. Two things follow. Deliveries the old program still had pending when you stopped it in step 4 are sent after the rollback. And the plugin prunes old events by `RETENTION_DAYS`, as the old program did, so what it deleted stays deleted. This has not been tested end to end: after the rollback, watch for a burst of old notifications in the first minutes. If you see one, stop the sei stack again before it sends more.

6. Start the old stacks:

   ```bash
   cd /opt/stacks/sei-monitor && docker compose up -d
   cd /opt/stacks/diario-alerr && docker compose up -d
   ```

   Check each with its old `/healthz` and its logs.

Coming forward again after a rollback is a new take-over. Before the hub starts, reset its stored position for each plugin you move back, as the next section describes. Otherwise the hub resumes from a position that no longer matches the plugin's database. For the gazette, every alert the Go program stored in the meantime has a higher `seq` than the hub's old position, so the hub would announce all of them again, though Go already sent them. After the reset, follow steps 4 and 5 again, including the query in step 5.

## 8. Operating rules

- **Resetting a plugin's database.** Do it only together with the hub's stored position for that plugin, and with the hub stopped. Reset alone, the plugin's numbering restarts below the hub's position, and its new alerts up to that position are never delivered. The gazette plugin tells you when this happens: it logs `the hub is ahead of this database: reset the hub's position for this plugin` at `error` level once per process, and `/api/plugins` shows `diario` as `failing` with the same text in `message`. Until the two agree it returns no events and confirms none, so it deletes none of them either. Reset the hub's side too. This deletes the hub's stored events for that plugin, so back up the hub's database first. The table and columns are from `sql/schema/00001_hub.sql`.

  ```bash
  docker exec -i <postgres-container> psql -U monitor_hub -d monitor_hub <<'SQL'
  DELETE FROM events WHERE plugin = '<plugin>';
  UPDATE plugins SET cursor = 0, baselined_at = NULL WHERE name = '<plugin>';
  SQL
  ```

  The delete is needed because the hub silently drops an event whose id it already stored for that plugin, and a new database reuses ids such as `welcome`. The next read is a new baseline: what the plugin held when it started is stored and not announced. On a new gazette database the welcome and the first cycle's alerts are announced on the hub's second read, about 15 seconds later. This statement was run on a throwaway copy of the hub's schema, not on a production database.

- **No retention in the hub.** The hub never deletes events or deliveries. Rows for a channel that is never paired or never works pile up. Prune them by hand if the database grows.
- **Nobody is told when a plugin is failing.** The hub sends no notification about a plugin's state. A `failing` plugin shows only in `/api/plugins`, with the reason in `message`, and in the plugin's own lines in the hub log. Look at it now and then:

  ```bash
  curl -s -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/plugins
  docker compose logs --since 24h monitor-hub | grep 'plugin=diario' | grep -E 'level=(WARN|ERROR)'
  ```

- **Gazette client limits.** The plugin stops with a clear error on a gazette listing of more than 100 pages, a listing page over 4 MiB, a PDF over 256 MiB, or more than 64 MiB of text from one PDF.
- **An edition that can never be read.** One edition the plugin cannot read, such as a broken PDF or one over the limits above, stops every later edition from being recorded, as in the Go program, and each cycle downloads them all again. `diario` stays `failing` with `could not read edition <title>: ...`. To get past it, read that edition yourself, then mark it as seen with its numeric id. The id is the `id` the gazette's media API gives for that title: the address is `DEFAULT_GAZETTE_API_URL` in `src/config.rs` of the diario-alerr repository, and adding `?mime_type=application/pdf&_fields=id,date_gmt,title&after=<day before>T00:00:00` lists the editions published after that time.

  ```bash
  docker exec <postgres-container> psql -U diario_alerr -d diario_alerr -c 'INSERT INTO seen_editions (edition_id) VALUES (<id>);'
  ```

  Then ask for a check (step 5) and expect `idle` with a fresh `last_success_at`.
- **Two scans at once, and `/tmp`.** The plugin scans two editions at a time. Each download is a file named `edition-<six letters or digits>.pdf` in `/tmp` until `pdftotext` has read it. The 320m tmpfs holds one PDF at the 256 MiB limit, not two (see "The files"). A plugin process that is killed mid-scan leaves its file behind, and the plugin deletes such files the next time it starts.
- **Two behaviors kept from the Go program.** An edition whose `date_gmt` is in the future moves the plugin's cursor there for good. After that the listing comes back empty, every cycle succeeds, and no edition is seen until you move the cursor back by hand:

  ```bash
  docker exec <postgres-container> psql -U diario_alerr -d diario_alerr -c 'SELECT cursor_at FROM monitor_state;'
  docker exec <postgres-container> psql -U diario_alerr -d diario_alerr -c "UPDATE monitor_state SET cursor_at = '<publication time of the last real edition>';"
  ```

  A `cursor_at` in the future means this happened. The other one: a PDF without a text layer, such as a scan saved as images, reads as "no match", with no alert and no error.
- **A plugin that never starts.** If `running` stays false, the plugin's configuration error is in the hub log. A misspelled `SEI__` or `DIARIO__` name is not an error: the plugin never sees it and uses the default or reports the variable missing.
- **Secrets from files.** `_FILE` variables need the file inside the container. The compose file mounts none, so add a volume before using one.

## 9. What changed for you

- **AMQP.** The exchange is `monitor-hub`, the routing key is `<plugin>.<kind>` for one event and `<plugin>.batch` for several, and the body is `{"events":[...]}` instead of one bare event per message. Delivery is at-least-once, and each message carries a message id derived from its body so a consumer can drop a repeat. Rebind consumers of the old exchange.
- **The SEI web page and routes.** The SEI service's web page (`/ui/`) and its domain routes (`/processes`, `/status` with SEI details) have no replacement yet. The API is the hub's: `/api/plugins`, `/api/plugins/{name}/check`, `/api/events`, with a different response shape and the header `Authorization: Bearer <API_TOKEN>`.
- **Feeds.** The old gazette service served its own public `/feed.xml`. The old SEI service served `/feed/<FEED_TOKEN>.xml`. The hub serves one feed for all plugins at `/feed.xml?token=<FEED_TOKEN>` (the token is needed when `FEED_TOKEN` is set), filtered with `&plugin=diario` or `&plugin=sei`. Subscribers of the old addresses must change them.

## 10. Known issues

The hub is built with the Go version pinned in the repository. That version carries two standard-library advisories, GO-2026-6859 and GO-2026-6809, and the hub's dependencies carry two more that its code does not call (`golang.org/x/net` 0.61.0 and `golang.org/x/crypto` 0.58.0). Raising the pinned Go version is the owner's decision.
