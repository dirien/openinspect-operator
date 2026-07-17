package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/daytona/clients/sdk-go/pkg/daytona"
	daytonatypes "github.com/daytona/clients/sdk-go/pkg/types"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

const (
	providerDaytona            = "daytona"
	daytonaFrameworkLabel      = "openinspect_framework"
	daytonaFrameworkLabelValue = "open-inspect"
	daytonaSessionIDLabel      = "openinspect_session_id"
)

// maxDaytonaSweepItems bounds the sandbox listing: a cursor pager that never
// terminates (or keeps returning a non-nil next cursor) must not hang the
// reconcile worker forever.
const maxDaytonaSweepItems = 10000

// sweepDaytonaSandboxes deletes Daytona sandboxes whose upstream labels match
// session IDs recorded in this CR's D1 database. The framework label alone is
// intentionally insufficient: every Open-Inspect installation shares it.
func sweepDaytonaSandboxes(
	ctx context.Context,
	d *openinspectv1alpha1.DaytonaSandboxSpec,
	apiKey string,
	ownedSessionIDs map[string]struct{},
) (int, error) {
	if len(ownedSessionIDs) == 0 {
		return 0, nil
	}
	// The SDK falls back to its own env vars / public default for an empty
	// URL — sweeping "successfully" against the wrong Daytona deployment.
	if d.APIURL == "" {
		return 0, fmt.Errorf("daytona sweep: spec.sandbox.daytona.apiUrl is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	usePolling := true
	client, err := daytona.NewClientWithConfig(&daytonatypes.DaytonaConfig{
		APIKey:               apiKey,
		APIUrl:               d.APIURL,
		Target:               d.Target,
		UseDeprecatedPolling: &usePolling,
	})
	if err != nil {
		return 0, fmt.Errorf("create daytona client: %w", err)
	}
	defer func() { _ = client.Close(ctx) }()

	// Finish the cursor-paginated listing BEFORE deleting anything: deleting
	// mid-iteration shifts pages under an offset-style cursor and skips
	// sandboxes that were owned.
	iter := client.List(ctx, &daytona.ListSandboxesQuery{
		Labels: map[string]string{daytonaFrameworkLabel: daytonaFrameworkLabelValue},
	})
	var owned []*daytona.Sandbox
	for n := 0; iter.Next(); n++ {
		if n > maxDaytonaSweepItems {
			return 0, fmt.Errorf("list daytona sandboxes: pagination never terminated")
		}
		sandbox := iter.Value()
		// Recheck labels locally even though Daytona filters server-side. This
		// keeps deletion fail-closed if a proxy or older API ignores the query.
		if sandbox.Labels[daytonaFrameworkLabel] != daytonaFrameworkLabelValue {
			continue
		}
		sessionID := sandbox.Labels[daytonaSessionIDLabel]
		if _, own := ownedSessionIDs[sessionID]; !own {
			continue
		}
		owned = append(owned, sandbox)
	}
	if err := iter.Err(); err != nil {
		return 0, fmt.Errorf("list daytona sandboxes: %w", err)
	}

	deleted := 0
	for _, sandbox := range owned {
		if err := sandbox.Delete(ctx); err != nil {
			return deleted, fmt.Errorf("delete daytona sandbox %q: %w", sandbox.ID, err)
		}
		deleted++
	}
	return deleted, nil
}
