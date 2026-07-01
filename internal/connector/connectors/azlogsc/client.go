package azlogsc

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/monitor/query/azlogs"
	"github.com/april/turntable/internal/engine"
)

// resolveClient lazily builds the real Log Analytics client. Auth is Azure AD
// via DefaultAzureCredential (audience api.loganalytics.io, set by the SDK).
func (c *Connector) resolveClient() (logsAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credential: %w", err)
	}
	lc, err := azlogs.NewClient(cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azlogs client: %w", err)
	}
	c.client = &realClient{lc: lc}
	return c.client, nil
}

// realClient adapts azlogs.Client to the narrow logsAPI, mapping the first
// result table's typed columns and rows into normalized form.
type realClient struct {
	lc *azlogs.Client
}

func (r *realClient) query(ctx context.Context, workspace, kql, timespan string) ([]logColumn, [][]any, error) {
	body := azlogs.QueryBody{Query: &kql}
	if timespan != "" {
		ti := azlogs.TimeInterval(timespan)
		body.Timespan = &ti
	}
	resp, err := r.lc.QueryWorkspace(ctx, workspace, body, nil)
	if err != nil {
		return nil, nil, err
	}
	if resp.Error != nil {
		return nil, nil, fmt.Errorf("query error: %s", resp.Error.Code)
	}
	if len(resp.Tables) == 0 {
		return nil, nil, nil
	}
	tbl := resp.Tables[0]
	cols := make([]logColumn, len(tbl.Columns))
	for i, c := range tbl.Columns {
		cols[i] = logColumn{name: derefStr(c.Name), typ: columnType(c.Type)}
	}
	rows := make([][]any, len(tbl.Rows))
	for i, row := range tbl.Rows {
		rows[i] = []any(row)
	}
	return cols, rows, nil
}

// columnType maps a Log Analytics column type to an engine type.
func columnType(t *azlogs.ColumnType) engine.Type {
	if t == nil {
		return engine.TypeAny
	}
	switch *t {
	case azlogs.ColumnTypeInt, azlogs.ColumnTypeLong:
		return engine.TypeInt
	case azlogs.ColumnTypeReal, azlogs.ColumnTypeDecimal:
		return engine.TypeFloat
	case azlogs.ColumnTypeBool:
		return engine.TypeBool
	case azlogs.ColumnTypeDatetime:
		return engine.TypeTime
	case azlogs.ColumnTypeString, azlogs.ColumnTypeGUID, azlogs.ColumnTypeTimespan:
		return engine.TypeString
	default: // dynamic
		return engine.TypeAny
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
