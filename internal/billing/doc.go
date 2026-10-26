// Package billing owns plans and subscriptions and answers
// Entitlement(account): what an account may have. Future payment webhooks
// land here too. It runs in otter api.
//
// billing never imports instances; instances asks billing. Plan codes and
// prices are never referenced outside this package.
//
// See docs/architecture.md §3.
package billing
