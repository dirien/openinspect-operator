package controller

import (
	"context"
	"fmt"
	"time"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
	daytonaprovider "github.com/dirien/openinspect-operator/internal/sandbox/provider/daytona"
)

const (
	providerDaytona            = "daytona"
	daytonaFrameworkLabel      = daytonaprovider.FrameworkLabel
	daytonaFrameworkLabelValue = daytonaprovider.FrameworkLabelValue
	daytonaSessionIDLabel      = daytonaprovider.SessionIDLabel
)

// sweepDaytonaSandboxes deletes Daytona sandboxes whose upstream labels match
// session IDs recorded in this CR's D1 database (EPIC1 D8 ownership). The
// framework label alone is intentionally insufficient: every Open-Inspect
// installation shares it.
func sweepDaytonaSandboxes(
	ctx context.Context,
	d *openinspectv1alpha1.DaytonaSandboxSpec,
	apiKey string,
	ownedSessionIDs map[string]struct{},
) (int, error) {
	if len(ownedSessionIDs) == 0 {
		return 0, nil
	}
	p, err := daytonaprovider.New(daytonaprovider.Config{
		APIKey: apiKey,
		APIURL: d.APIURL,
		Target: d.Target,
	})
	if err != nil {
		return 0, fmt.Errorf("daytona sweep: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	owned, err := p.ListOwnedSandboxes(ctx, provider.OwnerMetadata{SessionIDs: ownedSessionIDs})
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, sandbox := range owned {
		if err := p.TerminateSandbox(ctx, sandbox.ID); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}
