<!-- managed-by: vgi-starters (edit upstream in shared/README.md.tmpl, then run tools/render.py) -->
<p align="center">
  <img src="https://query.farm/media-kit/products/vgi/vgi-logo.png" alt="Vector Gateway Interface (VGI)" width="320">
</p>

<p align="center"><em>A Query.Farm VGI worker for DuckDB, written in Go.</em></p>

# VGI Hue Worker — Go

[![CI](https://github.com/your-org/vgi-hue/actions/workflows/ci.yml/badge.svg)](https://github.com/your-org/vgi-hue/actions/workflows/ci.yml)
[![vgi worker: query.farm](https://query.farm/media-kit/shields/vgi-worker.svg)](https://query.farm)

[VGI](https://query.farm/vgi) lets DuckDB call functions that live in a separate
process: your code, in any language, exposed as ordinary SQL. This worker adds
three example functions that you rename and replace with your own:

| Function | Kind | Shows you how to |
|---|---|---|
| `greet(name)` / `greet(name, style)` | scalar | vectorize over a column, overload, constrain a constant |
| `sequence_table(count, step := 1)` | table | stream rows in batches, take named arguments |
| `fetch_items(base_url, page_size := 100)` | table | page through a REST API, retry, read a secret |


## Quick start

**Prerequisites:** Go 1.26+ and [uv](https://docs.astral.sh/uv/) (it runs the SQL
test runner and the metadata checker).

```bash
make build
```

Then open DuckDB 2.0 or [Haybarn](https://query.farm/products/haybarn) and attach the
worker. `LOCATION` is the command DuckDB runs to start it; use an absolute path,
because a relative one resolves against DuckDB's working directory.

**DuckDB 2.0:**

```sql
SET allow_extension_repositories = 'allowed';
CREATE EXTENSION REPOSITORY IF NOT EXISTS query_farm WITH PREFIX 'https://duckdb-extensions.query.farm';
INSTALL vgi FROM query_farm;
LOAD vgi FROM query_farm;
ATTACH 'hue' (TYPE vgi, LOCATION '/abs/path/to/vgi-hue/bin/vgi-hue-worker');
```

**Haybarn 2.0** (`uvx haybarn-cli`):

```sql
INSTALL vgi FROM community;
LOAD vgi;
ATTACH 'hue' (TYPE vgi, LOCATION '/abs/path/to/vgi-hue/bin/vgi-hue-worker');
```

Now call the functions:

```sql
SELECT hue.main.greet('Ada');                         -- Hi, Ada!
SELECT hue.main.greet('Ada', 'formal');               -- Good day, Ada.
SELECT * FROM hue.main.sequence_table(5, step := 2) ORDER BY n;
```

`fetch_items` pages through a REST API. Try it against the public demo API (25 fixed items):

```sql
SELECT category, count(*) AS items, round(avg(price), 2) AS avg_price
FROM hue.main.fetch_items('https://vgi-starter-api.query-farm.services/v1')
GROUP BY category ORDER BY category;
```

If an older cached extension misbehaves, refresh it with `FORCE INSTALL vgi FROM query_farm;`
(DuckDB) or `FORCE INSTALL vgi FROM community;` (Haybarn).

## Using it in a pipeline

Worker functions compose with everything else in DuckDB:

```sql
-- Materialize an API into a local table.
CREATE TABLE items AS SELECT * FROM hue.main.fetch_items('https://vgi-starter-api.query-farm.services/v1');

-- Join it with local files.
SELECT i.name, i.price, s.units
FROM items i JOIN 'sales.parquet' s ON s.item_id = i.id;

-- Call a scalar per row of another table.
SELECT customer, hue.main.greet(customer, 'formal') FROM read_csv('customers.csv');
```

## Project layout

```
cmd/vgi-hue-worker/  # main.go: the worker binary (flags -> serve.Main)
internal/hue/
  worker.go        # assembles the catalog; register new functions in NewWorker
  meta.go          # catalog/schema metadata + helpers (objectTags, column)
  greet.go         # scalar example (overloads, constant arg with choices)
  sequence.go      # table function example (named args, streaming state)
  fetch_items.go   # REST API table function (pagination, retries, secrets)
  version.go       # the worker version (checked against release tags)
  *_test.go        # go test unit tests
internal/serve/    # --http / --unix / stdio and logging flags, shared with recipes
test/sql/          # sqllogictest files run through DuckDB
test/mock_api/     # offline mock of the REST API used by tests
examples/          # recipes (one package main, served by bin/recipes <name>)
ci/                # integration harness used by `make test-sql` and CI
scripts/           # vgi_template.py (init / template-diff)
vgi-agent-tests.yaml  # private graders for the agent test tasks
```

## Adding a function

1. Create `internal/hue/<name>.go`. Copy the closest example: `greet.go` for a
   scalar, `sequence.go` for a generator, `fetch_items.go` for an API.
2. Declare arguments as a struct with `vgi:"..."` tags. Use `pos=N` for positional
   arguments and `default=V` (no `pos`) for named ones; add `ge=`, `le=`, `pattern=` or
   `choices='a,b'` constraints. Scalar column arguments are `*array.String` (etc.) fields
   with `const=false`. `doc=` must be the last key in the tag.
3. Implement `Name()`, `Metadata()` (with `Description`, `Tags: withExamples(objectTags(...), examples)`
   and `Examples`), then `OnBindTyped`/`ProcessTyped` (scalar, wrap with
   `vgi.AsScalarFunction[Args]`) or `ArgumentSpecs`/`OnBind`/`NewState`/`Process` (table,
   wrap with `vgi.AsTableFunction[State]`).
4. Register it in `NewWorker` in `internal/hue/worker.go`, and add its examples to
   `allExamples`.

**Which kind of function?**

| You want to… | Use |
|---|---|
| compute one value per input row | a **scalar** function |
| return a set of rows from arguments (an API, a file, a generator) | a **table** function |
| transform or aggregate the rows of a query | a **table in-out** function or an **aggregate** |
| expose a fixed dataset by name (`SELECT * FROM hue.main.t`) | a **catalog table** |
| write data out to a file or service | `COPY … TO` with a copy function |
| accept `INSERT` / `UPDATE` / `DELETE` | a catalog table with insert/update/delete functions |

The [examples/](examples/) folder has a recipe for most of these. Writable tables are
covered in the Go SDK's API reference (look for the table descriptor's
insert, update and delete functions).

## Secrets and configuration

`fetch_items` shows the recommended pattern. A bearer token comes from a DuckDB secret
when one exists, and from an environment variable otherwise:

```sql
CREATE SECRET (TYPE hue_api, TOKEN 'your-token');      -- per session, never logged
```

```bash
HUE_API_TOKEN=your-token make run-http                # for servers and containers
```

Try it: the demo API's `/v1/auth` endpoint needs the published token `demo-token`.

```sql
SELECT count(*) FROM hue.main.fetch_items('https://vgi-starter-api.query-farm.services/v1/auth');   -- fails: HTTP 401
CREATE SECRET (TYPE hue_api, TOKEN 'demo-token');
SELECT count(*) FROM hue.main.fetch_items('https://vgi-starter-api.query-farm.services/v1/auth');   -- 25
```

Never bake credentials into the image or the source. If a function can't run without
real credentials, its examples can't execute in CI either. Set `execute: false` on the
metadata check for that worker (see `.github/workflows/ci.yml`), and add
`LINT_ARGS ?= --no-execute` to the Makefile so local runs match.

## Testing

```bash
make test-unit                 # fast Go unit tests
make test-sql                  # test/sql/*.test through DuckDB (stdio)
make test-sql TRANSPORT=http   # …over HTTP (also: launch)
make check                     # everything CI runs
```

`test/sql/*.test` files are [sqllogictest](https://duckdb.org/docs/stable/dev/sqllogictest/intro)
scripts. `ci/run-integration.sh` runs them against your worker and fails on any
unexpected skip, so a broken transport can't hide behind a green run.
`fetch_items` is tested against `test/mock_api/server.py` so tests are fast and
offline.

**Testing against real data:** put live tests in their own `.test` file that starts with
`require-env HUE_LIVE`. They run only when that variable is set. Add the skip reason
to `EXPECTED_SKIP_REASONS` in CI.

## Metadata and the quality score

DuckDB, agents and documentation tools discover your functions through their metadata:
titles, descriptions, argument docs, examples and keywords.
[vgi-lint](https://github.com/Query-farm/vgi-lint-check) checks it, and CI runs it on
every push. Its settings live in `vgi-lint.toml`, which `make lint-explain`,
`vgi-lint lint`, and CI all read. This template scores 100/100 with no findings.

```bash
make lint-explain                              # findings, each with its fix
make lint-explain LINT_ARGS="--format agent"   # the same, grouped for coding agents
make lint-ai                                   # optional: an agent must solve vgi-agent-tests.yaml
```

Each report also states an **assurance level**: how much was actually verified.

| Level | Reached when | Here |
|---|---|---|
| L1 structural | the static metadata is clean | workers that need real credentials (`execute: false` in CI) |
| L2 behavioural | the examples also run and return what they claim | **the CI gate** |
| L3 semantic | an agent solves the worker's own test tasks, and the docs are reviewed | `make lint-ai`, before publishing |

- **How strict.** Your copy fails on warnings (`fail_on = "warning"` in `vgi-lint.toml`).
  Raise it to `"info"` before you publish the worker for others.
- **The linter isn't pinned.** It tracks the current quality bar, so a new rule can flag
  metadata that passed yesterday. Fix it, or waive the finding.
- **Waivers.** If a finding is genuinely wrong for your worker, waive it in
  `vgi-lint.toml` with a `kind` and a `reason`.
- **Existing workers.** If you're adding this gate to a worker that already has
  findings, record them with `vgi-lint lint --baseline vgi-lint-baseline --update-baseline`
  and set `baseline` in `vgi-lint.toml`. After that, only new findings fail.

Metadata lives in `internal/hue/meta.go` and each function's `Metadata()`.

## Logging and debugging

stdout carries the Arrow protocol, so **never print to stdout**. Log to stderr instead.
Use `log/slog` (the SDK configures it to write to stderr); `--debug` or `--log-level debug` turns it up.

- Under DuckDB (stdio/launch), worker stderr shows up in DuckDB's error messages and
  terminal.
- In Docker it goes to `docker logs`.
- `make debug` runs one query against the worker with debug logging on.

## Deploying

The same binary serves stdio (DuckDB starts it locally) or HTTP (you host it):

```bash
docker build -t weather-worker .
docker run -p 8000:8000 -e HUE_API_TOKEN=... weather-worker   # health: GET /health
```

```sql
ATTACH 'hue' (TYPE vgi, LOCATION 'https://workers.example.com/hue');
```

Put hosted workers behind TLS and authentication. See
[authentication](https://query.farm/vgi/authentication) for bearer tokens and JWTs, and
[distributing a worker](https://query.farm/vgi/distributing-a-worker) for packaging.
Pass secrets with `-e` or your platform's secret manager.

## Releasing

Push a tag like `v0.1.0`. The release workflow builds signed binaries for linux (amd64, arm64), macOS (arm64) and Windows (amd64) as static pure-Go binaries, and attaches them to the GitHub release. Users download the archive for their platform and point `LOCATION` at the binary inside. Go developers can also `go install github.com/your-org/vgi-hue/cmd/vgi-hue-worker@v0.1.0`. Every artifact is signed with cosign and
carries SLSA build provenance. `ci/check-version.sh` makes sure the tag matches the
version in `internal/hue/version.go`.

## Using with coding agents

[AGENTS.md](AGENTS.md) is written for AI coding agents (Claude Code reads it via
`CLAUDE.md`; Codex, Cursor and others read it directly). It covers the commands, the
recipe for adding a function, the metadata checklist and the sharp edges. A good first
prompt:

> Read AGENTS.md. Replace `fetch_items` with `forecast(lat DOUBLE, lon DOUBLE)` that calls
> the Open-Meteo API. Include full metadata, a mock-server test and a `HUE_LIVE` test.
> You're done when `make check` passes.

## Keeping up with the template

`make template-diff` shows what changed upstream (CI, docs, examples) since your copy was
made. Your copy records its template version in `.vgi-hue.json`.

## Learn more

- [What is VGI?](https://query.farm/vgi) · [Getting started](https://query.farm/vgi/getting-started) · [Building a worker](https://query.farm/vgi/building-a-worker)
- [Go SDK docs](https://query.farm/vgi/docs/go/): [scalar tutorial](https://query.farm/vgi/docs/go/tutorial/scalar/), [table tutorial](https://query.farm/vgi/docs/go/tutorial/table/)
- [Distributing a worker](https://query.farm/vgi/distributing-a-worker) · [Choosing a language](https://query.farm/vgi/languages) · [llms.txt](https://query.farm/vgi/llms.txt)
- SDK source: [vgi-go](https://github.com/Query-farm/vgi-go) · RPC layer: [https://vgi-rpc-go.query.farm](https://vgi-rpc-go.query.farm)

## Support

Open an issue on this repository. For VGI itself, see [query.farm](https://query.farm).

## Authorship & license

The starter template is written by [Query.Farm](https://query.farm) and released under
the [BSD Zero Clause License](LICENSE) (0BSD). Use it for anything, with no attribution
required. Replace this section with your own license once the code is yours.
