// Package modal implements the operator-side Modal provider: named-secret
// convergence, deployed-app endpoint discovery and health, and finalization
// (sandbox termination + app stop). It deliberately uses Modal's Go SDK and
// its authenticated API — never web functions added to a patched upstream
// checkout (EPIC1 D2). App deployment itself is NOT here: Modal Functions
// cannot be deployed through the Go SDK, which is exactly the scoped D5
// exception the modal-deploy Job exists for.
package modal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	modal "github.com/modal-labs/modal-client/go"
	pb "github.com/modal-labs/modal-client/go/proto/modal_proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AppName is the fixed name used by the clean upstream Modal deployment.
const AppName = "open-inspect"

// DefaultEnvironment is Modal's default environment name; it publishes
// without a URL suffix.
const DefaultEnvironment = "main"

// The three named secrets the unmodified upstream app resolves at deploy
// time (packages/modal-infra/src/app.py).
const (
	SecretLLMAPIKeys  = "llm-api-keys" //nolint:gosec // secret NAME from upstream app.py, not a credential
	SecretGitHubApp   = "github-app"   //nolint:gosec // secret NAME from upstream app.py, not a credential
	SecretInternalAPI = "internal-api"
)

// Config carries explicit credentials; nothing is read from process
// environment.
type Config struct {
	TokenID     string
	TokenSecret string
	Workspace   string
	Environment string
	// Logf receives progress lines; nil discards them. Never log credential
	// values through it.
	Logf func(format string, args ...any)
}

// Provider is the Modal adapter.
type Provider struct {
	cfg Config
}

