<p align="center">
  <img src="https://query.farm/media-kit/products/vgi/vgi-logo.png" alt="Vector Gateway Interface (VGI)" width="320">
</p>

<p align="center"><em>A Query.Farm VGI worker for DuckDB, written in Go.</em></p>

# vgi-hue: Philips Hue lights in DuckDB (Go)

[![CI](https://github.com/Query-farm/vgi-hue-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Query-farm/vgi-hue-go/actions/workflows/ci.yml)
[![vgi worker: query.farm](https://query.farm/media-kit/shields/vgi-worker.svg)](https://query.farm)

A [VGI](https://query.farm/vgi) worker, written in Go, that lets DuckDB read and control
Philips Hue lights through a Hue bridge (CLIP v2 API). The live state of every light,
room, zone, scene and sensor is a table, and changing lights is a query.

```sql
SELECT room, count(*) FILTER (WHERE is_on) AS lit FROM hue.main.lights GROUP BY room;
SELECT * FROM hue.main.set_room('Kitchen', power := false);
```

## Quick start

**Prerequisites:** Go 1.26+ and [uv](https://docs.astral.sh/uv/) (it runs the SQL
test runner, the mock bridge and the metadata checker).

```bash
make build        # bin/vgi-hue-worker (the worker) and bin/recipes
```

The build is pure Go (`CGO_ENABLED=0`), so `bin/vgi-hue-worker` is a single static binary.

### 1. Create an application key

You don't need the bridge's IP for the worker itself: it discovers the bridge. To create a
key, find the IP once with `curl -s https://discovery.meethue.com` (or in the Hue app under
Settings → Bridges), then **press the round link button on the bridge** and, within 30
seconds, run:

```bash
curl -k -X POST https://<bridge-ip>/api -d '{"devicetype":"vgi-hue#duckdb"}'
# [{"success":{"username":"AbCdEf..."}}]   <- this "username" is your app key
export HUE_APP_KEY=AbCdEf...
```

### 2. Attach it

`LOCATION` is the command DuckDB runs to start the worker. Use an absolute path, because
a relative one resolves against DuckDB's working directory.

**DuckDB 2.0:**

```sql
SET allow_extension_repositories = 'allowed';
CREATE EXTENSION REPOSITORY IF NOT EXISTS query_farm WITH PREFIX 'https://duckdb-extensions.query.farm';
INSTALL vgi FROM query_farm;
LOAD vgi FROM query_farm;

ATTACH 'hue' (TYPE vgi, LOCATION '/abs/path/to/hue-worker-go/bin/vgi-hue-worker',
              app_key getenv('HUE_APP_KEY'));
```

**Haybarn 2.0** (`uvx haybarn-cli`): `INSTALL vgi FROM community; LOAD vgi;`, then the same `ATTACH`.

If an older cached extension misbehaves, refresh it with `FORCE INSTALL vgi FROM query_farm;`
(DuckDB) or `FORCE INSTALL vgi FROM community;` (Haybarn).

`app_key` is declared secret, so DuckDB shows it as `<redacted>` in `duckdb_databases()`;
`getenv` keeps it out of your SQL text and history too.

| Option | Meaning | Fallback |
|---|---|---|
| `app_key` | Hue application key (secret, redacted) | `HUE_APP_KEY` env var of the worker |
| `transactional` | Optional, default `false`: stage light changes and send them at `COMMIT` (see [Transactions](#transactions)) | — |
| `bridge` | Optional: bridge IP or host name (HTTPS is used), a full `http(s)://` URL, or a bridge id (16 hex digits) to pick one of several bridges | `HUE_BRIDGE` env var, then discovery |

### Bridge discovery

With no `bridge` (or with a bridge id), the worker finds the bridge itself, in this order:

1. The bridge it found last time (cached in `$XDG_CACHE_HOME/vgi-hue/bridges.json`, by
   default `~/.cache/vgi-hue/bridges.json`), if it still answers. This also follows a
   bridge whose DHCP address changed.
2. mDNS (`_hue._tcp.local`) on your LAN, which works without internet access.
3. Signify's `https://discovery.meethue.com`, which lists bridges that recently checked in
   from your public IP.

Each candidate is confirmed via the bridge's `/api/config`. With several bridges, the error
lists them; pick one with `bridge '<id or address>'`. Set `HUE_DISCOVERY=off` to require an
explicit address, or `HUE_DISCOVERY_MDNS=off` to skip mDNS.

The bridge's certificate is signed by Signify's private CA, so the worker does not verify
it; use it only on a network you trust.

## Reading

| Table | One row per |
|---|---|
| `hue.main.lights` | light: `light_id`, `name`, `room`, `archetype`, `is_on`, `brightness` (0–100), `color_mode`, `color_temperature_kelvin`, `color_x`/`color_y`, `color_hex`, `gradient_hex`, `gradient_xy`, `gradient_mode`, `gradient_points_capable`, `gradient_pixel_count`, `effect`, `reachable`, `product_name`, `model_id`, `device_id` |
| `hue.main.rooms` | room or zone: `room_id`, `name`, `kind`, `archetype`, `grouped_light_id`, `any_on`, `brightness`, `light_count`, `light_names` |
| `hue.main.scenes` | scene: `scene_id`, `name`, `room`, `room_id`, `active` |
| `hue.main.sensors` | sensor (a disabled one has `enabled = false` and NULL readings): `sensor_id`, `kind` ∈ motion / temperature / light_level / contact / device_power, `device_id`, `device_name`, `room`, `enabled`, with `motion`, `temperature_c`, `light_level_lux`, `contact`, `battery_percent`, `battery_state`, `reachable`, `last_changed` |

```sql
-- Lights the bridge cannot reach (switched off at the wall?)
SELECT name, room FROM hue.main.lights WHERE NOT reachable;

-- Average temperature per room, and batteries that need replacing
SELECT room, round(avg(temperature_c), 1) FROM hue.main.sensors WHERE kind = 'temperature' GROUP BY room;
SELECT device_name, battery_percent FROM hue.main.sensors WHERE kind = 'device_power' AND battery_percent < 20;
```

Every query reads the bridge live. Nothing is cached.

## Changing lights with UPDATE

The `lights` table is writable. `WHERE` picks the lights; `SET` says what to change:

```sql
UPDATE hue.main.lights SET is_on = true, brightness = 40 WHERE room = 'Kitchen';
UPDATE hue.main.lights SET color_hex = '#ff8000' WHERE name LIKE 'Signe%' RETURNING name, color_hex;
UPDATE hue.main.lights SET color_temperature_kelvin = 2700 WHERE color_mode IS NOT NULL;
UPDATE hue.main.lights SET name = 'Reading lamp' WHERE name = 'Desk lamp';
```

| Updatable column | Values |
|---|---|
| `is_on` | `true` / `false` |
| `brightness` | 0–100 (above 0 turns the light on unless `is_on` says otherwise; 0 turns it off) |
| `color_temperature_kelvin` | 2000–6500 |
| `color_hex` | `'#rrggbb'`, `'#rgb'` or a basic colour name |
| `color_x`, `color_y` | CIE 1931 xy, each 0–1; sent as is (see [Colours](#colours-hex-or-cie-xy)) |
| `gradient_hex`, `gradient_xy` | the points of a gradient light (see [Gradients](#gradients)) |
| `gradient_mode` | one of the light's gradient modes |
| `effect` | one of the light's effects; `'no_effect'` stops it (see [Effects](#effects)) |
| `name` | 1–32 characters; renames the light on the bridge |

Every other column belongs to the bridge or the device, and changing it is refused before
anything is sent (`cannot UPDATE ... of hue.main.lights; updatable columns: ...`). Each light
gets one command (a rename with a state change gets two). `RETURNING` shows the rows as
written. Each light has a stable `rowid`, a hash of its id, which is how DuckDB tells the
worker which lights matched.

### Colours: hex or CIE xy

`hue.main.lights` reports a light's colour twice: `color_hex` (an approximate
`'#rrggbb'` at full brightness) and `color_x`/`color_y` (the CIE 1931
chromaticity the bridge stores, 0–1). Both are writable:

```sql
-- '#rrggbb' (or 'red', 'blue', ...): converted to xy by the worker.
UPDATE hue.main.lights SET color_hex = '#ff8000' WHERE name = 'Sofa lamp';

-- xy is sent as is, no hex round trip. SET only one coordinate and the
-- other keeps the light's current value.
UPDATE hue.main.lights SET color_x = 0.3, color_y = 0.6 WHERE name = 'Sofa lamp';
UPDATE hue.main.lights SET color_y = 0.35 WHERE name = 'Sofa lamp';
```

Setting a colour switches `color_mode` to `'xy'`. `RETURNING` (and reads inside
a transaction) show the xy you asked for; the bridge may clamp it to the lamp's
gamut, so a later read can differ slightly. The worker refuses, before sending
anything: `color_hex` together with `color_x`/`color_y` (`set either color_hex or
color_x/color_y, not both`), a coordinate outside 0–1, and xy on a light without
colour (`light 'Kitchen pendant' does not support color`).

### Gradients

Gradient lights (Hue gradient lightstrips, Play gradient, Festavia, ...) light several
colours at once. Their rows carry `gradient_hex VARCHAR[]` and `gradient_xy STRUCT(x DOUBLE,
y DOUBLE)[]` (the same points), `gradient_mode`, `gradient_points_capable` (how many points
the light takes) and `gradient_pixel_count` (how many segments it lights individually); for
other lights these are NULL.

```sql
SELECT name, gradient_hex, gradient_mode, gradient_points_capable
FROM hue.main.lights WHERE gradient_hex IS NOT NULL;

UPDATE hue.main.lights SET gradient_hex = ['#ff0000', '#ffff00', '#0000ff'] WHERE name = 'Hallway strip';
UPDATE hue.main.lights SET gradient_xy = [{'x': 0.2, 'y': 0.3}, {'x': 0.5, 'y': 0.4}],
                           gradient_mode = 'segmented_palette'
WHERE name = 'Hallway strip';
-- A mode on its own re-sends the current points with the new mode (refused when
-- the light has no gradient, e.g. after a single colour replaced it).
UPDATE hue.main.lights SET gradient_mode = 'interpolated_palette_mirrored' WHERE name = 'Hallway strip';

-- Or with a function (light id or name, like set_light):
SELECT ok, request FROM hue.main.set_gradient('Hallway strip', ['#ff0000', '#00ff00'], mode := 'random_pixelated');
```

Modes are whatever the light lists (usually `interpolated_palette`,
`interpolated_palette_mirrored`, `random_pixelated`, `segmented_palette`). A
gradient takes 2 to `gradient_points_capable` points (the bridge refuses fewer), none NULL,
each 0–1. A gradient and a single colour (`color_hex`, `color_x`/`color_y`,
`color_temperature_kelvin`) cannot be set in one statement, nor `gradient_hex`
and `gradient_xy`. Setting a single colour on a gradient light replaces the
gradient: `gradient_hex` reads `[]` afterwards. Setting a gradient sets the
light's colour (`color_x`/`color_y`, `color_hex`, `color_mode` 'xy') to its first point.

### Effects

`effect` is the light's running dynamic effect (`'candle'`, `'fire'`, `'prism'`,
...), `'no_effect'` when none, and NULL for lights without effects.

```sql
UPDATE hue.main.lights SET effect = 'candle' WHERE name = 'Sofa lamp';
UPDATE hue.main.lights SET effect = 'no_effect' WHERE name = 'Sofa lamp';  -- stop it
```

An effect the light does not list is refused with the allowed values. In a
transactional attach, colour, gradient, effect and `set_gradient` changes are staged like
every other light change and sent at `COMMIT` (`ROLLBACK` discards them).

### Pacing

Hue asks for at most about 10 light commands and 1 room or scene command per second. The
worker starts light commands 100 ms apart (room and scene commands 1 s apart) and doesn't
wait for each answer before starting the next: up to 4 requests are in flight at once, so
12 lights take about 1.2 s plus one round trip instead of 12 round trips. Requests for the
same light still go one after another, in order, and a room or scene command waits for the
commands before it and runs alone. If the bridge refuses one, no further request starts,
the ones already in flight finish, and the statement fails with the first refusal; lights
already changed stay changed (in a transactional commit they are put back, see below).
`COMMIT` sends its staged requests the same way. Requests that get a 429 or 503 are retried
with backoff, honouring `Retry-After`, and every request has a 10 s timeout.

## Transactions

Attach with `transactional true` and light changes wait for `COMMIT`:

```sql
ATTACH 'hue' (TYPE vgi, LOCATION '/abs/path/to/hue-worker-go/bin/vgi-hue-worker',
              app_key getenv('HUE_APP_KEY'), transactional true);

BEGIN;
UPDATE hue.main.lights SET is_on = true, brightness = 40 WHERE room = 'Kitchen';
SELECT * FROM hue.main.recall_scene('Relax', room := 'Living room');
SELECT name, brightness FROM hue.main.lights WHERE room = 'Kitchen';  -- shows 40: your pending change
COMMIT;      -- everything is sent now, paced and pipelined (see Pacing)
-- or ROLLBACK: nothing was sent, no light changed
```

- `UPDATE` and the control functions stage their requests instead of sending them. `COMMIT`
  sends them in order, merging consecutive changes to the same light. `ROLLBACK` discards them.
- Reads inside the transaction show its pending light changes. Staged room commands and scenes
  aren't reflected in the lights table until they're sent.
- The bridge has no transactions of its own. If it refuses a request part-way through a commit,
  the lights already changed are put back to their state from before the commit, and `COMMIT`
  fails with what happened. That's all-or-nothing in practice, not ACID: another app or a wall
  switch can still change a light mid-transaction.
- DuckDB runs every statement in a transaction. Outside `BEGIN`, a statement applies as it
  commits, right after it runs, and a bridge refusal fails that statement. In a transactional
  attach, `set_room`'s `wait_ms` has nothing to wait for (`settled` is NULL), and `set_lights`
  reports bridge refusals at commit, not per row.
- Works on every transport. Staged changes live in a SQLite file (pure-Go `modernc.org/sqlite`)
  on the worker's host: `$HUE_TX_DIR`, else the user cache directory (`~/.cache/vgi-hue` on
  Linux, `~/Library/Caches/vgi-hue` on macOS, `%LocalAppData%\vgi-hue` on Windows). Over HTTP,
  run the workers for one bridge on a single host.

Without `transactional`, every change is sent as its statement runs, which is the default.

## Controlling with functions

| Function | Changes |
|---|---|
| `set_light(light, ...)` | one light, by id or (case-insensitive) name |
| `set_gradient(light, colors, mode := 'interpolated_palette')` | the points of a gradient light (see [Gradients](#gradients)) |
| `set_room(room, ..., wait_ms := NULL)` | every light in a room or zone at once (its grouped light), by id or name; `wait_ms` (0–30000) waits until the bulbs report the change |
| `recall_scene(scene, room := NULL)` | activates a scene, by id or name (+ room when the name repeats) |
| `set_lights((query), ...)` | every light a query returns; the query needs a `light_id` column (a `name` column is echoed) |

State arguments (`set_light`, `set_room`, `set_lights`) are named and optional; give at
least one:

| Argument | Values |
|---|---|
| `power` | `true` / `false` (`on` is a SQL keyword) |
| `brightness` | 0–100; above 0 implies on, 0 turns off |
| `kelvin` | 2000–6500 white temperature (not with `color`) |
| `color` | `'#rrggbb'`, `'#rgb'`, or red / orange / yellow / green / cyan / blue / purple / magenta / pink / white |
| `transition_ms` | fade time in milliseconds (0–3600000) |

Each returns one row per change: `target_type`, `target_id`, `name`, `ok`, `error`, the
JSON `request` sent, and `settled`. The bridge relays a room command to each bulb over
Zigbee, so `hue.main.lights` can show the old state for a second or two afterwards. Pass
`wait_ms := 3000` to `set_room` to wait for the bulbs; `settled` says whether every
reachable one reported the change in time (it is NULL when you don't wait). Bad arguments
fail at bind, before anything is sent. `set_lights` reports a failure on one light in that
light's row and carries on with the others, pacing its commands at about 10 per second, as
Hue requires.

```sql
SELECT * FROM hue.main.set_light('Desk lamp', brightness := 40, kelvin := 2700);
SELECT * FROM hue.main.set_room('Living room', color := 'orange', brightness := 30, transition_ms := 2000);
SELECT * FROM hue.main.recall_scene('Relax', room := 'Living room');

-- Everything off except the bedroom
SELECT name, ok FROM hue.main.set_lights(
  (SELECT light_id, name FROM hue.main.lights WHERE is_on AND room IS DISTINCT FROM 'Bedroom'),
  power := false);
```

## Project layout

```
cmd/vgi-hue-worker/main.go  # the worker binary (flags -> serve.Main)
internal/serve/             # stdio / --unix / --http and logging flags, shared with the recipes
internal/hue/
  worker.go        # NewWorker: assembles the catalog (tables, functions, attach options, transaction hooks)
  attach.go        # bridge / app_key / transactional ATTACH options, read via the SDK's AttachOptions()
  discovery.go     # find the bridge: cache, mDNS, discovery.meethue.com
  hue.go           # CLIP v2 client (retries, timeouts), colour math, request bodies
  inventory.go     # lights / rooms / scenes / sensors catalog tables
  lights_write.go  # UPDATE hue.main.lights (rowid -> light, SET -> one PUT per light)
  control.go       # set_light / set_room / recall_scene / set_gradient, set_lights (table in-out)
  transactions.go  # sendAll (paced, pipelined PUTs), staging (SQLite), COMMIT (with restore on refusal), ROLLBACK
  sqlite_wal.go    # opens the shared staging database in WAL mode
  meta.go          # catalog/schema metadata and helpers (objectTags, column)
  examples.go      # each function's example queries and the executable examples
  arrowutil.go, omap.go  # Arrow cell helpers; ordered JSON bodies
  version.go       # the worker version (checked against release tags)
  *_test.go        # go test unit tests
test/mock_bridge/server.py  # stateful offline mock of a Hue bridge
test/sql/*.test             # SQL tests against the mock (hue, lights_color, transactions)
examples/                   # recipes (one package main, served by bin/recipes <name>)
ci/run-integration.sh       # runs test/sql over stdio / launch / http
ci/lint.sh                  # vgi-lint against the mock bridge
vgi-agent-tests.yaml        # private graders for the agent test tasks
```

### How the ATTACH options reach the functions

`attachOptionSpecs` (in `attach.go`) declares `bridge`, `app_key` (secret) and
`transactional`. Functions and the transaction hooks read them with the SDK's
`AttachOptions()` accessor (on `ProcessParams` and the transaction hook context): the SDK
carries the options inside the attach value, which DuckDB sends back on every call, and
encrypts the secret `app_key` with a worker-held key, so the app key is never in plaintext
in any opaque value or in function state. The worker never packs options into its own attach
bytes. `attachValidator` turns on transactions only for a `transactional true` attach.
Binding never contacts the bridge, so `DESCRIBE` and planning work offline.

## Testing

```bash
make test-unit                 # go test ./... (pipelining, colours/gradients, staging, flags)
make test-sql                  # test/sql/*.test through DuckDB (stdio); TRANSPORT=launch|http
make test-sql-all              # ...over every transport
make lint                      # gofmt, go vet, golangci-lint
make lint-explain              # vgi-lint metadata check against the mock bridge
make check                     # everything CI runs
```

All tests run against `test/mock_bridge/server.py`, so they need no bridge and change no
real lights. `ci/run-integration.sh` starts the mock with `MOCK_FLAKY=1`, so each GET is
answered with a 429 the first time and the retry path is always exercised. It also clears
`HUE_BRIDGE` / `HUE_APP_KEY` and points `HUE_TX_DIR` at a scratch directory, so your real
bridge and staging file never leak into a test run. It fails on any unexpected skip, so a
broken transport can't hide behind a green run.

To try it against your real bridge, attach the built binary as in [Quick start](#2-attach-it).
Set `HUE_DEBUG=1` (stderr) or `HUE_DEBUG=/abs/path/to/hue.log` (a file, since DuckDB hides
worker stderr) for a trace of the bridge requests, or pass `--debug` in `LOCATION`
(`LOCATION '/abs/path/to/bin/vgi-hue-worker --debug'`) for the SDK's debug logging.

## Deploying over HTTP

```bash
bin/vgi-hue-worker --http --host 127.0.0.1 --port 8000     # health: GET /health
# or: docker build -t vgi-hue-worker . && docker run -p 8000:8000 -e HUE_APP_KEY vgi-hue-worker
```

```sql
ATTACH 'hue' (TYPE vgi, LOCATION 'http://localhost:8000', app_key getenv('HUE_APP_KEY'));
```

The worker must be able to reach the bridge on your LAN (mDNS discovery does not work from
inside a Docker bridge network; pass `bridge` or `-e HUE_BRIDGE=...`). Put any hosted worker
behind TLS and [authentication](https://query.farm/vgi/authentication).

## Releasing

`ci/check-version.sh` makes sure a release tag matches the version in
`internal/hue/version.go`. The release workflow (`.github/workflows/release.yml`) builds
signed static binaries for linux (amd64, arm64), macOS (arm64) and Windows (amd64), with
cosign signatures and SLSA provenance, and attaches them to the GitHub release. In this
repository (`Query-farm/vgi-hue-*`) a pushed tag does not release on its own: run the
workflow manually (`workflow_dispatch`).

## Built from

The [Query.Farm VGI Go starter](https://github.com/Query-farm/vgi-starter-go) (see
`.vgi-starter.json`). `make template-diff` shows upstream changes. [AGENTS.md](AGENTS.md)
has the conventions for coding agents.

- [What is VGI?](https://query.farm/vgi) · [Building a worker](https://query.farm/vgi/building-a-worker) · [llms.txt](https://query.farm/vgi/llms.txt)
- [Go SDK docs](https://query.farm/vgi/docs/go/) · SDK source: [vgi-go](https://github.com/Query-farm/vgi-go)

## License

[BSD Zero Clause License](LICENSE) (0BSD).
