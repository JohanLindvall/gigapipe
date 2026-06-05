package promql_transpiler

import (
	"strings"
	"testing"
	"time"

	clconfig "github.com/metrico/cloki-config"
	clokiconfig "github.com/metrico/cloki-config/config"
	rconfig "github.com/metrico/qryn/v4/reader/config"
	"github.com/metrico/qryn/v4/reader/logql/logql_transpiler/shared"
	"github.com/metrico/qryn/v4/reader/promql/promql_parser"
	sql "github.com/metrico/qryn/v4/reader/utils/sql_select"
)

// renderAggPushdown transpiles an aggregation query and returns the SQL of the
// substitute pushed down to ClickHouse (empty string if nothing was pushed down).
func renderAggPushdown(t *testing.T, q string) string {
	t.Helper()
	if rconfig.Cloki == nil {
		rconfig.Cloki = &clconfig.ClokiConfig{Setting: &clokiconfig.ClokiBaseSettingServer{}}
	}
	script, err := promql_parser.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	script, err = TranspileExpressionV2(script)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range script.Substitutes {
		req, err := v.Request.Process(&shared.PlannerContext{
			From: time.Unix(0, 0), To: time.Unix(300, 0), Type: 2,
			Step:                    15 * time.Second,
			TimeSeriesGinTableName:  "time_series_gin",
			SamplesTableName:        "samples_v3",
			TimeSeriesTableName:     "time_series",
			TimeSeriesDistTableName: "time_series",
			Metrics15sTableName:     "metrics_15s",
			Metrics15sDistTableName: "metrics_15s",
		})
		if err != nil {
			t.Fatal(err)
		}
		str, err := req.String(sql.DefaultCtx())
		if err != nil {
			t.Fatal(err)
		}
		return str
	}
	return ""
}

// count(...) must be pushed down to ClickHouse just like sum(...), emitting a
// count() aggregate instead of a regular metric scan.
func TestCountPushdown(t *testing.T) {
	str := renderAggPushdown(t, `count by (job) (http_requests_total)`)
	if str == "" {
		t.Fatal("count() was not pushed down to a ClickHouse substitute")
	}
	if !strings.Contains(str, "toFloat64(count())") {
		t.Fatalf("expected toFloat64(count()) aggregate, got: %s", str)
	}
}

func TestSumPushdownStillWorks(t *testing.T) {
	str := renderAggPushdown(t, `sum by (job) (http_requests_total)`)
	if !strings.Contains(str, "sum(val)") {
		t.Fatalf("expected sum(val) aggregate, got: %s", str)
	}
}

// Ungrouped aggregation collapses to a single empty-labelled series, so the
// labels_req join against time_series must be skipped entirely.
func TestUngroupedAggSkipsLabelsJoin(t *testing.T) {
	for _, q := range []string{
		`count({__name__=~".+"})`,
		`sum({__name__=~".+"})`,
		`count by () (http_requests_total)`,
	} {
		str := renderAggPushdown(t, q)
		if strings.Contains(str, "labels_req") {
			t.Fatalf("%s: expected no labels_req join, got: %s", q, str)
		}
		// "FROM time_series " (trailing space) matches the labels scan but not the
		// "time_series_gin" fingerprint table (next char is '_').
		if strings.Contains(str, "FROM time_series ") {
			t.Fatalf("%s: expected no time_series scan, got: %s", q, str)
		}
		if !strings.Contains(str, "cityHash64('{}')") {
			t.Fatalf("%s: expected constant empty-labels fingerprint, got: %s", q, str)
		}
	}
}

// count() ignores sample values, so pre_agg must not decode them with
// argMaxMerge; sum() must still decode them.
func TestCountSkipsValueDecode(t *testing.T) {
	for _, q := range []string{`count({__name__=~".+"})`, `count by (job) (http_requests_total)`} {
		if str := renderAggPushdown(t, q); strings.Contains(str, "argMaxMerge") {
			t.Fatalf("%s: count should not decode values (argMaxMerge), got: %s", q, str)
		}
	}
	if str := renderAggPushdown(t, `sum({__name__=~".+"})`); !strings.Contains(str, "argMaxMerge(last)") {
		t.Fatalf("sum must still decode values, got: %s", str)
	}
}

// The ungrouped labels row must not re-reference pre_agg, otherwise ClickHouse
// re-inlines it and scans metrics_15s twice. It is sourced from the fp CTE.
func TestUngroupedLabelsRowAvoidsDoubleScan(t *testing.T) {
	str := renderAggPushdown(t, `count({__name__=~".+"})`)
	// "pre_agg" appears once as the CTE definition and once where the samples are
	// counted — but never a third time for the labels row.
	if n := strings.Count(str, "pre_agg"); n != 2 {
		t.Fatalf("expected pre_agg referenced exactly twice (def + count), got %d: %s", n, str)
	}
	if !strings.Contains(str, "'{}' as labels FROM fp LIMIT 1") {
		t.Fatalf("expected labels row sourced from fp, got: %s", str)
	}
}

// Grouping by labels (by / without) must still use the labels_req join.
func TestGroupedAggKeepsLabelsJoin(t *testing.T) {
	for _, q := range []string{
		`count by (job) (http_requests_total)`,
		`count without () (http_requests_total)`,
		`sum by (job) (http_requests_total)`,
	} {
		str := renderAggPushdown(t, q)
		if !strings.Contains(str, "labels_req") {
			t.Fatalf("%s: expected labels_req join for grouped aggregation, got: %s", q, str)
		}
	}
}
