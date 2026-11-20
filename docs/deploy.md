# Deploying monitor-hub on the Pi

This runbook moves two services onto the hub: sei-monitor (the government process monitor) and diario-alerr (the gazette monitor). Each keeps its own database. The hub gets a new one. After the take-over, "Add the anhanguera monitor" sets up a third monitor, which replaces no old service. Run the steps in order and do not skip a check. Every command runs in `/opt/stacks/monitor-hub` unless it says otherwise.

Placeholders are written in angle brackets and are yours to fill in:

- `<postgres-container>`: the name of the Postgres container that holds the database a command names, from `docker ps`. On the shared Postgres that is one container for all the databases. If the SEI database still lives in the sei-monitor stack's own Postgres, commands on `sei_monitor` use that container instead (see step 1).
- `<hub-db-password>`: the password you choose for the hub's database role.
- `<anhanguera-db-password>`: the password you choose for the anhanguera monitor's database role.
- `<old-stack-network>`: the Docker network of the sei-monitor stack, only when its database is still bundled in that stack (see step 1).
- `<monitor>`: the monitor's name, such as `sei` or `diario`.
- `<FEED_TOKEN>`: the value in your `.env`.

## The files

`compose.yaml` runs one service, `monitor-hub`. `.env.example` lists every variable the hub and its monitors read. Copy both to `/opt/stacks/monitor-hub/` and rename the second to `.env`.

The compose file sets three limits. They are estimates, not measurements, and the reasons are:

- `mem_limit: 768m`. The gazette monitor downloads PDFs of up to 256 MiB into `/tmp`, and a tmpfs counts against the container's memory.
- `/tmp` is a 320m tmpfs: room for one PDF at the gazette monitor's 256 MiB limit, plus the monitors' sockets. The gazette monitor scans two editions at once, so two PDFs that large do not fit together. Real editions are a few megabytes. Holding two at the limit would take 512m of the 768m memory limit and leave 256m for the hub, the monitors and two `pdftotext` runs, so the tmpfs stays at one. It is `noexec` because sockets do not need exec.
- `pids_limit: 256`. The hub starts the monitor processes and `pdftotext`, and threads count as pids.

Watch them for a few days with `docker stats monitor-hub`. If the container is killed during a gazette check, raise the memory limit first. If a PDF download fails with `No space left on device`, raise the tmpfs size and the memory limit by the same amount. The pids limit should not need to change.

The image's own healthcheck stays in force. The reverse-proxy labels (`caddy` and `caddy.reverse_proxy`) and the `x-arcane` block follow the gazette's old compose file, with `hub.lan` as the address.

## 1. Before you start

### Pin the old stacks to their Go images

Do this first, for two reasons.

- Both old compose files follow the tag `latest`. When a Rust version reaches the default branch of its repository, CI publishes it as `latest`. The Rust image is a monitor for the hub and does not run on its own: started alone, it prints that the hub did not launch it and exits at once, and `restart: unless-stopped` turns that into a restart loop while nothing watches the gazette or SEI. Anything that pulls on the board would swap the working program for that loop.
- A rollback must start the Go programs, so the old stacks have to point at the Go images.

Both Rust versions are released: the gazette monitor as `ghcr.io/itztas/diario-alerr:2.0.1` and the SEI monitor as `ghcr.io/itztas/sei-monitor:2.0.0`. Their pull requests into `main`, https://github.com/ItzTas/diario-alerr/pull/1 and https://github.com/ItzTas/sei-monitor/pull/1, are left open on purpose, because merging either one moves `latest` to the Rust monitor. So pin first, then merge, and keep the pins afterwards: the way back needs the Go images. The old gazette compose file reads its tag from `DIARIO_ALERR_TAG` and falls back to `latest`. The old SEI compose file names `ghcr.io/itztas/sei-monitor:latest` directly.

The Go images to pin:

- sei-monitor: `ghcr.io/itztas/sei-monitor:1.0.0`, the Go release `v1.0.0` (the tag has no `v`). On 2026-11-17 `latest` was the same image.
- diario-alerr: `ghcr.io/itztas/diario-alerr:sha-764b330`, the Go program built from `main` after the `1.0.0` release. On 2026-11-17 `latest` was the same image. Its program differs from `1.0.0` only in how it matches `WATCH_TERMS`: it carries the two matching fixes (names with middle names between the words, and registration numbers in any formatting), and the monitor matches the same way (step 3). Its database code is the same as in `1.0.0`. Pinned to `1.0.0` instead, the gazette would match the old way until the switch and again after a rollback, and miss the alerts those fixes find.

1. sei-monitor: its compose file has a literal image line. Change it in `/opt/stacks/sei-monitor/compose.yaml`:

   ```yaml
   image: ghcr.io/itztas/sei-monitor:1.0.0
   ```

2. diario-alerr: its compose file reads the tag from a variable. Add this line to `/opt/stacks/diario-alerr/.env`:

   ```
   DIARIO_ALERR_TAG=sha-764b330
   ```

   The files on your host may differ from the ones in the repositories: edit the image line or variable you actually have.

3. Restart each stack and check the image:

   ```bash
   cd /opt/stacks/sei-monitor && docker compose pull sei-monitor && docker compose up -d sei-monitor
   cd /opt/stacks/diario-alerr && docker compose pull && docker compose up -d
   docker ps --format '{{.Names}}\t{{.Image}}' | grep -E 'sei-monitor|diario-alerr'
   ```

   Pull and start only the `sei-monitor` service: if the stack still bundles its database, a plain `pull` and `up -d` would also pull a newer `postgres:18-alpine` and could recreate the database container.

   Check: the SEI app container shows `ghcr.io/itztas/sei-monitor:1.0.0` and the gazette one `ghcr.io/itztas/diario-alerr:sha-764b330`. Also note the image digest of each, in case a tag is ever overwritten:

   ```bash
   docker image inspect --format '{{index .RepoDigests 0}}' ghcr.io/itztas/sei-monitor:1.0.0
   docker image inspect --format '{{index .RepoDigests 0}}' ghcr.io/itztas/diario-alerr:sha-764b330
   ```

   To pin the content itself and not only the tag, add the digest after the tag: `image: ghcr.io/itztas/sei-monitor:1.0.0@sha256:<digest>` and `DIARIO_ALERR_TAG=sha-764b330@sha256:<digest>`, with the `sha256:` part each command printed. Docker then pulls and runs that exact image, even if a rebuild later puts another one under the same tag. After you add a digest, run the commands of item 3 again: the pin takes effect only when the stack starts again, and the image column then shows the tag followed by the digest.

4. Only now merge the two pull requests. Once their CI runs have finished, pull again on the board:

   ```bash
   cd /opt/stacks/sei-monitor && docker compose pull sei-monitor && docker compose up -d sei-monitor
   cd /opt/stacks/diario-alerr && docker compose pull && docker compose up -d
   docker ps --format '{{.Names}}\t{{.Image}}' | grep -E 'sei-monitor|diario-alerr'
   ```

   Check: both containers still show the images of item 3 and stay up.

