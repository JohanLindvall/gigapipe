package planner

import (
	"fmt"
	"github.com/metrico/qryn/v4/reader/logql/logql_transpiler/clickhouse_planner"
	"github.com/metrico/qryn/v4/reader/logql/logql_transpiler/shared"
	sql "github.com/metrico/qryn/v4/reader/utils/sql_select"
	"strings"
)

type AggPlanner struct {
	Main   shared.SQLRequestPlanner
	Labels []string
	By     bool
	Fn     string
}

func (s *AggPlanner) Process(ctx *shared.PlannerContext) (sql.ISelect, error) {
	main, err := s.Main.Process(ctx)
	if err != nil {
		return nil, err
	}

	patchVal, err := s.patchVal()
	if err != nil {
		return nil, err
	}

	// count() ignores sample values, so drop the argMaxMerge(last) decode from
	// pre_agg: the value column is never merged, and not even read from
	// metrics_15s. Grouped or not, the outer count() only counts rows.
	if s.Fn == "count" {
		main = dropValue(main)
	}

	// Ungrouped aggregation — `count(x)`/`sum(x)` with no by/without, or `by ()` —
	// collapses every series into a single empty-labelled output series. In that
	// case the new labels are a constant `{}`, so we can skip both getLabels and
	// patchLabels and the labels_req join against time_series entirely.
	if s.By && len(s.Labels) == 0 {
		return s.processNoGrouping(main, patchVal), nil
	}
	return s.processGrouped(main, patchVal, ctx)
}

// processGrouped aggregates per output series, joining pre_agg against the
// relabelled time_series fingerprints (labels_req) to compute group membership.
func (s *AggPlanner) processGrouped(main sql.ISelect, patchVal sql.SQLObject,
	ctx *shared.PlannerContext) (sql.ISelect, error) {
	withFp := findWith(main, "fp")
	if withFp == nil {
		return nil, fmt.Errorf("could not find fingerprint subquery")
	}

	withMain := sql.NewWith(main, "pre_agg")
	withLabels := sql.NewWith(s.getLabels(withFp, ctx), "labels_req")
	fp := sql.NewRawObject("labels_req.new_fingerprint")
	ts := sql.NewRawObject("timestamp_ms")

	values := sampleRow(fp, patchVal).
		From(sql.NewWithRef(withMain)).
		Join(sql.NewJoin("any left", sql.NewWithRef(withLabels),
			sql.Eq(sql.NewRawObject("pre_agg.fingerprint"), sql.NewRawObject("labels_req.old_fingerprint")))).
		GroupBy(fp, ts).
		OrderBy(asc(fp), asc(ts))

	labelsReq := labelsRow(sql.NewRawObject("new_fingerprint"), sql.NewRawObject("new_labels")).
		From(sql.NewWithRef(withLabels))

	return aggResult(values, labelsReq, withLabels, withMain), nil
}

// processNoGrouping handles ungrouped aggregation: all series collapse into one
// output series with empty labels (`{}`). It aggregates pre_agg by timestamp only
// and emits a constant fingerprint, avoiding the time_series labels_req join.
func (s *AggPlanner) processNoGrouping(main sql.ISelect, patchVal sql.SQLObject) sql.ISelect {
	withMain := sql.NewWith(main, "pre_agg")
	// The single output series carries empty labels `{}`; keep its fingerprint
	// consistent with the grouped path (cityHash64 of the new labels).
	fp := sql.NewRawObject("cityHash64('{}')")
	ts := sql.NewRawObject("timestamp_ms")

	values := sampleRow(fp, patchVal).
		From(sql.NewWithRef(withMain)).
		GroupBy(ts).
		OrderBy(asc(ts))

	// One labels row for the single output series. Source it from the fingerprint
	// CTE (fp) rather than pre_agg: a second reference to pre_agg would re-inline
	// it (ClickHouse does not materialize WITH) and scan metrics_15s twice. fp is
	// the cheap GIN scan and, like the grouped path's labels_req, yields a row per
	// existing series — so LIMIT 1 emits the labels row iff any series exists.
	labelsFrom := sql.NewWithRef(withMain)
	if withFp := findWith(main, "fp"); withFp != nil {
		labelsFrom = sql.NewWithRef(withFp)
	}
	labelsReq := labelsRow(fp, sql.NewRawObject("'{}'")).
		From(labelsFrom).
		Limit(sql.NewIntVal(1))

	return aggResult(values, labelsReq, withMain)
}

// sampleRow builds the type=1 (samples) row of an aggregation result with the
// given output fingerprint and aggregated value. The caller adds FROM/JOIN,
// GROUP BY and ORDER BY.
func sampleRow(fingerprint, val sql.SQLObject) sql.ISelect {
	return sql.NewSelect().Select(
		sql.NewSimpleCol("1", "type"),
		sql.NewCol(fingerprint, "fingerprint"),
		sql.NewSimpleCol("timestamp_ms", "timestamp_ms"),
		sql.NewCol(val, "val"),
		sql.NewSimpleCol("''", "labels"))
}

