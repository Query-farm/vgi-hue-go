<!-- managed-by: vgi-starters (edit upstream in shared/AGENTS.md.tmpl, then run tools/render.py) -->
# AGENTS.md: working on this VGI worker

This repository is a **VGI worker** written in Go. DuckDB starts it (or connects
to it over HTTP) and calls its functions from SQL; data moves as Apache Arrow
batches. Background reading, in order of usefulness:

1. https://query.farm/vgi/llms.txt — index of all VGI docs, written for agents
2. https://query.farm/vgi/docs/go/ — the Go SDK (vgi-go)
3. https://query.farm/vgi/building-a-worker — concepts: catalogs, schemas, function kinds

## Definition of done

**`make check` passes.** It runs formatting and static checks, unit tests, the SQL
tests over the stdio, launch and http transports, and the metadata quality check
(vgi-lint, at the `fail_on` level in `vgi-lint.toml`). Don't hand work back until it passes. If a
check is wrong, fix the check and say why. Never delete a test to get green.

Faster loops while iterating: `make test-unit`, `make test-sql`, `make lint-explain`.

## Layout

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

## Recipe: add a function

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

Then, for every new function:

1. **Metadata** (`internal/hue/meta.go` and each function's `Metadata()`): title, category from the schema's category list,
   keywords, `doc_llm` (for agents: what it does, arguments, when to use it) and `doc_md`
   (for humans). Describe each argument, and for table functions describe the result
   columns. Add 1–2 `examples` with fully qualified, analytical SQL
   (`hue.main.fn(...)`): project, filter or aggregate rather than `SELECT *`.
2. **Executable examples:** if the function is deterministic and needs no network,
   add an `executable_examples` entry with `ORDER BY` and an `expected_result`.
3. **SQL test:** add cases to `test/sql/hue.test`, or a new `test/sql/<name>.test`
   with the same header (`require vgi`, `require-env HUE_WORKER`, then `ATTACH`).
   Avoid expected-error text that depends on SDK wording; use `<REGEX>:.*` for errors you
   don't control.
4. **Unit test** for any non-trivial logic.
5. **Agent task:** add a `{name, prompt}` to the catalog's agent test tasks, and its
   grader (`reference_sql` or `check_sql`) to `vgi-agent-tests.yaml`.
6. Run `make check`.

## Passing the metadata check

`make lint-explain` runs [vgi-lint](https://github.com/Query-farm/vgi-lint-check) against
the built worker with the settings in `vgi-lint.toml`. The linter isn't pinned, so new
rules arrive on their own. When it reports findings:

1. Run `make lint-explain LINT_ARGS="--format agent"`. Findings are grouped by rule:
   each rule's fix is stated once, followed by every object it applies to.
2. For a rule you don't understand, run
   `uvx --prerelease=allow --from 'vgi-lint-check>=2,<3' vgi-lint explain VGI123`.
   For tag names and value shapes, read `vgi-lint spec` rather than guessing.
3. Fix errors first, then warnings, then info. Fix the metadata in the worker, never
   the linter's settings.
4. If a finding is genuinely wrong for this worker, waive it in `vgi-lint.toml` with a
   `kind` and a `reason` (see the comments there). Never add an unexplained `ignore`.

The check executes examples (assurance level L2). `make lint-ai` adds L3: an agent must
solve the tasks in `vgi-agent-tests.yaml` using only the metadata, and the docs are
reviewed. It's optional, so run it before publishing.

## Choosing the function kind

| Need | Kind |
|---|---|
| one value per input row | scalar |
| rows from arguments (API, file, generator) | table function |
| transform the rows of a subquery | table in-out function |
| combine many rows into one value | aggregate |
| a fixed dataset referenced by name | catalog table |
| write data out to a file or service | `COPY … TO` copy function |
| accept INSERT / UPDATE / DELETE | catalog table with insert/update/delete functions (see the SDK API reference) |

Recipes for most kinds are in `examples/`.

## Sharp edges (read before writing code)

- **Never write to stdout.** stdout is the Arrow protocol channel, and one stray print
  corrupts the stream. Log to stderr. Use `log/slog` (the SDK configures it to write to stderr); `--debug` or `--log-level debug` turns it up.
- **Scalars have no named arguments.** For an optional scalar argument, add an overload
  (a second function with the same SQL name and one more argument). Named arguments
  (`step := 2`) work on table functions only.
- **Declared constraints aren't a guarantee.** `choices`, `ge`, `le` and `pattern` are
  published so DuckDB and agents can discover them, but SDKs differ in which ones they
  enforce: some skip named table arguments, C# enforces none, and `pattern` matching
  differs (Java requires a full match). Where its SDK doesn't enforce a constraint, the
  hue re-checks it in bind code. Do the same, and on a columnar scalar argument validate
  values in the compute body.
- **Reserved argument names.** DuckDB keywords such as `interval`, `end`, `from`, `order`
  and `table` break `name := value` syntax. Pick another name (`start_date`, `end_date`).
- **State must be serializable.** Table-function state (the `S` in `TypedTableFunc[S]`) is gob-encoded into the HTTP continuation token between batches, because each batch can be served by a different request or process. Keep the cursor in *exported* fields (gob silently drops unexported ones, so the scan would restart from row 0), keep it small, and never put secrets in it.
- **Examples must be deterministic.** Add `ORDER BY`, and no `now()`, random values or
  network. Examples that hit a live API go in `examples`, not `executable_examples`.
- **Credentials.** Read them from a DuckDB secret first (`CREATE SECRET (TYPE hue_api, …)`),
  then from an environment variable. Never put them in logs, examples or the repo, and
  keep them out of table-function state unless the state rule above says
  the SDK encrypts it. Workers that need real credentials set `execute: false` on the metadata check.
- **Errors.** Raise errors with a clear message. For HTTP failures, include the status
  code. DuckDB shows the message to the user.
- **Rate limits.** Retry 429 and 5xx with backoff, honour `Retry-After`, and set timeouts on
  every request.
- **Version pins.** Don't bump the vgi-go minor version by hand. A minor release can
  change the wire protocol, which must match the DuckDB extension exactly. Template
  updates (`make template-diff`) carry tested bumps.

## Language notes (Go)

- Scalars receive whole Arrow columns. `vgi.MapColumn` / `vgi.MapAllColumns` build the
  output column in one pass and keep NULLs as NULLs.
- Table functions: implement `vgi.TypedTableFunc[S]` and wrap it with
  `vgi.AsTableFunction[S]`. `S` is the per-scan cursor. Over HTTP the SDK gob-encodes it
  into the continuation token after every batch, so **every field that must survive
  must be exported**. An unexported field is silently reset on the next request. Copy
  the arguments into `S` in `NewState` instead of re-parsing them in `Process`.
- Emit with `out.EmitArrays(cols, rows)` in output-schema order, `Release()` the arrays
  you built, and call `out.Finish()` (not an error) when done.
- Use `column(name, type, comment, nullable)` from `meta.go` for output fields. The
  comment becomes the column description in DuckDB and agent tools.
- **No cgo by default** (vgi-go 0.40+). The worker builds as a static ~40 MB binary
  with `CGO_ENABLED=0`. Evaluating pushed filters inside the worker (`Evaluate`, or
  `AutoApplyFilters`) needs `import _ "github.com/Query-farm/vgi-go/vgi/exprduckdb"`,
  which embeds DuckDB through cgo: then set `CGO_ENABLED=1` (Makefile, CI, Dockerfile,
  `cgo: "1"` in release.yml) and expect ~100 MB binaries. Translating filters to SQL
  yourself, as `examples/pushdown_sqlite.go` does, needs neither.
- **Table-function state fields must be exported.** vgi-go 0.40+ refuses unexported fields
  at registration (they would be lost between HTTP batches).
- **The `replace` in `go.mod` is required.** Go doesn't inherit a dependency's `replace`
  directives, and vgi-go needs a patched `github.com/google/flatbuffers` on every
  transport (dictionary fields with Arrow metadata otherwise produce IPC schemas strict
  readers reject, until google/flatbuffers#9414 ships). Keep the line, and update it
  when a template update changes it. The SDK's second `replace`, an Arrow fork, is
  needed only for the optional shared-memory transport (`VGI_RPC_SHM_SIZE_BYTES`), so this
  template leaves it out. Add it from the vgi-go README if you enable SHM.
- Recipes: all six are implemented in Go (see `examples/`).
- Dependencies go in `go.mod` (`go get`, then `go mod tidy`). Keep them few: every
  dependency ships to every user of the worker.
- Gates: `gofmt`, `go vet`, `golangci-lint` (config in `.golangci.yml`), `go test`.

## Useful prompts

- "Add a scalar `slugify(text VARCHAR) -> VARCHAR` with full metadata and tests."
- "Replace `fetch_items` with a table function for <API>. Map its JSON to typed columns,
  read the token from a `hue_api` secret, and add mock-server tests."
- "Add a catalog table `countries` (code, name) with a primary key, following
  `examples/catalog_tables`."