### The images for the hub

The hub image carries the monitors listed in `monitors.json` in the hub repository: `diario` from `ghcr.io/itztas/diario-alerr:2.3.0` and `sei` from `ghcr.io/itztas/sei-monitor:2.3.0`. Each `ghcr.io/itztas/monitor-hub` image built from that file holds both, under `/opt/monitor-hub/monitors/diario/` and `/opt/monitor-hub/monitors/sei/`.

CI tags releases from Conventional Commits on every push to `main`, in three separate streams: `vX.Y.Z` for the hub image, `sdk-go/vX.Y.Z` for the Go SDK and `sdk-rust/vX.Y.Z` for the Rust SDK. Each stream looks only at commits that touch its own files, so an SDK-only commit does not release the hub. A `feat` is a minor release, `fix` and `perf` are patches, and any breaking change (`!` or a `BREAKING CHANGE:` footer) is a major one. Other types, such as `docs`, `chore`, `ci` and `test`, release nothing. Every tag gets a GitHub Release with its changelog. A tag pushed with `GITHUB_TOKEN` starts no new run, so the image is built in the same run that created the hub tag and gets the `X.Y.Z` and `X.Y` tags. SDK tags build no image. A `v*` tag pushed by hand still publishes its version tags.

Pull the image now so the downtime later is short, and list the monitors inside it:

```bash
docker compose pull
docker run --rm --entrypoint ls ghcr.io/itztas/monitor-hub:latest /opt/monitor-hub/monitors
```

Check: the list shows both `diario` and `sei`. If `ls` cannot find `/opt/monitor-hub/monitors`, or `sei` is missing, the image is too old: stop here, leave both old stacks running, and pull again after the hub's CI has published a newer image. Step 4 stops sei-monitor too, and nothing in an image without `sei` would replace it, so SEI would go unwatched. If you set `MONITOR_HUB_TAG`, use that tag in the command instead of `latest`.

### Where the databases are

The gazette's stack already uses the shared Postgres: its compose file joins the external Docker network `postgres` and reaches the server as `postgres:5432`, with the role and database `diario_alerr`, and it bundles no database of its own. The hub joins the same network and keeps its own database there (step 2). Check that the network holds:

```bash
docker network ls --filter name=^postgres$
docker network inspect postgres --format '{{range .Containers}}{{.Name}}{{"\n"}}{{end}}'
```

Check: the first command lists a network named `postgres`, and the second lists the shared Postgres container (that name is your `<postgres-container>`), `diario-alerr`, the sei-monitor app if its database is on the shared server, and nothing else you did not expect. `docker compose config` does not catch a missing external network: the hub would fail at `up` with "network postgres not found".

The SEI database is either on the shared Postgres too, or still in the sei-monitor stack's own Postgres (the `v1.0.0` compose file bundles one). In the second case the hub must join that stack's network as well:

1. Find the network. It is named after the stack's folder, for example `sei-monitor_default`:

   ```bash
   docker network ls
   ```

2. In `compose.yaml`, uncomment the two `<old-stack-network>` lines and put that network's name in both.
3. That stack names its database service `postgres`, which is also the name of the shared server on the `postgres` network, so the host name `postgres` is ambiguous. In `SEI__DATABASE_URL` use the container name of the stack's Postgres, from `docker ps`, and use that container as `<postgres-container>` for every command on `sei_monitor`. In `DATABASE_URL` and `DIARIO__DATABASE_URL` use the shared Postgres container's name instead of `postgres` too: on both networks the name can resolve to the SEI stack's server, which has no `monitor_hub` or `diario_alerr` role.
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

The copy also holds the `anhanguera` block. Delete it from `.env` for now: "Add the anhanguera monitor", after step 5, sets that monitor up once the take-over is done.

### The monitor database URLs

`SEI__DATABASE_URL` and `DIARIO__DATABASE_URL` point at the existing databases, so nothing the old services already saw is announced again. The old `.env` files may not hold a ready `DATABASE_URL`.

- `SEI__DATABASE_URL`: the old sei-monitor compose file builds it from `POSTGRES_PASSWORD`. Its shape is
  `postgres://sei_monitor:<password>@<host>:5432/sei_monitor?sslmode=disable`, with user and database `sei_monitor` and host `postgres` in the `v1.0.0` file.
- `DIARIO__DATABASE_URL`: diario-alerr keeps `DATABASE_URL` in its `.env`. The example in its repository is
  `postgres://diario_alerr:<password>@postgres:5432/diario_alerr?sslmode=disable`.

Take the real user, password, host and database from the running stack's own compose file and `.env` on the host. A password with special characters must be percent-encoded in the URL. The host is `postgres` on the shared network and the container name for a bundled database. If the hub also joins the sei-monitor stack's network, use container names for all three URLs, the hub's `DATABASE_URL` included (see step 1).

Both monitors are stricter about their database than the Go programs were. A URL and a role that worked for Go can fail here:

- The role must own the tables and the sequence the monitor reads: `alerts_seq_seq` for the gazette, `events_id_seq` for SEI. On its first start the gazette monitor applies a migration that alters a table, and every `Events` call of either monitor reads its sequence. This matters if a database was ever moved by dump and restore under another role.
- Connect straight to Postgres. A pooler in transaction mode breaks the session lock each monitor holds, and both send an `options` start-up parameter that such a pooler refuses.
- The URL must carry the role's password. Both monitors refuse a URL without one, or with an empty one, with `DATABASE_URL has no password: add the role's password to the URL`. If the database trusts the role without a password, put any password there; Postgres ignores it.
- The monitors connect without TLS. `sslmode=disable`, `allow` and `prefer` work. `require`, `verify-ca` and `verify-full` fail at once.
- Both refuse several hosts, a URL with no host, and any query key they do not know with `DATABASE_URL is not a valid Postgres address`, which never repeats the URL. diario also refuses the key=value form (`host=db user=diario`) and any scheme other than `postgres://` and `postgresql://`. The libpq keys that 2.1 accepted, such as `connect_timeout` and `keepalives`, are accepted and ignored.
- An `idle_session_timeout` on the role no longer makes a monitor exit. The lock connection is queried every 0.5 s, and pool connections are tested before each use. A monitor still exits if its lock connection closes or stops answering (30 s for sei, 10 s for diario), so do not run `pg_terminate_backend` on it or put a firewall in between that cuts long connections. For sei, each restart logs in to SEI once, up to 12 times an hour.

Check each role:

```bash
docker exec <postgres-container> psql -U diario_alerr -d diario_alerr -c '\dt' -c '\ds'
docker exec <postgres-container> psql -U sei_monitor -d sei_monitor -c '\dt' -c '\ds'
```

Check: the `Owner` of every table and of `alerts_seq_seq` or `events_id_seq` is the role in that URL. Use your own role and database names.

### From the old sei-monitor `.env`