// New validates the configuration and returns the adapter.
func New(cfg Config) (*Provider, error) {
	if cfg.TokenID == "" || cfg.TokenSecret == "" {
		return nil, fmt.Errorf("modal: token id/secret are required")
	}
	// The SDK also consults ambient configuration the ClientParams cannot
	// override (profiles, MODAL_SERVER_URL). Credentials are per-CR; refuse
	// to run where ambient state could redirect them to another deployment.
	for _, env := range []string{"MODAL_SERVER_URL", "MODAL_PROFILE", "MODAL_CONFIG_PATH"} {
		if os.Getenv(env) != "" {
			return nil, fmt.Errorf("modal: refusing to run with %s set: ambient modal configuration could redirect per-CR credentials", env)
		}
	}
	if cfg.Environment == "" {
		cfg.Environment = DefaultEnvironment
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Provider{cfg: cfg}, nil
}

// session builds an authenticated SDK client plus access to the raw proto
// client on the same connection. The public Go SDK does not expose every RPC
// the operator needs (named-secret creation, FunctionGet metadata, AppStop);
// capturing the SDK's authenticated connection through an interceptor keeps
// those calls on official, authenticated channels without a parallel auth
// implementation. rawClient is only usable after the first SDK call on the
// session has forced a connection.
type session struct {
	client *modal.Client
	connMu sync.Mutex
	conn   *grpc.ClientConn
}

func (p *Provider) newSession() (*session, error) {
	s := &session{}
	capture := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		// Capture only the FIRST connection: every session's first RPC runs
		// on the SDK's control-plane client, and later SDK versions may add
		// side connections (input plane, command router) that must never
		// hijack the raw control-plane calls.
		s.connMu.Lock()
		if s.conn == nil {
			s.conn = cc
		}
		s.connMu.Unlock()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	client, err := modal.NewClientWithOptions(&modal.ClientParams{
		TokenID:               p.cfg.TokenID,
		TokenSecret:           p.cfg.TokenSecret,
		Environment:           p.cfg.Environment,
		GRPCUnaryInterceptors: []grpc.UnaryClientInterceptor{capture},
	})
	if err != nil {
		return nil, fmt.Errorf("create modal client: %w", err)
	}
	s.client = client
	return s, nil
}

func (s *session) raw() (pb.ModalClientClient, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn == nil {
		return nil, fmt.Errorf("modal: no authenticated connection captured yet")
	}
	return pb.NewModalClientClient(s.conn), nil
}

func (s *session) close() { s.client.Close() }

// EnsureNamedSecrets creates or overwrites the named Modal secrets in the
// configured environment — the SDK equivalent of upstream's
// `modal secret create <name> KEY=VALUE... --force`.
func (p *Provider) EnsureNamedSecrets(ctx context.Context, secrets map[string]map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	s, err := p.newSession()
	if err != nil {
		return err
	}
	defer s.close()

	// Any SDK call forces the authenticated connection; probing our own app
	// name (NotFound is fine) has no side effects.
	if _, err := s.client.Apps.FromName(ctx, AppName, &modal.AppFromNameParams{Environment: p.cfg.Environment}); err != nil {
		var notFound modal.NotFoundError
		if !errors.As(err, &notFound) && status.Code(err) != codes.NotFound {
			return fmt.Errorf("modal connection probe: %w", err)
		}
	}
	raw, err := s.raw()
	if err != nil {
		return err
	}
	for name, values := range secrets {
		_, err := raw.SecretGetOrCreate(ctx, pb.SecretGetOrCreateRequest_builder{
			DeploymentName:     name,
			EnvironmentName:    p.cfg.Environment,
			EnvDict:            values,
			ObjectCreationType: pb.ObjectCreationType_OBJECT_CREATION_TYPE_CREATE_OVERWRITE_IF_EXISTS,
		}.Build())
		if err != nil {
			return fmt.Errorf("ensure modal secret %q: %w", name, err)
		}
		p.cfg.Logf("modal secret %s converged (%d keys)", name, len(values))
	}
	return nil
}

// Endpoints is what the operator discovers from the deployed app.
type Endpoints struct {
	AppID string
	// HealthURL is the api-health web endpoint.
	HealthURL string
	// WebSuffix is the environment suffix embedded in *.modal.run URLs
	// ("" for the default environment).
	WebSuffix string
}

// ErrAppNotFound reports that the upstream app is not deployed in the
// configured environment.
var ErrAppNotFound = errors.New("modal app not found")

// DiscoverEndpoints resolves the deployed app and derives the endpoint
// web suffix from the api_health function's actual web URL — the operator
// never guesses URL shapes.
func (p *Provider) DiscoverEndpoints(ctx context.Context) (Endpoints, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	s, err := p.newSession()
	if err != nil {
		return Endpoints{}, err
	}
	defer s.close()

	app, err := s.client.Apps.FromName(ctx, AppName, &modal.AppFromNameParams{Environment: p.cfg.Environment})
	if err != nil {
		var notFound modal.NotFoundError
		if errors.As(err, &notFound) || status.Code(err) == codes.NotFound {
			return Endpoints{}, ErrAppNotFound
		}
		return Endpoints{}, fmt.Errorf("find modal app: %w", err)
	}
	raw, err := s.raw()
	if err != nil {
		return Endpoints{}, err
	}
	fn, err := raw.FunctionGet(ctx, pb.FunctionGetRequest_builder{
		AppName:         AppName,
		ObjectTag:       "api_health",
		EnvironmentName: p.cfg.Environment,
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return Endpoints{}, ErrAppNotFound
		}
		return Endpoints{}, fmt.Errorf("resolve api_health function: %w", err)
	}
	webURL := fn.GetHandleMetadata().GetWebUrl()
	if webURL == "" {
		return Endpoints{}, fmt.Errorf("api_health function has no web URL")
	}
	suffix, err := p.webSuffix(webURL)
	if err != nil {
		return Endpoints{}, err
	}
	return Endpoints{AppID: app.AppID, HealthURL: webURL, WebSuffix: suffix}, nil
}

// webSuffix extracts the environment web suffix from a deployed function
// URL (https://<workspace>[-<suffix>]--open-inspect-api-health.modal.run)
// and cross-checks the workspace, so a wrong spec.workspace surfaces here
// instead of producing dead URLs in the control plane.
func (p *Provider) webSuffix(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse function URL %q: %w", rawURL, err)
	}
	host := u.Hostname()
	slug, _, ok := strings.Cut(host, "--")
	if !ok {
		return "", fmt.Errorf("function URL %q has no workspace slug", rawURL)
	}
	switch {
	case slug == p.cfg.Workspace:
		return "", nil
	case p.cfg.Environment == DefaultEnvironment:
		// The default environment publishes WITHOUT a suffix, so any extra
		// slug content means the configured workspace is wrong (e.g.
		// workspace "acme" against a real workspace "acme-corp") — never
		// misread it as an environment suffix.
		return "", fmt.Errorf("deployed app URL slug %q does not match spec workspace %q (the default environment publishes without a suffix): fix spec.sandbox.modal.workspace", slug, p.cfg.Workspace)
	case strings.HasPrefix(slug, p.cfg.Workspace+"-"):
		return strings.TrimPrefix(slug, p.cfg.Workspace+"-"), nil
	default:
		return "", fmt.Errorf("deployed app URL slug %q does not match spec workspace %q: fix spec.sandbox.modal.workspace", slug, p.cfg.Workspace)
	}
}

