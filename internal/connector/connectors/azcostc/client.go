package azcostc

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/costmanagement/armcostmanagement"
)

const dateLayout = "2006-01-02"

// resolveClient lazily builds the real Cost Management client. Auth is Azure AD
// via DefaultAzureCredential (env / managed identity / az login).
func (c *Connector) resolveClient() (costAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credential: %w", err)
	}
	qc, err := armcostmanagement.NewQueryClient(cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azcost client: %w", err)
	}
	c.client = &realClient{qc: qc}
	return c.client, nil
}

// realClient adapts armcostmanagement.QueryClient to the narrow costAPI, building
// the QueryDefinition and flattening the typed columns + rows.
type realClient struct {
	qc *armcostmanagement.QueryClient
}

func (r *realClient) query(ctx context.Context, scope string, def queryDef) ([]costColumn, [][]any, error) {
	sum := armcostmanagement.FunctionTypeSum
	metric := def.metric
	dataset := &armcostmanagement.QueryDataset{
		Aggregation: map[string]*armcostmanagement.QueryAggregation{
			def.metricAlias: {Function: &sum, Name: &metric},
		},
	}
	if def.granularity != "" {
		g := armcostmanagement.GranularityType(def.granularity)
		dataset.Granularity = &g
	}
	for i := range def.grouping {
		gd := def.grouping[i]
		kind := armcostmanagement.QueryColumnTypeDimension
		if gd.isTag {
			kind = armcostmanagement.QueryColumnTypeTag
		}
		name := gd.name
		dataset.Grouping = append(dataset.Grouping, &armcostmanagement.QueryGrouping{Name: &name, Type: &kind})
	}

	exportType := armcostmanagement.ExportType(def.exportType)
	timeframe := armcostmanagement.TimeframeType(def.timeframe)
	qd := armcostmanagement.QueryDefinition{
		Type:      &exportType,
		Timeframe: &timeframe,
		Dataset:   dataset,
	}
	if def.start != "" && def.end != "" {
		from, err1 := time.Parse(dateLayout, def.start)
		to, err2 := time.Parse(dateLayout, def.end)
		if err1 != nil || err2 != nil {
			return nil, nil, fmt.Errorf("start/end must be YYYY-MM-DD")
		}
		qd.TimePeriod = &armcostmanagement.QueryTimePeriod{From: &from, To: &to}
	}

	resp, err := r.qc.Usage(ctx, scope, qd, nil)
	if err != nil {
		return nil, nil, err
	}
	props := resp.Properties
	if props == nil {
		return nil, nil, nil
	}
	cols := make([]costColumn, len(props.Columns))
	for i, c := range props.Columns {
		cols[i] = costColumn{name: derefStr(c.Name), typ: costColumnType(derefStr(c.Type))}
	}
	rows := make([][]any, len(props.Rows))
	for i, row := range props.Rows {
		rows[i] = row
	}
	return cols, rows, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