| Old name | New name |
|---|---|
| `DATABASE_URL` (or built from `POSTGRES_PASSWORD`) | `SEI__DATABASE_URL`, as above |
| `SEI_URL`, `SEI_USERNAME`, `SEI_ORG` | the same name with `SEI__` in front, and the same values, character for character. Together they identify the account in the database. A change starts a new account, whose first check only takes note of what is there and reports nothing. |
| `SEI_API_URL`, `SEI_UNITS`, `SEI_TIMEZONE`, `SEI_CONCURRENCY` | the same name with `SEI__` in front |
| `SEI_PASSWORD`, `SEI_PASSWORD_FILE`, `SEI_PASSWORD_CMD` | `SEI__SEI_PASSWORD` and the same for the other two. Set one only. A password with `$` or quotes can reach the monitor changed by the way `.env` is read, which costs one rejected login and a pause: use `SEI__SEI_PASSWORD_FILE` for it. A file or a command needs more than a variable (section 8, "Secrets from files"). |
| `POLL_INTERVAL`, `NOTIFICATION_DETAIL`, `RETURN_WARNING_DAYS`, `RETENTION_DAYS` | the same name with `SEI__` in front |
| `LOG_LEVEL` | `LOG_LEVEL` for the hub and `SEI__LOG_LEVEL` for the monitor. Keep the hub's at `info` or `debug` for the take-over: step 5 waits for `info` lines, which `warn` or `error` hide. |
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

The sei monitor reads `PATH` to find the password command, and it receives the hub's `PATH`. A `SEI__PATH` variable is dropped by the hub.

### From the old diario-alerr `.env`

