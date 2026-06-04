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
