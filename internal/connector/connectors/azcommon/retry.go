// Package azcommon holds small helpers shared across the Azure connectors
// (azrgraphc, azcostc, azmetricsc, azlogsc, aztablesc) — the Azure analogue of
// the shared azkql renderer.
package azcommon

import (
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// RetryOptions returns a retry policy tuned for Azure throttling, for use in any
// azcore-pipeline client's options (ARM control-plane via arm.ClientOptions, or
// a data-plane client's ClientOptions — both embed policy.ClientOptions, whose
// Retry field this fills).
//
// Azure Resource Manager and the various data-plane APIs enforce aggressive
// per-tenant/per-subscription rate limits, and a single dashboard refresh fires
// many panels — hence many queries — at once, so 429s are routine. The azcore
// pipeline already retries 429/5xx and honors the server's Retry-After header,
// but its defaults give up too early for us in two ways:
//
//   - only 3 retries — a burst of throttling can outlast that; and
//   - it declines a Retry-After that exceeds MaxRetryDelay (default 60s) and
//     bails instead of waiting — yet under real throttling Azure frequently asks
//     for longer than 60s, so the client was giving up exactly when the server
//     had told it precisely how long to wait.
//
// Raising both makes a genuine server-directed backoff be respected. Everything
// else (exponential backoff, retriable-status detection, context-cancellation
// awareness) stays as the SDK provides.
func RetryOptions() policy.RetryOptions {
	return policy.RetryOptions{
		MaxRetries:    6,
		RetryDelay:    2 * time.Second,
		MaxRetryDelay: 2 * time.Minute,
	}
}