| Old name | New name |
|---|---|
| `DATABASE_URL` | `DIARIO__DATABASE_URL`, as above |
| `WATCH_TERMS` | `DIARIO__WATCH_TERMS`, same value. See below for how the monitor matches. |
| `START_AFTER`, `CHECK_INTERVAL_MINUTES` | the same name with `DIARIO__` in front |
| `GAZETTE_API_URL` | `DIARIO__GAZETTE_API_URL`, optional. The monitor has a built-in default. |
| `TELEGRAM_BOT_TOKEN` | `TELEGRAM_TOKEN` |
| `TELEGRAM_CHAT_ID` | unchanged |
| `NTFY_URL` and `NTFY_TOPIC` | one variable, `NTFY_URL`, which must end with the topic: the old URL, a slash, the old topic. When `NTFY_TOPIC` was unset the old topic was `diario-alerr`. |
| `NTFY_TOKEN` | unchanged |
| `TZ` (in the old compose file) | `TZ` in `.env`, optional |
| `PUBLIC_HOST`, `PUBLIC_URL` | `PUBLIC_URL` is optional in the hub (the feed's link). `PUBLIC_HOST` is not used. |
| `HTTP_PORT`, `TELEGRAM_API_URL`, `STATE_FILE`, `POSTGRES_PASSWORD` | gone. The hub has no equivalent. |

The monitor matches `WATCH_TERMS` the way the Go image pinned in step 1 does. Matching ignores case, accents and repeated whitespace. A term with a digit is a registration number: leading zeros and one `.`, `-`, `/` or space between characters are ignored, so `0034712k` also matches `34.712-K` and `34712/K`, but not `134712-K`. Any other term is a name: its words match in order with up to four words of letters between them, so `Paulo Santos` also matches `PAULO HENRIQUE DE OLIVEIRA SANTOS`, but a number or a comma in between stops it. A term too long to match stops the monitor at start with `WATCH_TERMS term <n> (<size>) is too long to match: shorten it`, where `<n>` counts from 1. A name reaches that limit at 32 words.

Compose settings changed too:

| Old | New |
|---|---|
| `APP_PORT`, default 8087 | `APP_PORT`, default 8090 |
| `BIND_ADDRESS`, default 127.0.0.1 | the same |
| `DIARIO_ALERR_TAG` | `MONITOR_HUB_TAG` |

The hub has one Telegram channel, one ntfy channel and so on for all its monitors. If the two old services used different bots, chats or topics, pick one for each channel now.

### New

`DATABASE_URL` (the hub's database from step 2, with `<hub-db-password>`), `API_TOKEN`, `FEED_TOKEN` (the feed is open to anyone if you leave it out), and the compose settings at the bottom of `.env.example`.

Check the file before moving on:

```bash
docker compose config --quiet && echo ok
docker network inspect postgres --format '{{.Name}}'
```

The second command prints `postgres`. Run `docker network inspect` for the sei-monitor stack's network as well if you added it in step 1.

## 4. Stop the old stacks

Running old and new at once doubles every notification. Stop the apps but keep the databases and volumes. Both must be stopped before the hub starts in step 5. Each monitor takes a lock in its database that keeps out a second copy of the monitor, but the Go programs never take that lock: with the gazette running twice every alert is announced twice, and with SEI two monitors fight over the same account.

### The SEI program

The SEI monitor continues the database as Go left it: the same account, no new baseline, no repeated events. Check these first.

1. Let the Go program send everything it has. It sends pending notifications every 30 seconds and retries a failed one after 1 minute, 5 minutes, 30 minutes, 2 hours and 6 hours. This must return `0`:

   ```bash
   docker exec <postgres-container> psql -U sei_monitor -d sei_monitor -c "SELECT count(*) FROM deliveries WHERE status = 'pending';"
   ```

   A notification still pending at the switch is never sent while the monitor runs: the hub's first read takes its event as history, and the monitor never reads the Go program's `deliveries` table. If a channel never delivers (an unpaired WhatsApp number, for example), its rows stay pending: pair it, or accept that they go out only after a rollback.

   A delivery that fails six times in a row, about 8.6 hours after the first try, is marked `failed` and drops out of that count. Neither Go nor the monitor ever sends it. List the ones from the last day and pass them on yourself:

   ```bash
   docker exec <postgres-container> psql -U sei_monitor -d sei_monitor -c "SELECT e.id, e.kind, e.payload::jsonb->>'process_number' AS process, e.detected_at, d.channel, d.last_error FROM deliveries d JOIN events e ON e.id = d.event_id WHERE d.status = 'failed' AND e.detected_at > now() - interval '1 day' ORDER BY e.id;"
   ```

2. Check that the account is not paused by a rejected login, and note how many accounts the database holds:

   ```bash
   docker exec <postgres-container> psql -U sei_monitor -d sei_monitor -c 'SELECT paused_reason FROM accounts;' -c 'SELECT count(*) FROM accounts;'
   ```

   Check: every `paused_reason` is empty. Note the count for step 5. It is usually 1, more if `SEI_URL`, `SEI_USERNAME` or `SEI_ORG` ever changed. With a reason there, the monitor starts held and watches nothing until you request a check (section 8, "When SEI refuses the login"). It says so once, in an event the hub announces on its second read. Fix the password now, and after step 5 request a check as that section describes.

3. Make sure `.env` has the same `SEI_URL`, `SEI_USERNAME` and `SEI_ORG` as the old `.env`, with the `SEI__` prefix, and that `SEI__DATABASE_URL` points at this database (step 3).

The Go program keeps checking SEI until it stops. An event it records after item 1 may still be pending when it stops, and nobody sends it. So once item 1 returns `0`, do the rest of this step without a pause: the gazette check, the stop, the query of item 1 once more on the stopped program's database, then step 5. If that second count is not `0`, list what nobody will send and pass it on yourself:

```bash
docker exec <postgres-container> psql -U sei_monitor -d sei_monitor -c "SELECT e.id, e.kind, e.payload::jsonb->>'process_number' AS process, e.detected_at, d.channel FROM deliveries d JOIN events e ON e.id = d.event_id WHERE d.status = 'pending' ORDER BY e.id;"
```

### The gazette program

First make sure the old gazette program has nothing left to send. The hub never announces an alert that Go stored and did not deliver (step 5 explains why). Look at its log for the last check interval plus a few minutes. The command below uses 35 minutes, for the default interval of 30: if you set `CHECK_INTERVAL_MINUTES`, use that value plus 5 instead.

```bash
docker logs --since 35m diario-alerr 2>&1 | grep 'delivery failed'
```

Check: no output. A `delivery failed, retrying next cycle` line means an alert is still waiting for that channel. Fix the channel and wait for the next cycle, or note the alert and pass it on yourself. Use the container name from `docker ps` if yours differs.

### Stop both

Stop only the app services. If the sei-monitor stack still has its own Postgres service, `docker compose down` would stop that database too, and the hub needs it. For the gazette, `stop` keeps the container, so Docker still knows when it stopped (below). Never use `down -v`.

```bash
cd /opt/stacks/sei-monitor && docker compose stop sei-monitor
cd /opt/stacks/diario-alerr && docker compose stop diario-alerr
docker ps --format '{{.Names}}' | grep -E 'sei-monitor|diario-alerr'
```

Check: no app container is left. The sei-monitor stack's Postgres container may remain.

From now on, never start an old app while the hub runs, neither by hand nor from a stack manager. The stopped containers still exist, and `docker compose up -d` in an old folder starts the Go program next to the monitors: every gazette alert goes out twice, and two monitors work the same SEI account. Only the rollback (section 7) starts them again, after the hub has stopped.

Note the time each Go container stopped. Step 5 compares them with what the monitors record. Docker keeps them, in UTC:

```bash
docker inspect -f '{{.State.FinishedAt}}' diario-alerr
docker inspect -f '{{.State.FinishedAt}}' "$(cd /opt/stacks/sei-monitor && docker compose ps -aq sei-monitor)"
```

Now run the pending count of "The SEI program" once more, list the rows if it is not `0`, and go on to step 5.

## 5. Start the hub and check it

The hub listens on port 8090 on the host, or on your `APP_PORT`. The commands below use 8090.

If an older hub used the `data` volume, move each monitor's directory from `<DATA_DIR>/plugins/<name>` to `<DATA_DIR>/monitors/<name>` before you start this release; the hub does not move it for you. diario, sei and anhanguera keep nothing there, so this matters only for a monitor of your own.

```bash
cd /opt/stacks/monitor-hub
docker compose up -d
docker compose logs --tail 50 monitor-hub
```

Check the log: a `monitor-hub starting` line listing both monitors and your channels (an `info` line, so `LOG_LEVEL` must be `info` or `debug`, see step 3). An `invalid configuration` error lists every problem at once. A monitor with bad settings exits, and the hub restarts it after 1 s, 5 s, 30 s, 2 min and then every 5 min. So look at `running` below, not only at whether the container is up.

Put your API token in a shell variable without writing it into history:

```bash
API_TOKEN=$(grep '^API_TOKEN=' .env | cut -d= -f2-)
```

1. Health, with no token:

   ```bash
   curl -s http://127.0.0.1:8090/healthz
   ```

   Expected: `{"ok":true}`.

2. Both monitors running. The first poll happens up to 15 seconds after start, so a monitor may show `unknown` at first. Repeat after half a minute.

   ```bash
   curl -s -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/monitors
   ```

   Expected: `{"items":[...]}` with `diario` first and `sei` second, each with `"running":true`. `diario` shows `idle` or `checking`. `sei` shows `failing` with `sei-monitor has not reached SEI yet` until it has reached SEI, then `checking` during its first check and `idle` after it. `sei` as `paused` means SEI refused the login (section 8, "When SEI refuses the login"). A 401 means the token is wrong.

3. The first read of each monitor is a baseline: the hub stores what the monitor already holds and announces none of it. Watch your channels for a few minutes.
   - The gazette's first read comes on the hub's first poll after the monitor is up, usually within about 15 seconds, later while the other monitor's poll is slow.
   - The SEI monitor answers the hub's reads only once it has reached SEI: it loads the login page and finds its account, without logging in. Until then the hub logs a `poll a monitor` warning for `monitor=sei` on every poll. Its first read comes on the hub's first poll after that. While SEI does not answer, the monitor keeps trying, and the baseline waits.
   - Both monitors hold back what their own first check records until that read has ended. Those events reach the hub on the next poll and are announced as usual. So a notification in the first minutes is normal: a gazette edition the Go program had not scanned yet that matches, or a SEI change since the Go program's last check.
   - Gazette alerts the Go program had stored and not delivered are part of the baseline, and nobody announces them. Step 4 looked for them before the stop, and item 4 below looks again. For SEI, step 4 made sure there were none.
   - On a new gazette database, not this take-over, the welcome and the first cycle's alerts are announced on the hub's second read. The take-over sends no welcome, because the database already has its state.

   Do not restart the hub or the stack until the hub has logged `baseline complete` for both monitors (next item). A restart before that line can make the baseline take in what the monitor's first check recorded, and then nobody announces it. The same happens when the hub itself restarts a monitor after it crashed. The hub logs that as `monitor exited` or `monitor failed to start`:

   ```bash
   docker compose logs monitor-hub | grep -E 'msg="(monitor exited|monitor failed to start)" monitor=(diario|sei)'
   ```

   If you must restart the hub before both lines, save its log first, because item 4 looks for lines from before the restart:

   ```bash
   docker compose logs monitor-hub > ~/hub-before-restart.log
   ```

   Then restart it the way the change needs. `docker compose restart monitor-hub` keeps the container and its log. After a change to `.env` you need `docker compose up -d`, which recreates the container and starts a new, empty log: in item 4, grep `~/hub-before-restart.log` as well.

4. The end of both baselines, and what nobody announced:

   ```bash
   docker compose logs monitor-hub | grep 'msg="baseline complete"'
   ```

   Expected: one line with `monitor=diario` and one with `monitor=sei`, usually within a minute of the start, later for `sei` while SEI does not answer. It is an `info` line: with `LOG_LEVEL` at `warn` or `error` it never shows, so set it to `info` first (step 3).

   For SEI, if nothing restarted before its line, there is nothing to look for. Otherwise the events its first check recorded may have gone into the baseline. Look in the hub log for the monitor's `cycle finished` lines from before the restart: an `events` count above 0 means that check recorded events. Then list the events recorded since the Go program stopped, with the time you noted in step 4, and look for each of them in your channels. Do not use the hub's start time: after a restart Docker reports the restart, which comes after those events.

   ```bash
   docker compose logs monitor-hub | grep 'msg="cycle finished" monitor=sei'
   docker exec <postgres-container> psql -U sei_monitor -d sei_monitor -c "SELECT id, kind, payload::jsonb->>'process_number' AS process, detected_at FROM events WHERE detected_at >= '<when the Go program stopped>' ORDER BY id;"
   ```

   Also check that the monitor kept the Go program's account. This must return the count you noted in step 4:

   ```bash
   docker exec <postgres-container> psql -U sei_monitor -d sei_monitor -c 'SELECT count(*) FROM accounts;'
   ```

   One more means `SEI__SEI_URL`, `SEI__SEI_USERNAME` or `SEI__SEI_ORG` differs from the old `.env`: the monitor started a new account, and its first check only took note of what is there. Stop the hub and compare the three values with the old `.env` before you go on.

   For the gazette, wait one more minute after its line, then look in the monitor's database for alerts that nobody was told about:

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
   - A row recorded before the Go container stopped (the time you noted in step 4), with `sent_by_go` empty or missing a channel the old stack used, was announced by nobody. Read it there and pass it on yourself.
   - A row recorded after the hub started comes from the monitor's first cycle and went out through the hub, unless something restarted first (next point).
   - If the hub or the stack restarted before the `baseline complete` line, or the hub log shows `monitor exited` or `monitor failed to start` for `monitor=diario` before it (item 3), run the query anyway: the rows recorded around the restart may have been taken in by the baseline. Look for each of them in your channels.

   Both `seen_at` and the hub's log times carry their UTC offset, so compare them with that in mind. Use your own role and database names.

5. The PDF extractor in the image:

   ```bash
   docker exec monitor-hub pdftotext -v
   ```

   Expected: a `pdftotext version` line. The gazette monitor runs the same check before it starts and refuses to start without it. The hub log then shows a `fatal` line with `monitor=diario` and an `error` that starts with `checking pdftotext:`, followed by `monitor failed to start`, and `/api/monitors` shows `diario` with `"running":false`.

6. Ask the gazette monitor to check now. A whole cycle also proves that the arm64 build runs on the board: it resolves and reaches the gazette over TLS, reads the listing and writes to its database.

   ```bash
   curl -s -X POST -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/monitors/diario/check
   ```

   Expected: `{"accepted":true,"message":"..."}`. A 404 means the monitor name is wrong, a 503 or 502 that the monitor is not reachable. `accepted:false` carries the monitor's reason in `message`. With `a check is already running`, the cycle the monitor starts with has not ended: ask again in a minute.

   Then read the monitor list after half a minute:

   ```bash
   curl -s -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/monitors
   ```

   Expected for `diario`: `"state":"idle"` and a `last_success_at` later than the moment you asked (the API gives times in UTC). `checking` means the cycle is still running, so repeat. `failing` gives the reason in `message`.

   Do not ask `sei` for a check now. Its first check starts on its own as soon as it has reached SEI, and it refuses a check within 5 minutes of the last one. The same list proves the SEI side: `sei` as `idle` with a `last_success_at` later than the start means the arm64 build reached SEI over TLS, logged in, read your units and wrote to its database.

7. The stored events and the feed:

   ```bash
   curl -s -H "Authorization: Bearer $API_TOKEN" 'http://127.0.0.1:8090/api/events?limit=5'
   curl -s "http://127.0.0.1:8090/feed.xml?token=<FEED_TOKEN>" | head -5
   ```

8. No RPC errors after a few poll rounds (the hub polls every 15 seconds):

   ```bash
   docker compose logs --since 10m monitor-hub | grep -iE 'error|warn'
   ```

   Expected: no output, or only lines you can explain, such as the `poll a monitor` warnings for `sei` before it reached SEI (item 3). Leave `LOG_LEVEL=debug` off unless you are chasing something.

Finally open `http://hub.lan` if the proxy is set up.

## Add the anhanguera monitor

The anhanguera monitor reads the Anhanguera student portal (AVA) and reports invoices, activity deadlines and calendar events. It is a third monitor next to `diario` and `sei`, and it starts on its own database. There is nothing to take over: it has no old service.

Pull the image first (`docker compose pull`) and check that it carries the monitor:

```bash
docker run --rm --entrypoint ls ghcr.io/itztas/monitor-hub:latest /opt/monitor-hub/monitors
```

Check: the list shows `anhanguera`. If it does not, the image is older than the commit that added the monitor: skip this section and come back once the hub's CI has published a newer image. If you set `MONITOR_HUB_TAG`, use that tag instead of `latest`.

### Create its database

Run this in the shared Postgres as its admin user, with a real password you keep for the next step. The role owns the database, so it also owns every table the monitor creates.

```bash
docker exec -i <postgres-container> psql -U postgres <<'SQL'
CREATE ROLE anhanguera_monitor LOGIN PASSWORD '<anhanguera-db-password>';
CREATE DATABASE anhanguera_monitor OWNER anhanguera_monitor;
SQL
docker exec -i <postgres-container> psql -U postgres -c '\l' | grep anhanguera_monitor
```

Check: one line with `anhanguera_monitor` as owner. The monitor applies its migrations on first start. The rules from step 3, "The monitor database URLs", apply here too: connect straight to Postgres with no pooler in between, use `sslmode=disable` or `sslmode=prefer`, and leave `idle_session_timeout` at `0` for the role.

### Set the variables

Copy the `anhanguera` block from `.env.example` into `.env`. Four settings are required:

- `ANHANGUERA__DATABASE_URL`, or `ANHANGUERA__DATABASE_URL_FILE`: `postgres://anhanguera_monitor:<anhanguera-db-password>@postgres:5432/anhanguera_monitor?sslmode=disable`. If the hub also joins the sei-monitor stack's network (step 1), put the shared Postgres container's name in place of `postgres`, as in the other URLs.
- `ANHANGUERA__AVA_USERNAME`: the student's CPF.
- `ANHANGUERA__AVA_PASSWORD`, or `ANHANGUERA__AVA_PASSWORD_FILE`: the portal password. A password with `$` or quotes can reach the monitor changed, so use the file form for it. Mount the file as section 8, "Secrets from files", shows for the SEI password.
- `ANHANGUERA__COURSES`: the course names to watch, separated by commas.

The rest have defaults. The monitor reads the portal once an hour, and every start of the monitor is one login to the portal, so do not change the settings in a loop. Then restart the hub (`docker compose up -d`) and check `/api/monitors` as in step 5: `anhanguera` should be `running`.

### Run `probe` after the first deploy

The monitor's queries come from the portal's documented API, and `probe` is the first check against what the portal really answers. Run it once the monitor has started once, so its database has a state row:

```bash
docker exec monitor-hub /opt/monitor-hub/monitors/anhanguera/anhanguera-monitor probe
```

It reads the same `ANHANGUERA__` settings the monitor uses, runs every query once per enrollment and per configured course, and prints one JSON line on stdout. The line holds shapes only: field names, types and dates with every digit replaced by `9`. It never prints a value, an id, a name, a URL or a token. Each operation has a verdict, `ok` or a problem such as `login_rejected` or `unavailable`.

Exit codes:

- `0`: every verdict is `ok`.
- `2`: some verdicts are not `ok`. Read which operations in the output.
- `1`: it could not run. The reason is on stderr: a configuration error, no state row (`start the monitor once first`, so start the monitor and repeat), an unreachable database or portal, or a rejected login.

`probe` records nothing, so you can run it again. When the first invoice appears on the portal, compare the `Invoices` shape in the output with the monitor's `docs/site-api.md` and fix whichever side is wrong.

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

Use this if the hub misbehaves and you want the old services back. The old stacks are already pinned to their Go images (step 1). The rollback also stops the anhanguera monitor, which has no old service to take over.

1. List what the hub has not delivered. Nobody sends these after the rollback: the old programs do not read the hub's tables.

   ```bash
   docker exec -i <postgres-container> psql -U monitor_hub -d monitor_hub -c "SELECT e.seq, e.monitor, e.title, e.url, e.detected_at, d.channel, d.status, d.attempts, d.last_error FROM deliveries d JOIN events e ON e.seq = d.event_seq WHERE d.status <> 'sent' ORDER BY e.seq, d.channel;"
   ```

   Each row is one notification for one channel: the event's position in the hub, its monitor, title and link, when the hub stored it, and the channel. `pending` means the hub still meant to send it. `failed` means the hub gave up after six attempts, with the last error in `last_error`; the hub never retries those, so the list may hold old ones you already know about. The query reads only the database, so it works with the hub stopped too. Save the output, and pass on every row you have not already handled.

2. Stop the hub:

   ```bash
   cd /opt/stacks/monitor-hub && docker compose down
   ```

   The hub may have read SEI events after item 1 and not sent them before it stopped, and the SEI monitor may have recorded events after the hub's last read. Run the query of item 1 once more, which now also shows what the hub read in its last seconds and never sent, then list the SEI events the hub never read. The first command prints the hub's stored position for `sei`; put that number in the second:

   ```bash
   docker exec <postgres-container> psql -U monitor_hub -d monitor_hub -tAc "SELECT cursor FROM monitors WHERE name = 'sei';"
   docker exec <postgres-container> psql -U sei_monitor -d sei_monitor -c "SELECT id, kind, payload::jsonb->>'process_number' AS process, detected_at FROM events WHERE id > <that position> ORDER BY id;"
   ```

   Nobody sends these after the rollback (item 5). Pass every row on yourself.

   Then list the gazette alerts the hub never confirmed. The script in the next item marks them as delivered too, and they are on no other list, so after it nobody sends them:

   ```bash
   docker exec <postgres-container> psql -U diario_alerr -d diario_alerr -c 'SELECT seq, id, title, link, published_at FROM alerts WHERE seq > (SELECT acked_seq FROM monitor_state) ORDER BY seq;'
   ```

   These are alerts the monitor recorded after the hub's last read, and, while the hub was ahead of this database (section 8), every alert the monitor recorded since it was reset. The monitor learns what the hub stored only on the hub's next poll, so the list also holds the alerts of the last read, which may appear in item 1 too. Pass every row on yourself. A rollback usually follows a hub that misbehaved, and that is when this list can be long.

3. Gazette database. The old Go program announces every stored alert that has no row in `alert_deliveries` for one of its channels, `telegram` and `ntfy`. The monitor never writes that table, so every alert it stored would be announced again. Before you start the old gazette stack, run the monitor's rollback script, `sql/rollback/mark-alerts-delivered.sql` in the diario-alerr repository at tag `v2.0.1`:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/ItzTas/diario-alerr/v2.0.1/sql/rollback/mark-alerts-delivered.sql -o ~/mark-alerts-delivered.sql
   cat ~/mark-alerts-delivered.sql
   docker exec -i <postgres-container> psql -U diario_alerr -d diario_alerr < ~/mark-alerts-delivered.sql
   ```

   Check: `cat` shows a single `INSERT INTO alert_deliveries` statement, and `psql` answers `INSERT 0` followed by a count. Use the role and database names of your stack.

   What it costs: the script marks every stored alert as delivered on both channels, including any alert the hub had stored and not delivered yet (item 1 lists those). After the rollback nobody sends them. Rows for a channel the old stack never enabled are ignored by the old program, and the script is safe to run twice. The monitor's test `go_sends_nothing_from_the_rust_era_after_rollback` (`tests/go_compat.rs`) runs it and then the Go `v1.0.0` program on the same database. The image pinned in step 1 has the same database code as `v1.0.0`. The script has not been run against your database.

4. Gazette schema. The monitor added one column, `monitor_state.acked_seq`, with default `0`. The old program ignores it: the monitor's `tests/go_compat.rs` starts the `v1.0.0` program on a migrated database.

5. SEI database. Nothing to run. The monitor adds no table or column and uses the Go program's schema, and Go starts on the same database with no SQL step. Go writes a notification row only for the events it records itself, so it never announces an event the monitor stored, even when it detects the same change again. What to expect once it runs:
   - Go announces only what it detects from then on. Its first notification can take up to 30 seconds after the check that found it.
   - Notifications Go left pending at the switch to the monitor go out right after the rollback, possibly days late. Step 4 made sure there were none.
   - When the monitor deleted old events by `RETENTION_DAYS`, Go's notification rows for them went too, pending ones included. Those are never sent.
   - Events the monitor recorded after the hub's last read are sent by nobody: item 2 lists them.
   - Go has no hold after a rejected login: if a pause reason is on record, it tries the login once at start, as it always did. Fix the password first if SEI refused it (section 8).

   The monitor's test `go_sends_nothing_from_the_rust_era` (`tests/go_compat.rs` in the sei-monitor repository) starts the Go `v1.0.0` program on a database the monitor wrote. It has not been run against your database.

6. Start the old stacks, with the same `.env` files as before the switch. For SEI that means the same `SEI_URL`, `SEI_USERNAME` and `SEI_ORG` on the same database:

   ```bash
   cd /opt/stacks/sei-monitor && docker compose up -d
   cd /opt/stacks/diario-alerr && docker compose up -d
   docker ps --format '{{.Names}}\t{{.Image}}' | grep -E 'sei-monitor|diario-alerr'
   ```

   Check: the images are the Go ones pinned in step 1, and each service answers on its old `/healthz` and logs no error. The hub's database is not touched by the old services and can stay.

### Coming forward again

Moving back to the hub after a rollback is a new take-over. With the hub still stopped, reset its stored position for both monitors. Otherwise the hub resumes from a position that no longer matches what the Go programs added in between, and announces again everything they stored and already sent: for the gazette every alert with a `seq` above the hub's old position, for SEI every event with a higher id. Back up the hub's database first, then reset:

```bash
docker exec <postgres-container> pg_dump -U monitor_hub -Fc monitor_hub > ~/monitor_hub-$(date +%F).dump
ls -l ~/monitor_hub-*.dump
docker exec -i <postgres-container> psql -U monitor_hub -d monitor_hub <<'SQL'
DELETE FROM events WHERE monitor IN ('diario', 'sei');
UPDATE monitors SET cursor = 0, baselined_at = NULL WHERE name IN ('diario', 'sei');
SQL
```

`psql` answers `DELETE` with a count, then `UPDATE 2`. This is the reset of section 8 for both monitors, and it also deletes the hub's stored events of both and the deliveries still waiting for them, so do it after item 1 of the rollback has listed those.

Then follow steps 4 and 5 again: for SEI, the pending count and the pause check of step 4; for the gazette, the log check of step 4 and the query of step 5. In that query, `sent_by_go` shows `ntfy,telegram` for every alert that existed at the rollback, because the rollback script wrote those rows, even where Go never sent it. The column only tells you something about alerts recorded after the rollback.

## 8. Operating rules

- **Resetting a monitor's database.** Do it only together with the hub's stored position for that monitor, and with the hub stopped. Reset alone, the monitor's numbering restarts below the hub's position, and its new events up to that position are never delivered. Both monitors tell you when this happens. The gazette monitor logs a line that starts with `the hub is ahead of this database` at `error` level once per process. The SEI monitor logs one that starts the same way, at `error` level, when it notices. `/api/monitors` then shows the monitor as `failing` with the same text in `message`. Until the two agree the monitor returns no events and confirms none, so it deletes none of them either. Reset the hub's side too. This deletes the hub's stored events for that monitor, so back up the hub's database first. The table and columns are from `sql/schema/00001_hub.sql`, as `sql/schema/00004_monitors.sql` renamed them.

  ```bash
  docker exec <postgres-container> pg_dump -U monitor_hub -Fc monitor_hub > ~/monitor_hub-$(date +%F).dump
  docker exec -i <postgres-container> psql -U monitor_hub -d monitor_hub <<'SQL'
  DELETE FROM events WHERE monitor = '<monitor>';
  UPDATE monitors SET cursor = 0, baselined_at = NULL WHERE name = '<monitor>';
  SQL
  ```

  The delete is needed because the hub silently drops an event whose id it already stored for that monitor, and a new database reuses ids such as `welcome`. The next read is a new baseline: what the monitor held when it started is stored and not announced. On a new gazette database the welcome and the first cycle's alerts are announced on the hub's second read, about 15 seconds later. On a new SEI database the monitor's first check of the account only takes note of what is there, and reports start with its second check. This statement was run on a throwaway copy of the hub's schema, not on a production database.

- **No retention in the hub.** The hub never deletes events or deliveries. Rows for a channel that is never paired or never works pile up. Prune them by hand if the database grows.
- **Nobody is told when a monitor is failing.** The hub sends no notification about a monitor's state. A `failing` or `paused` monitor shows only in `/api/monitors`, with the reason in `message`, and in the monitor's own lines in the hub log. There are two exceptions: a SEI login that SEI refused, which also records an event (next point), and an anhanguera portal that keeps failing for `ANHANGUERA__FAILING_ALERT_AFTER` (24 hours by default), after which that monitor records an alert. Look at the list after every hub restart and every configuration change, and now and then:

  ```bash
  curl -s -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/monitors
  docker compose logs --since 24h monitor-hub | grep -E 'monitor=(diario|sei|anhanguera)' | grep -E 'level=(WARN|ERROR)'
  ```

  A failing `sei` check retries on its own after 30 seconds, then a minute, doubling up to `POLL_INTERVAL`, and goes back to `idle` after the first check that works. If the message names an unknown agency or module and stays, fix `SEI__SEI_ORG` or set `SEI__SEI_API_URL`, then restart the hub.

- **When SEI refuses the login.** The SEI monitor tries the login once. When SEI rejects it, the monitor stops logging in, because repeated failures can lock the account. It stays up and records one urgent `account_login_failed` event titled `sei-monitor could not log in to SEI`, with SEI's reason. `/api/monitors` shows `sei` as `paused`, with the reason followed by a hint to restart the monitor with the configuration fixed and then request a check, and the monitor refuses checks.

  The monitor also stores the reason in its database, so the pause survives restarts. A monitor that starts with a reason on record is held: it does not log in at all, `/api/monitors` shows `paused` with the reason followed by `nothing is logging in: request a check once the configuration is fixed`, and it records one more `account_login_failed` event, at most once per UTC day. Restarting the hub is not a retry, on purpose: the hub also restarts the monitor by itself, for example after a lost database connection, and every rejected attempt counts toward SEI locking the account. Only a check you request tries the login again. To resume:

  1. Fix the password: in SEI, or in `.env`, the secret file or the password command.
  2. Restart the hub so the monitor starts held with the new configuration. After editing `.env`, run `docker compose up -d` in `/opt/stacks/monitor-hub` (a plain `restart` keeps the old environment); otherwise `docker compose restart monitor-hub`. You can skip this when `sei` already shows the held message and its configuration did not change: you fixed the password in SEI, or it comes from `SEI__SEI_PASSWORD_CMD`, which runs at every login.
  3. Request a check:

     ```bash
     curl -s -X POST -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/monitors/sei/check
     ```

     Expected: `{"accepted":true,"message":""}`. `accepted:false` with `sei-monitor has not reached SEI yet` means the monitor is still connecting after the restart: ask again in a minute. `accepted:false` with `monitoring is paused because SEI rejected the login` means the monitor is paused, not held: do item 2 first.
  4. Read `/api/monitors` after half a minute. `sei` should be `idle` with a `last_success_at` later than your request. If SEI rejected the password again, it is `paused` with the first message: start over from item 1.

  Once SEI accepts the login of a requested check, the monitor erases the recorded reason, even when something later in that check fails, and then retries on its own as for any failing check. If the erase did not reach the database, a restart before the monitor's next good check finds the reason still recorded and holds the monitor again, without a login: request another check. A check whose login got no answer from SEI (an error page or a time-out) also leaves the monitor held: request another one later.

- **SEI operator commands.** The SEI binary has three commands for you: `orgs` lists the acronym and id of every agency on the SEI login page and never logs in; `test-login` logs in and shows the SEI and module versions and your units; `diagnose` checks how the SEI API behaves and prints only counts and types. `test-login` and `diagnose` cost one login each run and do not look at a rejection the monitor recorded, so with a wrong password each run is one more rejected login. Run them inside the hub's container, where the binary lives. This copies the hub's `SEI__` variables to the plain names the command reads and removes every other variable except `PATH`, so no secret shows up on a command line and the command never sees the hub's other settings:

  ```bash
  cd /opt/stacks/monitor-hub
  docker compose exec monitor-hub sh -c '
  names="SEI_URL SEI_API_URL SEI_USERNAME SEI_PASSWORD SEI_PASSWORD_FILE SEI_PASSWORD_CMD SEI_ORG SEI_UNITS SEI_TIMEZONE SEI_CONCURRENCY"
  for name in $names; do
    eval "value=\${SEI__$name-}"
    [ -z "$value" ] || export "$name=$value"
  done
  for name in $(env | sed -n "s/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p"); do
    case " PATH $names " in *" $name "*) ;; *) unset "$name" ;; esac
  done
  exec /opt/monitor-hub/monitors/sei/sei-monitor "$@"' sei test-login
  ```

  Put `orgs` or `diagnose` in place of `test-login` at the end. They work while the monitor runs. Ctrl+C stops a command with exit code 130.

- **Gazette client limits.** The monitor stops with a clear error on a gazette listing of more than 100 pages, a listing page over 4 MiB, a PDF over 256 MiB, or more than 64 MiB of text from one PDF.
- **An edition that can never be read.** One edition the monitor cannot read, such as a broken PDF or one over the limits above, stops every later edition from being recorded, as in the Go program, and each cycle downloads them all again. `diario` stays `failing` with `could not read edition <title>: ...`. To get past it, read that edition yourself, then mark it as seen with its numeric id. The id is the `id` the gazette's media API gives for that title: the address is `DEFAULT_GAZETTE_API_URL` in `src/config.rs` of the diario-alerr repository, and adding `?mime_type=application/pdf&_fields=id,date_gmt,title&after=<day before>T00:00:00` lists the editions published after that time.

  ```bash
  docker exec <postgres-container> psql -U diario_alerr -d diario_alerr -c 'INSERT INTO seen_editions (edition_id) VALUES (<id>);'
  ```

  Then ask for a check (step 5) and expect `idle` with a fresh `last_success_at`.
- **Two scans at once, and `/tmp`.** The monitor scans two editions at a time. Each download is a file named `edition-<six letters or digits>.pdf` in `/tmp` until `pdftotext` has read it. The 320m tmpfs holds one PDF at the 256 MiB limit, not two (see "The files"). A monitor process that is killed mid-scan leaves its file behind, and the monitor deletes such files the next time it starts.
- **Two behaviors kept from the Go program.** An edition whose `date_gmt` is in the future moves the monitor's cursor there for good. After that the listing comes back empty, every cycle succeeds, and no edition is seen until you move the cursor back by hand:

  ```bash
  docker exec <postgres-container> psql -U diario_alerr -d diario_alerr -c 'SELECT cursor_at FROM monitor_state;'
  docker exec <postgres-container> psql -U diario_alerr -d diario_alerr -c "UPDATE monitor_state SET cursor_at = '<publication time of the last real edition>';"
  ```

  A `cursor_at` in the future means this happened. The other one: a PDF without a text layer, such as a scan saved as images, reads as "no match", with no alert and no error.
- **A monitor that never starts.** If `running` stays false, the monitor's configuration error is in the hub log. A misspelled `SEI__`, `DIARIO__` or `ANHANGUERA__` name is not an error: the monitor never sees it and uses the default or reports the variable missing.
- **Secrets from files.** `_FILE` variables need the file inside the container. The compose file mounts none, so add a volume before using one. The hub and its monitors run as user `65532`, which must be able to read the file. For the SEI password, for example, put it in `/opt/stacks/monitor-hub/secrets/sei_password`, then:

  ```bash
  sudo chown 65532:65532 /opt/stacks/monitor-hub/secrets/sei_password
  sudo chmod 400 /opt/stacks/monitor-hub/secrets/sei_password
  ```

  Uncomment the matching `volumes` line in `compose.yaml`, which mounts the file read-only at `/run/secrets/sei_password`, set `SEI__SEI_PASSWORD_FILE=/run/secrets/sei_password` and leave `SEI__SEI_PASSWORD` out. The monitor reads the file once, at start, so restart the hub after you change it. `SEI__DATABASE_URL_FILE` works the same way. The gazette monitor has no `_FILE` form.
- **The SEI password command.** `SEI__SEI_PASSWORD_CMD` runs at every login, without a shell, with a 30-second limit, and without the secret variables in its environment. Its program must exist inside the hub's image, which has no password manager. The monitor refuses to start when it cannot find the program on `PATH`.
- **Consumers.** A consumer is a program that receives the hub's notifications; [`protocol.md`](protocol.md#consumers) describes the contract. Create one inside the container. The token prints once, so copy it now:

  ```bash
  docker exec -it monitor-hub monitor-hub consumers add notebook
  docker exec -it monitor-hub monitor-hub consumers list
  docker exec -it monitor-hub monitor-hub consumers rotate notebook
  docker exec -it monitor-hub monitor-hub consumers revoke notebook
  ```

  `rotate` prints a new token and keeps the position; the old token stops working. `revoke` deletes the consumer, and its open stream ends within 25 seconds. A new consumer starts at the newest event and receives no history. `list` shows each consumer's position, how many events it is behind, and when it last acknowledged and was last seen. With `API_TOKEN`, `GET /api/consumers` returns the same, plus `connected` for whether a stream is open now:

  ```bash
  curl -s -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8090/api/consumers
  ```

  To see the stream by hand, with the consumer's token:

  ```bash
  curl -N -H "Authorization: Bearer <token>" http://127.0.0.1:8090/consumer/v1/stream
  ```

  A consumer outside your home needs the hub over HTTPS or a VPN. Expose `/consumer/v1/` and `/icons/` and nothing more. They use the consumer's token, and `/icons/` needs none.

## 9. What changed for you

- **AMQP.** The exchange is `monitor-hub` unless you set `AMQP_EXCHANGE`, the routing key is `<monitor>.<kind>` for one event and `<monitor>.batch` for several, and the body is `{"events":[...]}` instead of one bare event per message. Delivery is at-least-once, and each message carries a message id derived from its body so a consumer can drop a repeat. Rebind consumers of the old exchange.
- **The SEI web page and routes.** The SEI service's web page (`/ui/`) and its domain routes (`/processes`, `/status` with SEI details) have no replacement yet. The API is the hub's: `/api/monitors`, `/api/monitors/{name}/check`, `/api/events`, with a different response shape and the header `Authorization: Bearer <API_TOKEN>`.
- **Feeds.** The old gazette service served its own public `/feed.xml`. The old SEI service served `/feed/<FEED_TOKEN>.xml`. The hub serves one feed for all monitors at `/feed.xml?token=<FEED_TOKEN>` (the token is needed when `FEED_TOKEN` is set), filtered with `&monitor=diario` or `&monitor=sei`. Subscribers of the old addresses must change them.
- **The image.** `ghcr.io/itztas/monitor-hub` is public on the registry, so `docker compose pull` needs no login. It carries the monitors, so there is one image to pull instead of one per service.
- **A refused SEI login.** The Go program tried the login again at every start. The monitor does not: after SEI refuses it, only a check you request tries again (section 8).
- **The first minutes.** A new monitor's first read is stored and not announced (step 5). The gazette's welcome on a new database, and its first cycle's alerts, come on the hub's second read, about 15 seconds after the first, not at the moment the monitor starts.

## 10. Known issues

The hub is built with the Go version pinned in the repository. That version carries two standard-library advisories, GO-2026-6859 and GO-2026-6809, and the hub's dependencies carry two more that its code does not call (`golang.org/x/net` 0.61.0 and `golang.org/x/crypto` 0.58.0). Raising the pinned Go version is the owner's decision.
