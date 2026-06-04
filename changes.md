# Changes

Work on the `improve` branch, centered on speeding up the canonical
"count all active series" query `count({__name__=~".+"})`, plus multi-arch
Docker build support.

## Query optimizations

The target query exercises the full PromQL aggregation pushdown path. Each change
below removes work from the generated ClickHouse SQL.

### 1. `.+` regex matcher → `notEmpty(val)`

`{__name__=~".+"}` is the idiomatic PromQL "match all series" selector (PromQL
rejects `.*` as the sole matcher). It was previously transpiled to a per-row
regex `match(val, '.+')` against every value in the `time_series_gin` index.

`.+` means exactly "non-empty value", so it is now emitted as a cheap, vectorized
`notEmpty(val)` check instead of compiling and running a regex.

- `reader/logql/logql_transpiler/clickhouse_planner/planner_stream_select.go`
- `reader/logql/logql_transpiler/clickhouse_planner/sql_misc.go`

### 2. `count(...)` pushed down to ClickHouse

`count` previously fell back to the in-process PromQL engine, which streamed every
raw series out of ClickHouse and counted in memory. It now follows the same
pushdown path as `sum`: the optimizer rewrites `count(<vector selector>)` into an
`AggPlanner` that emits `toFloat64(count())` directly in SQL.

- `reader/promql/promql_transpiler/optimizer/vector_agg.go` — `COUNT` added to the
  optimizer; operator → function name via an `aggFn` map.
- `reader/promql/promql_transpiler/planner/sum.go` — `patchVal` handles `"count"`.

### 3. Ungrouped aggregation fast path (skip the labels join)

`count(x)` / `sum(x)` with no `by`/`without` (or `by ()`) collapses every series
into a single empty-labelled output series. The old plan still joined a
`labels_req` CTE against `time_series`, parsing and rewriting every series' JSON
labels just to produce a constant `{}`.

`AggPlanner.Process` now detects this case (`By && len(Labels) == 0`) and skips
`getLabels`/`patchLabels` and the `time_series` join entirely, aggregating by
`timestamp_ms` with a constant `cityHash64('{}')` fingerprint. Grouped aggregation
(`by (...)`, `without (...)`) is unchanged — `without ()` correctly keeps the join.

- `reader/promql/promql_transpiler/planner/sum.go` — `processNoGrouping`.

### 4. `count` skips sample-value decoding (no `argMaxMerge`)

`count` only counts rows; it never reads the sample value. `pre_agg` now drops the
`argMaxMerge(last)` value decode for `count`, which avoids merging the aggregate
state and, more importantly, stops reading the value-state column from
`metrics_15s` at all. `sum` still decodes values.

- `reader/promql/promql_transpiler/planner/sum.go` — `dropValue` applied when
  `Fn == "count"`.

### 5. Ungrouped labels row no longer double-scans `metrics_15s`

ClickHouse does not materialize `WITH` subqueries — it re-inlines them at each
reference. The ungrouped labels row used `FROM pre_agg LIMIT 1`, which re-inlined
`pre_agg` and scanned `metrics_15s` a second time. It is now sourced from the
lightweight `fp` (GIN) CTE, so `metrics_15s` is scanned once. This also matches the
grouped path, whose label rows derive from series existence, not sample presence.

- `reader/promql/promql_transpiler/planner/sum.go` — `processNoGrouping`.

### Resulting SQL for `count({__name__=~".+"})`

```sql
WITH
  fp AS (
    SELECT fingerprint FROM time_series_gin
    WHERE date >= '...' AND type IN (2, 0)
      AND (key = '__name__' AND notEmpty(val) = 1)          -- (1)
    GROUP BY fingerprint HAVING groupBitOr(...) = 1
  ),
  pre_agg AS (
    SELECT samples.fingerprint AS fingerprint,
           intDiv(timestamp_ns, 15000000000) * 15000 AS timestamp_ms  -- (4) no argMaxMerge
    FROM metrics_15s AS samples
    WHERE timestamp_ns > ... AND timestamp_ns <= ...
      AND fingerprint IN (fp) AND type IN (2, 0)
    GROUP BY fingerprint, timestamp_ms
    ORDER BY fingerprint ASC, timestamp_ms ASC
  )
SELECT * FROM (
    SELECT 1 AS type, cityHash64('{}') AS fingerprint, timestamp_ms,  -- (3) no labels_req join
           toFloat64(count()) AS val, '' AS labels                    -- (2) count pushdown
    FROM pre_agg GROUP BY timestamp_ms ORDER BY timestamp_ms ASC
  )
UNION ALL (
    SELECT 2 AS type, cityHash64('{}') AS fingerprint, 0 AS timestamp_ms,
           toFloat64(0) AS val, '{}' AS labels
    FROM fp LIMIT 1                                                    -- (5) fp, not pre_agg
  )
```

### Refactor

`AggPlanner` was de-duplicated: the grouped and ungrouped paths share
`sampleRow`, `labelsRow`, `aggResult`, `asc`, and `findWith` helpers, so each path
only expresses what is genuinely different. Pure structure change — identical SQL.

### Known remaining overhead (not yet changed)

- The `pre_agg` inner `ORDER BY` is discarded by the outer aggregation.
- The `fp` GIN scan + `IN` filter is redundant for a match-all selector over
  metrics.

### Tests

- `reader/logql/logql_transpiler/clickhouse_planner/planner_stream_select_test.go`
  — `.+` → `notEmpty`, negation, and real regexes unchanged.
- `reader/promql/promql_transpiler/agg_pushdown_test.go` — count pushdown, sum
  unchanged, ungrouped skips the labels join, grouped keeps it, count skips
  `argMaxMerge`, and the labels row avoids the double scan.

## Build & packaging

### Multi-arch Docker image (arm64 + amd64)

`Dockerfile` cross-compiles with buildx: the builder stage runs on
`$BUILDPLATFORM` (native, no QEMU) and Go's built-in cross-compiler targets
`$TARGETARCH` (the project is pure Go, `CGO_ENABLED=0`). Also tidied to exec-form
`CMD` and `ENV key=value`.

`build.sh` drives `docker buildx build` for `linux/amd64,linux/arm64`,
auto-creating a `docker-container` builder. Configurable via `IMAGE`, `PLATFORMS`,
`VIEW`, `PUSH`, `LOAD`, `BUILDER`; guards that `--load` is single-platform only.

### Go module + build caching

The Dockerfile copies `go.mod`/`go.sum` and runs `go mod download` before copying
sources (so the download layer is reused unless deps change), and adds BuildKit
cache mounts for the module cache (`/go/pkg/mod`, shared across arches) and the Go
build cache (`/root/.cache/go-build`, keyed per `TARGETARCH`). Verified: after a
source change the module download is `CACHED` and the build reuses cached
artifacts.