// HealthCheck calls the deployed app's health endpoint.
func HealthCheck(ctx context.Context, healthURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check %s: %w", healthURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check %s: HTTP %d: %.200s", healthURL, resp.StatusCode, body)
	}
	return nil
}

// DeleteImage deletes one Modal image by ID, tolerating absence. Used for
// the platform's recorded repo/environment prebuild images during
// finalization.
func (p *Provider) DeleteImage(ctx context.Context, imageID string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	s, err := p.newSession()
	if err != nil {
		return err
	}
	defer s.close()
	err = s.client.Images.Delete(ctx, imageID, nil)
	if err != nil {
		var notFound modal.NotFoundError
		if errors.As(err, &notFound) || status.Code(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("delete modal image %q: %w", imageID, err)
	}
	return nil
}

// TerminateAndStop terminates every live sandbox (v1 and v2 listings) in
// the CR's Modal app and stops the app deployment. appFound is false when
// no app exists under the configured environment — callers must surface
// that instead of claiming a stop that never ran.
func (p *Provider) TerminateAndStop(ctx context.Context) (terminated int, appFound bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	s, err := p.newSession()
	if err != nil {
		return 0, false, err
	}
	defer s.close()

	app, err := s.client.Apps.FromName(ctx, AppName, &modal.AppFromNameParams{
		Environment:     p.cfg.Environment,
		CreateIfMissing: false,
	})
	if err != nil {
		var notFound modal.NotFoundError
		if errors.As(err, &notFound) || status.Code(err) == codes.NotFound {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("find modal app: %w", err)
	}

	sandboxes, err := s.client.Sandboxes.List(ctx, &modal.SandboxListParams{
		AppID:       app.AppID,
		Environment: p.cfg.Environment,
	})
	if err != nil {
		return 0, true, fmt.Errorf("list modal sandboxes: %w", err)
	}
	count, err := terminateAll(ctx, sandboxes)
	terminated += count
	if err != nil {
		return terminated, true, err
	}

	// The upstream runtime may use Modal's V2 sandbox API. It has a separate
	// list endpoint, so sweep both generations before stopping the app.
	v2, err := s.client.Sandboxes.ExperimentalList(ctx, &modal.SandboxExperimentalListParams{AppID: app.AppID})
	if err != nil {
		return terminated, true, fmt.Errorf("list modal v2 sandboxes: %w", err)
	}
	count, err = terminateAll(ctx, v2)
	terminated += count
	if err != nil {
		return terminated, true, err
	}

	raw, err := s.raw()
	if err != nil {
		return terminated, true, fmt.Errorf("stop modal app: %w", err)
	}
	_, err = raw.AppStop(ctx, pb.AppStopRequest_builder{AppId: app.AppID}.Build())
	if err != nil && status.Code(err) != codes.NotFound {
		return terminated, true, fmt.Errorf("stop modal app: %w", err)
	}
	return terminated, true, nil
}

func terminateAll(ctx context.Context, sandboxes iter.Seq2[*modal.Sandbox, error]) (int, error) {
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
