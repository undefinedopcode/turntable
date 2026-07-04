package cwmetricsc

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// fakeMetrics is a canned metricsAPI returning data points across two pages.
type fakeMetrics struct {
	lastInput *cloudwatch.GetMetricDataInput
}

func (f *fakeMetrics) GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.lastInput = in
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if in.NextToken == nil {
		return &cloudwatch.GetMetricDataOutput{
			MetricDataResults: []cwtypes.MetricDataResult{{
				Id:         aws.String("m1"),
				Timestamps: []time.Time{t0, t0.Add(5 * time.Minute)},
				Values:     []float64{1.5, 2.5},
			}},
			NextToken: aws.String("page2"),
		}, nil
	}
	return &cloudwatch.GetMetricDataOutput{
		MetricDataResults: []cwtypes.MetricDataResult{{
			Id:         aws.String("m1"),
			Timestamps: []time.Time{t0.Add(10 * time.Minute)},
			Values:     []float64{3.5},
		}},
		NextToken: nil,
	}, nil
}

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	var rows []engine.Row
	for {
		r, ok, err := it.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		rows = append(rows, r)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return rows
}

func TestResolveSchema(t *testing.T) {
	c := New()
	sc, err := c.Resolve(context.Background(), connector.Dataset{})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name string
		typ  engine.Type
	}{
		{"timestamp", engine.TypeTime},
		{"namespace", engine.TypeString},
		{"metric", engine.TypeString},
		{"stat", engine.TypeString},
		{"value", engine.TypeFloat},
		{"label", engine.TypeString},
		{"status", engine.TypeString},
	}
	if len(sc.Columns) != len(want) {
		t.Fatalf("got %d columns, want %d", len(sc.Columns), len(want))
	}
	for i, w := range want {
		col := sc.Columns[i]
		if col.Name != w.name || col.Type != w.typ || !col.Nullable {
			t.Errorf("col %d = %+v, want {%s %v nullable}", i, col, w.name, w.typ)
		}
	}
}

func TestScanPaginatesAndBuildsQuery(t *testing.T) {
	fake := &fakeMetrics{}
	c := newWithClient(fake)
	it, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{
			"region":         "us-west-2",
			"namespace":      "AWS/EC2",
			"metric":         "CPUUtilization",
			"dim_InstanceId": "i-123",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}

	r0 := rows[0].Values
	if !r0[0].V.(time.Time).Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("row0 timestamp = %v", r0[0].V)
	}
	if r0[1].V.(string) != "AWS/EC2" {
		t.Errorf("row0 namespace = %v", r0[1].V)
	}
	if r0[2].V.(string) != "CPUUtilization" {
		t.Errorf("row0 metric = %v", r0[2].V)
	}
	if r0[3].V.(string) != "Average" {
		t.Errorf("row0 stat (default) = %v", r0[3].V)
	}
	if r0[4].V.(float64) != 1.5 {
		t.Errorf("row0 value = %v", r0[4].V)
	}
	if rows[2].Values[4].V.(float64) != 3.5 {
		t.Errorf("row2 value = %v", rows[2].Values[4].V)
	}

	// Verify the built query: default stat, default period, dimension.
	q := fake.lastInput.MetricDataQueries[0]
	if q.MetricStat == nil {
		t.Fatal("MetricStat nil")
	}
	if *q.MetricStat.Stat != "Average" {
		t.Errorf("stat = %v", *q.MetricStat.Stat)
	}
	if *q.MetricStat.Period != 300 {
		t.Errorf("period = %v", *q.MetricStat.Period)
	}
	if *q.MetricStat.Metric.Namespace != "AWS/EC2" {
		t.Errorf("namespace = %v", *q.MetricStat.Metric.Namespace)
	}
	dims := q.MetricStat.Metric.Dimensions
	if len(dims) != 1 || *dims[0].Name != "InstanceId" || *dims[0].Value != "i-123" {
		t.Errorf("dimensions = %+v", dims)
	}
}

func TestScanCustomStatAndPeriod(t *testing.T) {
	fake := &fakeMetrics{}
	c := newWithClient(fake)
	it, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{
			"namespace": "Custom",
			"metric":    "Latency",
			"stat":      "p99",
			"period":    "60",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if rows[0].Values[3].V.(string) != "p99" {
		t.Errorf("stat = %v", rows[0].Values[3].V)
	}
	if *fake.lastInput.MetricDataQueries[0].MetricStat.Period != 60 {
		t.Errorf("period = %v", *fake.lastInput.MetricDataQueries[0].MetricStat.Period)
	}
}

func TestScanLimitNoPredicate(t *testing.T) {
	c := newWithClient(&fakeMetrics{})
	limit := 2
	it, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{"namespace": "N", "metric": "M"}},
		Limit:   &limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 2 {
		t.Fatalf("limit not honored: got %d rows, want 2", len(rows))
	}
}

func TestScanRequiresNamespaceAndMetric(t *testing.T) {
	c := newWithClient(&fakeMetrics{})
	if _, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{"metric": "M"}},
	}); err == nil {
		t.Error("expected error for missing namespace")
	}
	if _, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{"namespace": "N"}},
	}); err == nil {
		t.Error("expected error for missing metric")
	}
}

func TestName(t *testing.T) {
	if New().Name() != "cloudwatch" {
		t.Fatalf("Name = %q", New().Name())
	}
}
