# Changelog

## 0.1.0 (unreleased)

Initial worker generated from the VGI starter template.

- Colours, gradients and effects (same in all six ports; COLOR-SPEC-TS.md). `hue.main.lights`:
  `color_x`/`color_y` are now updatable (the xy is sent as is, `{"color":{"xy":..}}`; one
  coordinate alone keeps the other's current value). New columns `gradient_hex VARCHAR[]`,
  `gradient_xy STRUCT(x DOUBLE, y DOUBLE)[]`, `gradient_mode` (all updatable),
  `gradient_points_capable`, `gradient_pixel_count` (read-only) and `effect` (updatable,
  `'no_effect'` stops one). Writes are validated before anything is sent (conflicting colour
  columns, ranges, capabilities, point counts, modes, effects). A column counts as SET only when
  its value changed, lists element-wise, since DuckDB sends every column with RETURNING. In a
  transaction, UPDATE now compares against the rows as the transaction sees them (staged changes
  applied), so a `gradient_mode` change after a staged gradient keeps the staged points. New
  table function `set_gradient(light, colors, mode := 'interpolated_palette')`. COMMIT coalescing
  lets a later colour replace an earlier gradient and vice versa, and a failed COMMIT also
  restores gradients and effects. The mock bridge gained "Hallway strip" (gradient lightstrip in
  the Hallway: 5 points capable, 10 pixels, four modes), effects on the colour lights, and
  bridge-like 400s for invalid gradient/effects/xy writes; `hue.test` expectations that list every
  light were updated for the extra light. Tests: `color_test.go`, `test/sql/lights_color.test`.
  A gradient needs at least 2 points, as on a real bridge (it answers 207 "invalid attribute
  value (.gradient.points)" to 1 or 0): UPDATE and set_gradient refuse fewer before any PUT
  ("light '<name>' takes 2–<max> gradient points, got <n>"), and so does the mock (400). After a
  gradient write RETURNING reports `color_x`/`color_y`/`color_hex` as the first new point and
  `color_mode` 'xy', as the bridge sets them (the mock does the same, so a re-read matches).
  `set_gradient` with an empty or NULL list says "set_gradient needs at least two colours", and a
  `gradient_mode` alone on a light with no gradient (fewer than 2 points, staged changes included)
  is refused before any PUT: "light '<name>' has no gradient to change the mode of; set
  gradient_hex or gradient_xy too".

- Light PUTs are pipelined: an `UPDATE` of `hue.main.lights` (now one send per batch, not per
  row) and the COMMIT replay of a transactional attach start each request on its pace slot
  (~100 ms for lights, ~1 s for grouped_light/scene) without waiting for earlier responses, with
  at most 4 in flight (`maxInFlightRequests`). A light's own requests (state, then rename) stay in
  order, and a grouped_light/scene request waits for everything before it. On the first error no
  new request starts, the in-flight ones finish, and the statement fails with that error. 12
  lights against the mock bridge with a 300 ms PUT delay: ~3.7 s before, ~1.5 s after. The mock
  bridge gained `MOCK_PUT_DELAY_MS` and `MOCK_EXTRA_LIGHTS` (both off by default).

- Requires vgi-go v0.42.0 (protocol vgi.v3 3.3.0; the DuckDB vgi extension now rejects 3.2
  workers with "server is too old").
  Under 3.3 the tables' scans (`lights_scan`, `rooms_scan`, ...) and `lights_update` are listed
  `table_only`: DuckDB still uses them for the tables (projection pushdown for `rowid`, so
  UPDATE works) but they are no longer SQL functions. `duckdb_functions()` for the catalog now
  lists only `recall_scene`, `set_light`, `set_lights` and `set_room`, the same in every port;
  `hue.test` checks this and that calling a scan or `lights_update` directly fails.
  The scans are no longer registered with `RegisterTable` / `RegisterTableUnlisted`: each
  `CatalogTable.Function` registers its own scan, table-only by default (`RegisterTable` made
  `lights_scan` public). No `ExposeScan`: nothing calls the scan directly.

- Requires vgi-go v0.41.1 (protocol vgi.v3 3.2.0). The `pushdown_sqlite` recipe uses
  `ToSQLExact` (`ToSQL` is deprecated and widened `NOT IN` to every row); a predicate with no
  exact SQL translation is now an error instead of a silently unfiltered result.

Targets: vgi-go 0.42 · VGI protocol 3.3 · Haybarn 2.0.0rc11 · vgi-lint-check 2.x (unpinned)