// labelsRow builds the type=2 (labels metadata) row carrying the output series'
// labels. The caller adds FROM (and any LIMIT).
func labelsRow(fingerprint, labels sql.SQLObject) sql.ISelect {
	return sql.NewSelect().Select(
		sql.NewSimpleCol("2", "type"),
		sql.NewCol(fingerprint, "fingerprint"),
		sql.NewSimpleCol("0", "timestamp_ms"),
		sql.NewSimpleCol("toFloat64(0)", "val"),
		sql.NewCol(labels, "labels"))
}

// aggResult unions the samples and labels rows into the final result, declaring
// the supplied CTEs.
func aggResult(values, labelsReq sql.ISelect, withs ...*sql.With) sql.ISelect {
	return sql.NewSelect().With(withs...).
		Select(sql.NewRawObject("*")).
		From(&unionAll{ISelect: values, unions: []sql.ISelect{labelsReq}})
}

func asc(col sql.SQLObject) sql.SQLObject {
	return sql.NewOrderBy(col, sql.ORDER_BY_DIRECTION_ASC)
}

// findWith returns the named CTE declared on req (including hoisted ones), or nil.
func findWith(req sql.ISelect, alias string) *sql.With {
	for _, w := range req.GetWith() {
		if w.GetAlias() == alias {
			return w
		}
	}
	return nil
}

// dropValue removes the `val` column from a select. count() ignores sample
// values, so pre_agg can skip the argMaxMerge(last) decode — the value's
// aggregate-state column is then never read from metrics_15s.
func dropValue(req sql.ISelect) sql.ISelect {
	cols := req.GetSelect()
	kept := make([]sql.SQLObject, 0, len(cols))
	for _, c := range cols {
		if a, ok := c.(sql.Aliased); ok && a.GetAlias() == "val" {
			continue
		}
		kept = append(kept, c)
	}
	return req.Select(kept...)
}

func (s *AggPlanner) getLabels(withFp *sql.With, ctx *shared.PlannerContext) sql.ISelect {
	res := sql.NewSelect().Select(
		sql.NewSimpleCol("fingerprint", "old_fingerprint"),
		sql.NewSimpleCol("labels", "old_labels"),
		sql.NewCol(s.patchLabels(), "new_labels"),
		sql.NewSimpleCol("cityHash64(new_labels)", "new_fingerprint")).
		From(sql.NewRawObject(ctx.TimeSeriesDistTableName)).
		AndWhere(
			sql.Ge(sql.NewRawObject("date"), sql.NewStringVal(
				clickhouse_planner.FormatFromDate(ctx.From))),
			sql.NewIn(sql.NewRawObject("fingerprint"), sql.NewWithRef(withFp)))
	return res
}

func (s *AggPlanner) patchLabels() sql.SQLObject {
	sqlLabels := make([]sql.SQLObject, len(s.Labels))
	for i, label := range s.Labels {
		sqlLabels[i] = sql.NewStringVal(label)
	}

	return sql.NewCustomCol(func(ctx *sql.Ctx, options ...int) (string, error) {
		in := &notIn{In: sql.NewIn(sql.NewRawObject("x.1"), sqlLabels...), not: !s.By}
		sqlIn, err := in.String(ctx, options...)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("toJSONString("+
			"mapFromArrays("+
			"arrayMap(x -> x.1,arrayFilter(x -> %s, JSONExtractKeysAndValues(labels, 'String')) as a), "+
			"arrayMap(x -> x.2, a)))", sqlIn), nil
	})
}

func (s *AggPlanner) patchVal() (sql.SQLObject, error) {
	switch s.Fn {
	case "sum":
		return sql.NewRawObject("sum(val)"), nil
	case "count":
		// PromQL count() counts the series in each group at each timestamp; after
		// the join every contributing series is one row, so count() of the rows
		// yields the series count. The result is a float like any other sample.
		return sql.NewRawObject("toFloat64(count())"), nil
	}
	return nil, fmt.Errorf("unknown function: %s", s.Fn)
}

type notIn struct {
	*sql.In
	not bool
}

func (n *notIn) String(ctx *sql.Ctx, options ...int) (string, error) {
	if !n.not {
		return n.In.String(ctx, options...)
	}
	left, err := n.In.GetEntity()[0].String(ctx, options...)
	if err != nil {
		return "", err
	}
	strRight := make([]string, len(n.In.GetEntity())-1)
	for i, v := range n.In.GetEntity()[1:] {
		strRight[i], err = v.String(ctx, options...)
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%s NOT IN (%s)", left, strings.Join(strRight, ",")), nil
}
