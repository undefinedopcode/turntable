package azcostc

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/costmanagement/armcostmanagement"

	"github.com/april/turntable/internal/connector/connectors/azcommon"
)

const dateLayout = "2006-01-02"

// Module identity for the raw pipeline used to follow NextLink pages; only feeds
// the telemetry/User-Agent header, so the exact value is not load-bearing.
const (
	armModuleName    = "armcostmanagement.QueryClient"
	armModuleVersion = "v1.1.1"
)

// retryOptions wraps the shared Azure retry policy (see azcommon.RetryOptions —
// tuned for ARM throttling on dashboard refreshes) as ARM client options.
func retryOptions() *arm.ClientOptions {
	return &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{Retry: azcommon.RetryOptions()},
	}
}

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
	qc, err := armcostmanagement.NewQueryClient(cred, retryOptions())
	if err != nil {
		return nil, fmt.Errorf("azcost client: %w", err)
	}
	// A second, generic ARM client whose pipeline (auth + retry) we reuse to POST
	// to a page's NextLink URL — the Query API has no pager, so continuation is a
	// re-POST of the same body to that URL.
	armClient, err := arm.NewClient(armModuleName, armModuleVersion, cred, retryOptions())
	if err != nil {
		return nil, fmt.Errorf("azcost pipeline: %w", err)
	}
	c.client = &realClient{qc: qc, pipeline: armClient.Pipeline()}
	return c.client, nil
}

// realClient adapts armcostmanagement.QueryClient to the narrow costAPI, building
// the QueryDefinition and flattening the typed columns + rows. It also holds a
// raw ARM pipeline for following NextLink continuation pages.
type realClient struct {
	qc       *armcostmanagement.QueryClient
	pipeline runtime.Pipeline
}

func (r *realClient) query(ctx context.Context, scope string, def queryDef, nextLink string) ([]costColumn, [][]any, string, error) {
	qd, err := buildQueryDef(def)
	if err != nil {
		return nil, nil, "", err
	}

	var props *armcostmanagement.QueryProperties
	if nextLink == "" {
		resp, err := r.qc.Usage(ctx, scope, qd, nil)
		if err != nil {
			return nil, nil, "", err
		}
		props = resp.Properties
	} else {
		props, err = r.usagePage(ctx, nextLink, qd)
		if err != nil {
			return nil, nil, "", err
		}
	}
	if props == nil {
		return nil, nil, "", nil
	}

	cols := make([]costColumn, len(props.Columns))
	for i, c := range props.Columns {
		cols[i] = costColumn{name: derefStr(c.Name), typ: costColumnType(derefStr(c.Type))}
	}
	rows := make([][]any, len(props.Rows))
	for i, row := range props.Rows {
		rows[i] = row
	}
	return cols, rows, derefStr(props.NextLink), nil
}

// usagePage fetches a continuation page by POSTing the same query body to the
// NextLink URL through the ARM pipeline (which applies auth + the retry policy).
func (r *realClient) usagePage(ctx context.Context, nextLink string, qd armcostmanagement.QueryDefinition) (*armcostmanagement.QueryProperties, error) {
	req, err := runtime.NewRequest(ctx, http.MethodPost, nextLink)
	if err != nil {
		return nil, err
	}
	if err := runtime.MarshalAsJSON(req, qd); err != nil {
		return nil, err
	}
	resp, err := r.pipeline.Do(req)
	if err != nil {
		return nil, err
	}
	if !runtime.HasStatusCode(resp, http.StatusOK) {
		return nil, runtime.NewResponseError(resp)
	}
	var qr armcostmanagement.QueryResult
	if err := runtime.UnmarshalAsJSON(resp, &qr); err != nil {
		return nil, err
	}
	return qr.Properties, nil
}

// buildQueryDef translates a normalized queryDef into the SDK's QueryDefinition.
func buildQueryDef(def queryDef) (armcostmanagement.QueryDefinition, error) {
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
			return armcostmanagement.QueryDefinition{}, fmt.Errorf("start/end must be YYYY-MM-DD")
		}
		qd.TimePeriod = &armcostmanagement.QueryTimePeriod{From: &from, To: &to}
	}
	return qd, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
