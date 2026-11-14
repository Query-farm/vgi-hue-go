# Hue: preview, roll back and commit a brightness cap

[Open the executed notebook](01_preview_rollback_and_commit_a_brightness_cap.ipynb).
It selects reachable lights that are already on, previews a brightness cap in a
transaction, proves rollback leaves the bridge unchanged, and then commits the
same bounded change. A second attachment and a local HTTP-write audit distinguish
staged settings from bridge state.

**All devices and writes are synthetic.** The bundled repository mock bridge runs
only on loopback. No physical lights, real bridge, account, discovery or real
application key are used. Each execution starts a fresh fixture and disposable
transaction directory; the example has no live mode.

## Prerequisites and execution

Install [uv](https://docs.astral.sh/uv/), Git and Go 1.21 or newer. The Go launcher
provisions the exact Go 1.27.4 toolchain automatically. Its POSIX cache lock supports
macOS/Linux. From this directory:

```bash
uv sync --frozen
uv run python execute.py
```

The locked Python 3.13–3.14 notebook environment uses published Haybarn 2.0.0rc11.
The visible worker location is literal and pinned:

```text
uv run --frozen --project . python github_go_worker.py Query-farm/vgi-hue-go c8edf307c8d5bceae8d66c5d2184ff7dec2f5097 ./cmd/vgi-hue-worker go1.27.4
```

This public repository has no native release. The launcher fetches the exact Git
SHA, verifies it, downloads and verifies Go modules, and builds with read-only
modules, `CGO_ENABLED=0` and a pinned toolchain. It checks that go.mod/go.sum remain
unchanged, then caches the executable with its SHA-256 and pin manifest. A bounded
file lock serializes builds; installation is staged before publication. Build logs
go to stderr; stdout carries VGI. No worker checkout or engine build is needed.

First use needs GitHub, package and module index access and may take minutes. The
runner defaults to 600 seconds per cell; pass `--timeout 1200` if necessary. The
cache is under `$XDG_CACHE_HOME/vgi-notebook-workers/` (or `~/.cache/`), keyed by
repository, revision, package, platform and Go version. Upgrading requires changing
the visible pin and rerunning default, experiment, a cold launch and source gates.

For an editor, register and select this environment:

```bash
uv run python -m ipykernel install --user --name hue-examples \
  --display-name "Python (Hue notebooks)"
```

The entire `notebooks/` folder runs independently of the repository. Keep helpers,
fixtures, assets and lockfile together. Parameters and SQL are visible; setup and
plot implementation are folded. Restart and run all after parameter changes.

## Experiment

```bash
uv run python execute.py --parameters '{"BRIGHTNESS_CAP":30}' \
  --output-dir validation/cap-30
```

The cap accepts integer settings 1–100, default 60. Zero is excluded because a zero
brightness command can switch a lamp off. Only on, reachable lights above the cap
are selected. Stored settings of off and unreachable lamps remain unchanged.
Changing the cap starts from a fresh mock, not the prior committed state. Compare
selected lights, staged/final settings and the actual PUT count. Restore 60 to reset.

The runner uses fresh temporary kernels and notebook-directory cwd. It saves only
after success and requires a separate output destination for overrides. Experiment
copies receive all companions. Logs, charts and experiments are ignored.

## Fixture provenance and isolation

[fixtures/hue_bridge.py](fixtures/hue_bridge.py) is an unmodified copy of
`test/mock_bridge/server.py` at revision
`c8edf307c8d5bceae8d66c5d2184ff7dec2f5097`. Its SHA-256 is recorded in
[provenance.json](fixtures/provenance.json) and verified before import. The five
lights, rooms and other resources are invented repository test fixtures, not
recorded device responses. [mock_bridge.py](mock_bridge.py) wraps the handler with
an independent PUT-path counter; it does not alter its response or state semantics.

Setup clears inherited `HUE_*` and `MOCK_*` settings, disables both bridge and mDNS
discovery, binds only 127.0.0.1 and supplies an explicit loopback bridge URL to both
attachments. The public dummy key authenticates only this fixture. Transaction
storage is temporary. Cleanup closes connections and the server and removes the
transaction files. The mock contains no route that forwards commands to devices.

## Transaction and interpretation details

JupySQL normally opens a new native cursor and can call DBAPI commit after each
statement. The notebook's small `HaybarnSession` adapter retains one native
session, and `%config SqlMagic.autocommit = False` prevents automatic commits.
Visible SQL `BEGIN`, `ROLLBACK` and `COMMIT` alone control the transaction. The
adapter otherwise delegates native connection behavior.

Writes are standalone SQL statements and never captured DataFrames. Initial and
final API snapshots are materialized once. Inside the explicit transaction,
preview SELECTs stay read-only: Haybarn rejects writing a local snapshot table in
a transaction that already modified the VGI catalog. JupySQL may repeat those
small read-only fixture queries during capture; they cannot send light commands.
The local PUT audit verifies zero writes before/after rollback and exactly one
write per selected light after commit.

Brightness is the bridge's percent setting, not measured power, energy, emitted
light or perceived brightness. Off lights can retain nonzero settings. The mock
is deterministic; real bridges may fail, apply updates asynchronously or clamp
values. These results do not prove atomic physical-device behavior. No live-device
validation was performed. See the pinned [worker documentation](https://github.com/Query-farm/vgi-hue-go/blob/c8edf307c8d5bceae8d66c5d2184ff7dec2f5097/README.md)
and [STANDARD.md](STANDARD.md).

The native Haybarn connection uses an empty temporary SQL secret directory with
persistent secrets disabled. Both attachments use only explicit dummy bridge
credentials. Assertions confirm an empty SQL secret inventory before attachment
and at cleanup. Existing saved credentials are neither loaded nor modified.
