// Package cloudflare wraps the cloudflare-go SDK with the idempotent
// ensure/delete operations the reconciler needs. All operations are safe to
// repeat: ensures are get-or-create by name, deletes tolerate absence.
package cloudflare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	cf "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/d1"
	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/r2"
	"github.com/cloudflare/cloudflare-go/v7/workers"
)

// Client executes Cloudflare operations for one account.
type Client struct {
	api       *cf.Client
	AccountID string
}

// New builds a client. baseURL overrides the API endpoint when non-empty
// (regional gateways, e2e mock).
func New(accountID, apiToken, baseURL string) *Client {
	opts := []option.RequestOption{option.WithAPIToken(apiToken)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &Client{api: cf.NewClient(opts...), AccountID: accountID}
}

func isNotFound(err error) bool {
	var apierr *cf.Error
	if errors.As(err, &apierr) {
		return apierr.StatusCode == 404
	}
	return false
}

// maxListItems bounds auto-pagination: the SDK pager only stops on an empty
// page, so a misbehaving API would otherwise hang the reconcile worker
// forever.
const maxListItems = 100000

// EnsureD1 returns the ID of the database with the given name, creating it if
// missing.
func (c *Client) EnsureD1(ctx context.Context, name string) (string, error) {
	iter := c.api.D1.Database.ListAutoPaging(ctx, d1.DatabaseListParams{
		AccountID: cf.F(c.AccountID),
		Name:      cf.F(name),
	})
	for n := 0; iter.Next(); n++ {
		if n > maxListItems {
			return "", fmt.Errorf("list d1 databases: pagination never terminated")
		}
		if db := iter.Current(); db.Name == name {
			return db.UUID, nil
		}
	}
	if err := iter.Err(); err != nil {
		return "", fmt.Errorf("list d1 databases: %w", err)
	}
	db, err := c.api.D1.Database.New(ctx, d1.DatabaseNewParams{
		AccountID: cf.F(c.AccountID),
		Name:      cf.F(name),
	})
	if err != nil {
		return "", fmt.Errorf("create d1 database %q: %w", name, err)
	}
	return db.UUID, nil
}

// DeleteD1 removes the database, tolerating absence.
func (c *Client) DeleteD1(ctx context.Context, databaseID string) error {
	_, err := c.api.D1.Database.Delete(ctx, databaseID, d1.DatabaseDeleteParams{
		AccountID: cf.F(c.AccountID),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete d1 database: %w", err)
	}
	return nil
}

func (c *Client) d1Query(ctx context.Context, databaseID, sql string) ([]any, error) {
	page, err := c.api.D1.Database.Query(ctx, databaseID, d1.DatabaseQueryParams{
		AccountID: cf.F(c.AccountID),
		Body:      d1.DatabaseQueryParamsBodyD1SingleQuery{Sql: cf.F(sql)},
	})
	if err != nil {
		return nil, err
	}
	var rows []any
	for _, res := range page.Result {
		rows = append(rows, res.Results...)
	}
	return rows, nil
}

var migrationPrefix = regexp.MustCompile(`^\d+`)

// ApplyMigrations applies pending migrations in key order, tracked in
// _schema_migrations by numeric filename prefix — the same contract as the
// upstream d1-migrate.sh. Returns the total number of applied migrations.
func (c *Client) ApplyMigrations(ctx context.Context, databaseID string, files map[string]string) (int, error) {
	names := make([]string, 0, len(files))
	prefixes := map[string]string{}
	for name := range files {
		v := migrationPrefix.FindString(name)
		if v == "" {
			return 0, fmt.Errorf("migration %q has no numeric prefix", name)
		}
		if prev, dup := prefixes[v]; dup {
			return 0, fmt.Errorf("duplicate migration version %s: %q and %q", v, prev, name)
		}
		prefixes[v] = name
		names = append(names, name)
	}
	sort.Strings(names)

	_, err := c.d1Query(ctx, databaseID, `CREATE TABLE IF NOT EXISTS _schema_migrations (
		version TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	if err != nil {
		return 0, fmt.Errorf("ensure _schema_migrations: %w", err)
	}

	rows, err := c.d1Query(ctx, databaseID, "SELECT version FROM _schema_migrations ORDER BY version")
	if err != nil {
		return 0, fmt.Errorf("read applied migrations: %w", err)
	}
	applied := map[string]bool{}
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok {
			if v, ok := m["version"].(string); ok {
				applied[v] = true
			}
		}
	}

	for _, name := range names {
		version := migrationPrefix.FindString(name)
		if applied[version] {
			continue
		}
		// Apply and record in ONE batch request: if this call succeeds, both
		// ran; a crash between two separate calls would re-run the migration
		// on retry.
		record := fmt.Sprintf("INSERT INTO _schema_migrations (version, name) VALUES ('%s', '%s')",
			version, strings.ReplaceAll(name, "'", "''"))
		batch := strings.TrimRight(strings.TrimSpace(files[name]), ";") + ";\n" + record
		if _, err := c.d1Query(ctx, databaseID, batch); err != nil {
			return len(applied), fmt.Errorf("apply migration %q: %w", name, err)
		}
		applied[version] = true
	}
	return len(applied), nil
}

// EnsureKV returns the namespace ID for the given title, creating it if
// missing.
func (c *Client) EnsureKV(ctx context.Context, title string) (string, error) {
	iter := c.api.KV.Namespaces.ListAutoPaging(ctx, kv.NamespaceListParams{
		AccountID: cf.F(c.AccountID),
	})
	for n := 0; iter.Next(); n++ {
		if n > maxListItems {
			return "", fmt.Errorf("list kv namespaces: pagination never terminated")
		}
		if ns := iter.Current(); ns.Title == title {
			return ns.ID, nil
		}
	}
	if err := iter.Err(); err != nil {
		return "", fmt.Errorf("list kv namespaces: %w", err)
	}
	ns, err := c.api.KV.Namespaces.New(ctx, kv.NamespaceNewParams{
		AccountID: cf.F(c.AccountID),
		Title:     cf.F(title),
	})
	if err != nil {
		return "", fmt.Errorf("create kv namespace %q: %w", title, err)
	}
	return ns.ID, nil
}

// DeleteKV removes a namespace, tolerating absence.
func (c *Client) DeleteKV(ctx context.Context, namespaceID string) error {
	_, err := c.api.KV.Namespaces.Delete(ctx, namespaceID, kv.NamespaceDeleteParams{
		AccountID: cf.F(c.AccountID),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete kv namespace: %w", err)
	}
	return nil
}

// EnsureR2 creates the bucket if missing.
func (c *Client) EnsureR2(ctx context.Context, name string) error {
	_, err := c.api.R2.Buckets.Get(ctx, name, r2.BucketGetParams{AccountID: cf.F(c.AccountID)})
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("get r2 bucket %q: %w", name, err)
	}
	_, err = c.api.R2.Buckets.New(ctx, r2.BucketNewParams{
		AccountID: cf.F(c.AccountID),
		Name:      cf.F(name),
	})
	if err != nil {
		return fmt.Errorf("create r2 bucket %q: %w", name, err)
	}
	return nil
}

// DeleteR2 removes the bucket, tolerating absence. A non-empty bucket cannot
// be deleted by the API; the caller decides whether that blocks deletion.
func (c *Client) DeleteR2(ctx context.Context, name string) error {
	_, err := c.api.R2.Buckets.Delete(ctx, name, r2.BucketDeleteParams{AccountID: cf.F(c.AccountID)})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete r2 bucket %q: %w", name, err)
	}
	return nil
}

// ListWorkerNames returns the script names that exist on the account.
func (c *Client) ListWorkerNames(ctx context.Context) (map[string]bool, error) {
	names := map[string]bool{}
	iter := c.api.Workers.Scripts.ListAutoPaging(ctx, workers.ScriptListParams{
		AccountID: cf.F(c.AccountID),
	})
	for n := 0; iter.Next(); n++ {
		if n > maxListItems {
			return nil, fmt.Errorf("list workers: pagination never terminated")
		}
		names[iter.Current().ID] = true
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	return names, nil
}

// WorkerUpload is one fully resolved worker version.
type WorkerUpload struct {
	ScriptName         string
	Script             []byte
	CompatibilityDate  string
	CompatibilityFlags []string
	Bindings           []workers.ScriptUpdateParamsMetadataBindingUnion
	// NewSqliteClasses declares Durable Object classes on first upload only.
	NewSqliteClasses []string
}

// UploadWorker uploads a version of the script (creating the worker if
// needed) and deploys it, via the classic multipart script upload.
func (c *Client) UploadWorker(ctx context.Context, up WorkerUpload) error {
	meta := workers.ScriptUpdateParamsMetadata{
		MainModule:        cf.F("index.js"),
		CompatibilityDate: cf.F(up.CompatibilityDate),
		Bindings:          cf.F(up.Bindings),
	}
	if len(up.CompatibilityFlags) > 0 {
		meta.CompatibilityFlags = cf.F(up.CompatibilityFlags)
	}
	if len(up.NewSqliteClasses) > 0 {
		meta.Migrations = cf.F[workers.ScriptUpdateParamsMetadataMigrationsUnion](
			workers.ScriptUpdateParamsMetadataMigrations{
				NewSqliteClasses: cf.F[any](up.NewSqliteClasses),
			})
	}
	module := cf.FileParam(bytes.NewReader(up.Script), "index.js", "application/javascript+module")
	_, err := c.api.Workers.Scripts.Update(ctx, up.ScriptName, workers.ScriptUpdateParams{
		AccountID: cf.F(c.AccountID),
		Metadata:  cf.F(meta),
		Files:     cf.F([]io.Reader{module.Value}),
	})
	if err != nil {
		return fmt.Errorf("upload worker %q: %w", up.ScriptName, err)
	}
	return nil
}

// SetCron replaces the cron schedules of the script.
func (c *Client) SetCron(ctx context.Context, scriptName string, crons []string) error {
	body := make([]workers.ScriptScheduleUpdateParamsBody, len(crons))
	for i, expr := range crons {
		body[i] = workers.ScriptScheduleUpdateParamsBody{Cron: cf.F(expr)}
	}
	_, err := c.api.Workers.Scripts.Schedules.Update(ctx, scriptName, workers.ScriptScheduleUpdateParams{
		AccountID: cf.F(c.AccountID),
		Body:      body,
	})
	if err != nil {
		return fmt.Errorf("set cron on %q: %w", scriptName, err)
	}
	return nil
}

// EnableSubdomain exposes the script on the workers.dev subdomain.
func (c *Client) EnableSubdomain(ctx context.Context, scriptName string) error {
	_, err := c.api.Workers.Scripts.Subdomain.New(ctx, scriptName, workers.ScriptSubdomainNewParams{
		AccountID: cf.F(c.AccountID),
		Enabled:   cf.F(true),
	})
	if err != nil {
		return fmt.Errorf("enable subdomain on %q: %w", scriptName, err)
	}
	return nil
}

// DeleteWorker removes the script, tolerating absence.
func (c *Client) DeleteWorker(ctx context.Context, scriptName string) error {
	_, err := c.api.Workers.Scripts.Delete(ctx, scriptName, workers.ScriptDeleteParams{
		AccountID: cf.F(c.AccountID),
		Force:     cf.F(true),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete worker %q: %w", scriptName, err)
	}
	return nil
}
