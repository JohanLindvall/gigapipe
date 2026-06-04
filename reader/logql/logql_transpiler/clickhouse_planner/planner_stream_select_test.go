package clickhouse_planner

import (
	"strings"
	"testing"
	"time"

	clconfig "github.com/metrico/cloki-config"
	clokiconfig "github.com/metrico/cloki-config/config"
	"github.com/metrico/qryn/v4/reader/config"
	"github.com/metrico/qryn/v4/reader/logql/logql_transpiler/shared"
	sql "github.com/metrico/qryn/v4/reader/utils/sql_select"
)

func streamSelectSQL(t *testing.T, p *StreamSelectPlanner) string {
	t.Helper()
	if config.Cloki == nil {
		config.Cloki = &clconfig.ClokiConfig{Setting: &clokiconfig.ClokiBaseSettingServer{}}
	}
	ctx := &shared.PlannerContext{
		From:                   time.Unix(0, 0).UTC(),
		To:                     time.Unix(300, 0).UTC(),
		Type:                   shared.SAMPLES_TYPE_METRICS,
		TimeSeriesGinTableName: "time_series_gin",
		TimeSeriesTableName:    "time_series",
	}
	req, err := p.Process(ctx)
	if err != nil {
		t.Fatal(err)
	}
	str, err := req.String(sql.DefaultCtx())
	if err != nil {
		t.Fatal(err)
	}
	return str
}

// The canonical PromQL "match all series" query count({__name__=~".+"}) must not
// compile a per-row regex; it should reduce to a cheap notEmpty(val) check.
func TestStreamSelectMatchAllNonEmpty(t *testing.T) {
	str := streamSelectSQL(t, &StreamSelectPlanner{
		LabelNames: []string{"__name__"},
		Ops:        []string{"=~"},
		Values:     []string{".+"},
	})
	if !strings.Contains(str, "notEmpty(val)") {
		t.Fatalf("expected notEmpty(val) optimization, got: %s", str)
	}
	if strings.Contains(str, "match(val") {
		t.Fatalf("did not expect a regex match for .+, got: %s", str)
	}
}

// A negated `!~".+"` selects empty values, i.e. NOT notEmpty(val).
func TestStreamSelectNegatedMatchAllNonEmpty(t *testing.T) {
	str := streamSelectSQL(t, &StreamSelectPlanner{
		LabelNames: []string{"__name__"},
		Ops:        []string{"!~"},
		Values:     []string{".+"},
	})
	if !strings.Contains(str, "notEmpty(val)") {
		t.Fatalf("expected notEmpty(val) optimization, got: %s", str)
	}
}

// Any other regex must still go through ClickHouse match().
func TestStreamSelectRealRegexUnchanged(t *testing.T) {
	str := streamSelectSQL(t, &StreamSelectPlanner{
		LabelNames: []string{"__name__"},
		Ops:        []string{"=~"},
		Values:     []string{"foo.+"},
	})
	if !strings.Contains(str, "match(val") {
		t.Fatalf("expected regex match for foo.+, got: %s", str)
	}
}
