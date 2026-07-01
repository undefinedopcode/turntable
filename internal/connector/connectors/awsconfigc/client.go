package awsconfigc

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/configservice"
)

// resolveClient lazily builds the real AWS Config client from the region/profile
// options, mirroring the other AWS connectors.
func (c *Connector) resolveClient(ctx context.Context, opts map[string]any) (configAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	var loadOpts []func(*config.LoadOptions) error
	if r := stringOpt(opts, "region"); r != "" {
		loadOpts = append(loadOpts, config.WithRegion(r))
	}
	if p := stringOpt(opts, "profile"); p != "" {
		loadOpts = append(loadOpts, config.WithSharedConfigProfile(p))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	c.client = &realClient{cs: configservice.NewFromConfig(cfg)}
	return c.client, nil
}

// realClient adapts the configservice client to the narrow configAPI, dispatching
// to the aggregate API when an aggregator name is given.
type realClient struct {
	cs *configservice.Client
}

func (r *realClient) query(ctx context.Context, expression, aggregator string, limit int32, nextToken string) ([]string, string, error) {
	var token *string
	if nextToken != "" {
		token = &nextToken
	}
	if aggregator != "" {
		in := &configservice.SelectAggregateResourceConfigInput{
			Expression:                  aws.String(expression),
			ConfigurationAggregatorName: aws.String(aggregator),
			NextToken:                   token,
		}
		if limit > 0 {
			in.Limit = limit
		}
		out, err := r.cs.SelectAggregateResourceConfig(ctx, in)
		if err != nil {
			return nil, "", err
		}
		return out.Results, aws.ToString(out.NextToken), nil
	}
	in := &configservice.SelectResourceConfigInput{
		Expression: aws.String(expression),
		NextToken:  token,
	}
	if limit > 0 {
		in.Limit = limit
	}
	out, err := r.cs.SelectResourceConfig(ctx, in)
	if err != nil {
		return nil, "", err
	}
	return out.Results, aws.ToString(out.NextToken), nil
}
