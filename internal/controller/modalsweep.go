package controller

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	modal "github.com/modal-labs/modal-client/go"
	pb "github.com/modal-labs/modal-client/go/proto/modal_proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

// providerModal is the SANDBOX_PROVIDER value for Modal.
const providerModal = "modal"

// modalAppName is the fixed name used by the clean upstream Modal deployment.
const modalAppName = "open-inspect"

// modalDefaultEnvironment is Modal's default environment name; the CRD
// defaults spec.environment to it and it publishes without a URL suffix.
const modalDefaultEnvironment = "main"

func modalEnvironment(m *openinspectv1alpha1.ModalSandboxSpec) string {
	if m.Environment == "" {
		return modalDefaultEnvironment
	}
	return m.Environment
}

// cleanupModalApp terminates every live sandbox in the CR's Modal app and then
// stops the app deployment. It deliberately uses Modal's API rather than web
// functions added to a patched upstream checkout. appFound is false when no
// app exists under the spec's environment NAME — which the caller must surface:
// the runtime addresses the app via web-suffix URLs, so a name/suffix mismatch
// looks exactly like this and silently leaks the real app.
func cleanupModalApp(
	ctx context.Context,
	m *openinspectv1alpha1.ModalSandboxSpec,
	tokenID, tokenSecret string,
) (terminated int, appFound bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// The public Go SDK exposes sandbox lifecycle but not App.Stop. Capture the
	// authenticated connection built by the SDK, then use the generated AppStop
	// RPC that is shipped in the same module. Keeping this compatibility seam in
	// one function makes it straightforward to replace when Apps.Stop is public.
	var (
		connectionMu sync.Mutex
		connection   *grpc.ClientConn
	)
	captureConnection := func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		connectionMu.Lock()
		connection = cc
		connectionMu.Unlock()
		return invoker(ctx, method, req, reply, cc, opts...)
	}

	client, err := modal.NewClientWithOptions(&modal.ClientParams{
		TokenID:               tokenID,
		TokenSecret:           tokenSecret,
		Environment:           modalEnvironment(m),
		GRPCUnaryInterceptors: []grpc.UnaryClientInterceptor{captureConnection},
	})
	if err != nil {
		return 0, false, fmt.Errorf("create modal client: %w", err)
	}
	defer client.Close()

	app, err := client.Apps.FromName(ctx, modalAppName, &modal.AppFromNameParams{
		Environment:     modalEnvironment(m),
		CreateIfMissing: false,
	})
	if err != nil {
		var notFound modal.NotFoundError
		if errors.As(err, &notFound) || status.Code(err) == codes.NotFound {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("find modal app: %w", err)
	}

	sandboxes, err := client.Sandboxes.List(ctx, &modal.SandboxListParams{
		AppID:       app.AppID,
		Environment: modalEnvironment(m),
	})
	if err != nil {
		return 0, true, fmt.Errorf("list modal sandboxes: %w", err)
	}
	count, err := terminateModalSandboxes(ctx, sandboxes)
	terminated += count
	if err != nil {
		return terminated, true, err
	}

	// The upstream runtime may use Modal's V2 sandbox API. It has a separate
	// list endpoint, so sweep both generations before stopping the app.
	v2Sandboxes, err := client.Sandboxes.ExperimentalList(ctx, &modal.SandboxExperimentalListParams{AppID: app.AppID})
	if err != nil {
		return terminated, true, fmt.Errorf("list modal v2 sandboxes: %w", err)
	}
	count, err = terminateModalSandboxes(ctx, v2Sandboxes)
	terminated += count
	if err != nil {
		return terminated, true, err
	}

	connectionMu.Lock()
	modalConnection := connection
	connectionMu.Unlock()
	if modalConnection == nil {
		return terminated, true, fmt.Errorf("stop modal app: SDK did not expose its authenticated connection")
	}
	_, err = pb.NewModalClientClient(modalConnection).AppStop(ctx, pb.AppStopRequest_builder{
		AppId: app.AppID,
	}.Build())
	if err != nil && status.Code(err) != codes.NotFound {
		return terminated, true, fmt.Errorf("stop modal app: %w", err)
	}
	return terminated, true, nil
}

func terminateModalSandboxes(ctx context.Context, sandboxes iter.Seq2[*modal.Sandbox, error]) (int, error) {
	terminated := 0
	for sandbox, err := range sandboxes {
		if err != nil {
			return terminated, fmt.Errorf("list modal sandboxes: %w", err)
		}
		if _, err := sandbox.Terminate(ctx, nil); err != nil {
			// A sandbox may expire between listing and termination.
			if status.Code(err) == codes.NotFound {
				continue
			}
			return terminated, fmt.Errorf("terminate modal sandbox: %w", err)
		}
		terminated++
	}
	return terminated, nil
}
