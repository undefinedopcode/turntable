package azmetricsc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/monitor/query/azmetrics"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/monitor/armmonitor"
)

// resolveClient lazily builds the real Azure client (once, holding the
// credential). Auth is Azure AD via DefaultAzureCredential (environment, managed
// identity, or az login), like aztablesc.
func (c *Connector) resolveClient() (metricsAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credential: %w", err)
	}
	c.client = &realClient{
		cred:  cred,
		arm:   map[string]*armmonitor.MetricsClient{},
		batch: map[string]*azmetrics.Client{},
	}
	return c.client, nil
}

// realClient adapts the Azure SDKs to the narrow metricsAPI. Per-resource
// queries use armmonitor.MetricsClient (subscription-scoped, ARM control plane);
// batch queries use azmetrics.Client (region-scoped data plane). Both are built
// lazily and cached, since each is keyed by subscription / region.
type realClient struct {
	cred  azcore.TokenCredential
	mu    sync.Mutex
	arm   map[string]*armmonitor.MetricsClient // keyed by subscription
	batch map[string]*azmetrics.Client         // keyed by region
}

// ---- per-resource (armmonitor) ----------------------------------------------

func (r *realClient) armClient(subscription string) (*armmonitor.MetricsClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mc, ok := r.arm[subscription]; ok {
		return mc, nil
	}
	mc, err := armmonitor.NewMetricsClient(subscription, r.cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azmetrics client: %w", err)
	}
	r.arm[subscription] = mc
	return mc, nil
}

func (r *realClient) list(ctx context.Context, resourceURI string, q metricQuery) ([]metricSeries, error) {
	sub := subscriptionFromURI(resourceURI)
	if sub == "" {
		return nil, fmt.Errorf("azmetrics: resource must be a full ARM resource ID containing /subscriptions/<id>/")
	}
	mc, err := r.armClient(sub)
	if err != nil {
		return nil, err
	}
	opts := &armmonitor.MetricsClientListOptions{
		Metricnames: strPtr(strings.Join(q.metricNames, ",")),
		Aggregation: strPtr(q.aggregation),
	}
	if q.interval != "" {
		opts.Interval = strPtr(q.interval)
	}
	if q.timespan != "" {
		opts.Timespan = strPtr(q.timespan)
	}
	if q.namespace != "" {
		opts.Metricnamespace = strPtr(q.namespace)
	}
	if q.filter != "" {
		opts.Filter = strPtr(q.filter)
	}
	resp, err := mc.List(ctx, resourceURI, opts)
	if err != nil {
		return nil, err
	}

	var out []metricSeries
	for _, m := range resp.Value {
		name := localized(m.Name)
		for _, ts := range m.Timeseries {
			s := metricSeries{metric: name, dimensions: map[string]string{}}
			for _, md := range ts.Metadatavalues {
				s.dimensions[localized(md.Name)] = derefStr(md.Value)
			}
			for _, dp := range ts.Data {
				s.points = append(s.points, metricPoint{ts: derefTime(dp.TimeStamp), val: armPointValue(dp, q.aggregation)})
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// ---- batch (azmetrics data plane) -------------------------------------------

func (r *realClient) batchClient(region string) (*azmetrics.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if bc, ok := r.batch[region]; ok {
		return bc, nil
	}
	endpoint := "https://" + region + ".metrics.monitor.azure.com"
	bc, err := azmetrics.NewClient(endpoint, r.cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azmetrics batch client: %w", err)
	}
	r.batch[region] = bc
	return bc, nil
}

func (r *realClient) listBatch(ctx context.Context, subscription, region string, resourceIDs []string, q metricQuery) ([]metricSeries, error) {
	bc, err := r.batchClient(region)
	if err != nil {
		return nil, err
	}
	opts := &azmetrics.QueryResourcesOptions{Aggregation: strPtr(q.aggregation)}
	if q.interval != "" {
		opts.Interval = strPtr(q.interval)
	}
	if q.filter != "" {
		opts.Filter = strPtr(q.filter)
	}
	// The batch API takes separate start/end datetimes; split the "start/end"
	// timespan the connector built.
	if start, end, ok := splitTimespan(q.timespan); ok {
		opts.StartTime = strPtr(start)
		opts.EndTime = strPtr(end)
	}
	resp, err := bc.QueryResources(ctx, subscription, q.namespace, q.metricNames, azmetrics.ResourceIDList{ResourceIDs: resourceIDs}, opts)
	if err != nil {
		return nil, err
	}

	var out []metricSeries
	for _, md := range resp.Values {
		resource := derefStr(md.ResourceID)
		for _, m := range md.Values {
			name := localizedBatch(m.Name)
			for _, ts := range m.TimeSeries {
				s := metricSeries{resource: resource, metric: name, dimensions: map[string]string{}}
				for _, mv := range ts.MetadataValues {
					s.dimensions[localizedBatch(mv.Name)] = derefStr(mv.Value)
				}
				for _, dp := range ts.Data {
					s.points = append(s.points, metricPoint{ts: derefTime(dp.TimeStamp), val: batchPointValue(dp, q.aggregation)})
				}
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// ---- aggregation field selection --------------------------------------------

func armPointValue(mv *armmonitor.MetricValue, aggregation string) *float64 {
	switch aggregation {
	case "total", "sum":
		return mv.Total
	case "minimum", "min":
		return mv.Minimum
	case "maximum", "max":
		return mv.Maximum
	case "count":
		return mv.Count
	default:
		return mv.Average
	}
}

func batchPointValue(mv azmetrics.MetricValue, aggregation string) *float64 {
	switch aggregation {
	case "total", "sum":
		return mv.Total
	case "minimum", "min":
		return mv.Minimum
	case "maximum", "max":
		return mv.Maximum
	case "count":
		return mv.Count
	default:
		return mv.Average
	}
}

// ---- helpers -----------------------------------------------------------------

// splitTimespan splits a "start/end" timespan into its two ISO datetimes.
func splitTimespan(ts string) (string, string, bool) {
	if i := strings.Index(ts, "/"); i > 0 {
		return ts[:i], ts[i+1:], true
	}
	return "", "", false
}

func localized(s *armmonitor.LocalizableString) string {
	if s == nil {
		return ""
	}
	return derefStr(s.Value)
}

func localizedBatch(s *azmetrics.LocalizableString) string {
	if s == nil {
		return ""
	}
	return derefStr(s.Value)
}

func strPtr(s string) *string { return &s }

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
