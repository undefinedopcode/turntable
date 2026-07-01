package azmetricsc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/monitor/armmonitor"
)

// resolveClient lazily builds the real Azure client. The subscription ID is
// parsed from the ARM resource ID; auth is Azure AD via DefaultAzureCredential
// (environment, managed identity, or az login), like aztablesc.
func (c *Connector) resolveClient(resourceURI string) (metricsAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	sub := subscriptionFromURI(resourceURI)
	if sub == "" {
		return nil, fmt.Errorf("azmetrics: resource must be a full ARM resource ID containing /subscriptions/<id>/")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credential: %w", err)
	}
	mc, err := armmonitor.NewMetricsClient(sub, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azmetrics client: %w", err)
	}
	c.client = &realClient{mc: mc}
	return c.client, nil
}

// realClient adapts armmonitor.MetricsClient to the narrow metricsAPI, mapping
// the SDK's nested Value→Timeseries→Data shape into normalized metricSeries.
type realClient struct {
	mc *armmonitor.MetricsClient
}

func (r *realClient) list(ctx context.Context, resourceURI string, q metricQuery) ([]metricSeries, error) {
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

	resp, err := r.mc.List(ctx, resourceURI, opts)
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
				s.points = append(s.points, metricPoint{ts: derefTime(dp.TimeStamp), val: pointValue(dp, q.aggregation)})
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// pointValue selects the field of a MetricValue that matches the requested
// aggregation (defaulting to Average).
func pointValue(mv *armmonitor.MetricValue, aggregation string) *float64 {
	switch aggregation {
	case "total", "sum":
		return mv.Total
	case "minimum", "min":
		return mv.Minimum
	case "maximum", "max":
		return mv.Maximum
	case "count":
		return mv.Count
	default: // average
		return mv.Average
	}
}

func localized(s *armmonitor.LocalizableString) string {
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
