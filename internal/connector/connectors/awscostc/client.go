package awscostc

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

// resolveClient lazily builds the real Cost Explorer client. Cost Explorer is a
// global endpoint; it defaults to us-east-1 (overridable via the region option),
// mirroring the other AWS connectors' region/profile handling.
func (c *Connector) resolveClient(ctx context.Context, opts map[string]any) (costAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	region := stringOpt(opts, "region")
	if region == "" {
		region = "us-east-1"
	}
	loadOpts := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if p := stringOpt(opts, "profile"); p != "" {
		loadOpts = append(loadOpts, config.WithSharedConfigProfile(p))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	c.client = &realClient{ce: costexplorer.NewFromConfig(cfg)}
	return c.client, nil
}

// realClient adapts the costexplorer client to the narrow costAPI, paginating
// GetCostAndUsage and flattening ResultsByTime (Total or Groups) into costResults.
type realClient struct {
	ce *costexplorer.Client
}

func (r *realClient) get(ctx context.Context, req costRequest) ([]costResult, error) {
	in := &costexplorer.GetCostAndUsageInput{
		Granularity: cetypes.Granularity(req.granularity),
		Metrics:     req.metrics,
		TimePeriod:  &cetypes.DateInterval{Start: aws.String(req.start), End: aws.String(req.end)},
	}
	for _, g := range req.groupBy {
		in.GroupBy = append(in.GroupBy, cetypes.GroupDefinition{
			Type: cetypes.GroupDefinitionType(g.typ),
			Key:  aws.String(g.key),
		})
	}

	var out []costResult
	for {
		resp, err := r.ce.GetCostAndUsage(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, rbt := range resp.ResultsByTime {
			start, end := interval(rbt.TimePeriod)
			est := rbt.Estimated
			if len(rbt.Groups) > 0 {
				for _, g := range rbt.Groups {
					amounts, units := metricValues(g.Metrics)
					out = append(out, costResult{start: start, end: end, groups: g.Keys, amounts: amounts, units: units, estimated: est})
				}
			} else {
				amounts, units := metricValues(rbt.Total)
				out = append(out, costResult{start: start, end: end, amounts: amounts, units: units, estimated: est})
			}
		}
		if resp.NextPageToken == nil || *resp.NextPageToken == "" {
			break
		}
		in.NextPageToken = resp.NextPageToken
	}
	return out, nil
}

// metricValues extracts the per-metric amounts (parsed to float) and units from a
// Cost Explorer metric map. Each metric keeps its own unit — collapsing to one
// mislabels UsageQuantity (GB/Hrs, not a currency) and mixed-metric queries.
func metricValues(m map[string]cetypes.MetricValue) (map[string]float64, map[string]string) {
	amounts := make(map[string]float64, len(m))
	units := make(map[string]string, len(m))
	for name, mv := range m {
		amounts[name] = parseAmount(aws.ToString(mv.Amount))
		units[name] = aws.ToString(mv.Unit)
	}
	return amounts, units
}

func interval(d *cetypes.DateInterval) (time.Time, time.Time) {
	if d == nil {
		return time.Time{}, time.Time{}
	}
	start, _ := time.Parse(dateLayout, aws.ToString(d.Start))
	end, _ := time.Parse(dateLayout, aws.ToString(d.End))
	return start, end
}
