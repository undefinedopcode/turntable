package azrgraphc

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resourcegraph/armresourcegraph"

	"github.com/april/turntable/internal/connector/connectors/azcommon"
)

// retryOptions wraps the shared Azure retry policy (see azcommon.RetryOptions —
// tuned for ARM throttling on dashboard refreshes) as ARM client options.
func retryOptions() *arm.ClientOptions {
	return &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{Retry: azcommon.RetryOptions()},
	}
}

// resolveClient lazily builds the real Resource Graph client. Auth is Azure AD
// via DefaultAzureCredential (environment, managed identity, or az login), like
// aztablesc; no subscription is needed at construction.
func (c *Connector) resolveClient() (graphAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credential: %w", err)
	}
	gc, err := armresourcegraph.NewClient(cred, retryOptions())
	if err != nil {
		return nil, fmt.Errorf("azrgraph client: %w", err)
	}
	c.client = &realClient{gc: gc}
	return c.client, nil
}

// realClient adapts armresourcegraph.Client to the narrow graphAPI, requesting
// the objectArray result format and decoding Data (an array of objects) into
// []map[string]any.
type realClient struct {
	gc *armresourcegraph.Client
}

func (r *realClient) query(ctx context.Context, subscriptions []string, kql string, top int32, skipToken string) ([]map[string]any, string, error) {
	format := armresourcegraph.ResultFormatObjectArray
	opts := &armresourcegraph.QueryRequestOptions{ResultFormat: &format}
	if top > 0 {
		opts.Top = &top
	}
	if skipToken != "" {
		opts.SkipToken = &skipToken
	}
	req := armresourcegraph.QueryRequest{
		Query:         &kql,
		Options:       opts,
		Subscriptions: strPtrs(subscriptions),
	}
	resp, err := r.gc.Resources(ctx, req, nil)
	if err != nil {
		return nil, "", err
	}
	rows := decodeObjectArray(resp.Data)
	next := ""
	if resp.SkipToken != nil {
		next = *resp.SkipToken
	}
	return rows, next, nil
}

// decodeObjectArray coerces the objectArray Data payload (an []any of
// map[string]any) into typed maps, tolerating the empty/nil case.
func decodeObjectArray(data any) []map[string]any {
	arr, ok := data.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, el := range arr {
		if m, ok := el.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func strPtrs(ss []string) []*string {
	if len(ss) == 0 {
		return nil
	}
	out := make([]*string, len(ss))
	for i := range ss {
		s := ss[i]
		out[i] = &s
	}
	return out
}
