package cwlogsc

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// fakeLogs is a canned logsAPI returning events across two pages.
type fakeLogs struct {
	calls int
}

func (f *fakeLogs) FilterLogEvents(ctx context.Context, in *cloudwatchlogs.FilterLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	f.calls++
	if in.NextToken == nil {
		return &cloudwatchlogs.FilterLogEventsOutput{
			Events: []cwltypes.FilteredLogEvent{
				{
					Timestamp:     aws.Int64(1000),
					Message:       aws.String("hello"),
					LogStreamName: aws.String("stream-a"),
					EventId:       aws.String("e1"),
					IngestionTime: aws.Int64(1100),
				},
				{
					Timestamp:     aws.Int64(2000),
					Message:       aws.String("world"),
					LogStreamName: aws.String("stream-b"),
					EventId:       aws.String("e2"),
					IngestionTime: aws.Int64(2100),
				},
			},
			NextToken: aws.String("page2"),
		}, nil
	}
	return &cloudwatchlogs.FilterLogEventsOutput{
		Events: []cwltypes.FilteredLogEvent{
			{
				Timestamp:     aws.Int64(3000),
				Message:       aws.String("again"),
				LogStreamName: aws.String("stream-a"),
				EventId:       aws.String("e3"),
				IngestionTime: nil, // exercise nil-pointer -> NULL
			},
		},
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
		{"message", engine.TypeString},
		{"log_stream", engine.TypeString},
		{"event_id", engine.TypeString},
		{"ingestion_time", engine.TypeTime},
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

func TestScanPaginates(t *testing.T) {
	c := newWithClient(&fakeLogs{})
	it, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{
			Source:  "my-log-group",
			Options: map[string]any{"region": "us-east-1", "filter": "ERROR"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}

	// Row 0: timestamp + message + stream + id + ingestion.
	r0 := rows[0].Values
	if !r0[0].V.(time.Time).Equal(time.UnixMilli(1000).UTC()) {
		t.Errorf("row0 timestamp = %v", r0[0].V)
	}
	if r0[1].V.(string) != "hello" {
		t.Errorf("row0 message = %v", r0[1].V)
	}
	if r0[2].V.(string) != "stream-a" {
		t.Errorf("row0 stream = %v", r0[2].V)
	}
	if r0[3].V.(string) != "e1" {
		t.Errorf("row0 event_id = %v", r0[3].V)
	}
	if !r0[4].V.(time.Time).Equal(time.UnixMilli(1100).UTC()) {
		t.Errorf("row0 ingestion = %v", r0[4].V)
	}

	// Row 2 came from page 2 and has a nil ingestion time -> NULL.
	if !rows[2].Values[4].IsNull() {
		t.Errorf("row2 ingestion should be NULL, got %+v", rows[2].Values[4])
	}
	if rows[2].Values[1].V.(string) != "again" {
		t.Errorf("row2 message = %v", rows[2].Values[1].V)
	}
}

func TestScanLimitNoPredicate(t *testing.T) {
	c := newWithClient(&fakeLogs{})
	limit := 1
	it, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{"log_group": "g"}},
		Limit:   &limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 1 {
		t.Fatalf("limit not honored: got %d rows, want 1", len(rows))
	}
}

func TestScanRequiresLogGroup(t *testing.T) {
	c := newWithClient(&fakeLogs{})
	_, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{}},
	})
	if err == nil {
		t.Fatal("expected error for missing log_group")
	}
}

func TestName(t *testing.T) {
	if New().Name() != "cloudwatchlogs" {
		t.Fatalf("Name = %q", New().Name())
	}
}
