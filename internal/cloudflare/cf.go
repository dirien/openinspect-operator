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
	"net/http"
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
		return apierr.StatusCode == http.StatusNotFound
	}
	return false
}

// maxListItems bounds auto-pagination: the SDK pager only stops on an empty
// page, so a misbehaving API would otherwise hang the reconcile worker
// forever.
const maxListItems = 100000

// EnsureD1 returns the ID of the database with the given name, creating it if
// missing. created reports whether this call created it, so the caller can
// distinguish adoption of a foreign database from normal reconciliation.
func (c *Client) EnsureD1(ctx context.Context, name string) (id string, created bool, err error) {
	iter := c.api.D1.Database.ListAutoPaging(ctx, d1.DatabaseListParams{
		AccountID: cf.F(c.AccountID),
		Name:      cf.F(name),
	})
	for n := 0; iter.Next(); n++ {
		if n > maxListItems {
			return "", false, fmt.Errorf("list d1 databases: pagination never terminated")
		}
		if db := iter.Current(); db.Name == name {
			return db.UUID, false, nil
		}
	}
	if err := iter.Err(); err != nil {
		return "", false, fmt.Errorf("list d1 databases: %w", err)
	}
	db, err := c.api.D1.Database.New(ctx, d1.DatabaseNewParams{
		AccountID: cf.F(c.AccountID),
		Name:      cf.F(name),
	})
	if err != nil {
		return "", false, fmt.Errorf("create d1 database %q: %w", name, err)
	}
	return db.UUID, true, nil
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

// D1SessionIDs returns the session IDs recorded by this Open-Inspect
// deployment. Provider finalizers use these IDs as ownership evidence for
// sandboxes created dynamically by the control-plane worker. found is false
// when the evidence cannot exist — database gone or sessions table never
// created — so deletion can degrade to a skipped sweep instead of wedging
// the finalizer on its own missing database.
func (c *Client) D1SessionIDs(ctx context.Context, databaseID string) (ids []string, found bool, err error) {
	rows, err := c.d1Query(ctx, databaseID, "SELECT id FROM sessions ORDER BY id")
	if err != nil {
		if isNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read session IDs from d1: %w", err)
	}
	ids = make([]string, 0, len(rows))
	for _, row := range rows {
		values, ok := row.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("read session IDs from d1: unexpected row type %T", row)
		}
		id, ok := values["id"].(string)
		if !ok || id == "" {
			return nil, false, fmt.Errorf("read session IDs from d1: row has no string id")
		}
		ids = append(ids, id)
	}
	return ids, true, nil
}

// ImageRef is one provider prebuilt image recorded by the platform.
type ImageRef struct {
	Provider string
	ImageID  string
}

// D1ImageBuildRefs returns the provider image references recorded in this
// deployment's image_builds table (repo/environment prebuilds). Finalizers
// delete these provider-side artifacts before D1 itself goes away. found is
// false when the table or database does not exist — older schemas or a
// vanished database degrade to a skipped image sweep instead of wedging the
// finalizer.
func (c *Client) D1ImageBuildRefs(ctx context.Context, databaseID string) (refs []ImageRef, found bool, err error) {
	rows, err := c.d1Query(ctx, databaseID,
		"SELECT DISTINCT provider, provider_image_id FROM image_builds WHERE provider_image_id IS NOT NULL AND provider_image_id != ''")
	if err != nil {
		if isNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read image builds from d1: %w", err)
	}
	for _, row := range rows {
		values, ok := row.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("read image builds from d1: unexpected row type %T", row)
		}
		provider, _ := values["provider"].(string)
		imageID, _ := values["provider_image_id"].(string)
		if provider == "" || imageID == "" {
			continue
		}
		refs = append(refs, ImageRef{Provider: provider, ImageID: imageID})
	}
	return refs, true, nil
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

// ListKVTitles returns title -> namespace ID for the whole account: one paged
// list per reconcile instead of one per namespace.
func (c *Client) ListKVTitles(ctx context.Context) (map[string]string, error) {
	titles := map[string]string{}
	iter := c.api.KV.Namespaces.ListAutoPaging(ctx, kv.NamespaceListParams{
		AccountID: cf.F(c.AccountID),
	})
	for n := 0; iter.Next(); n++ {
		if n > maxListItems {
			return nil, fmt.Errorf("list kv namespaces: pagination never terminated")
		}
		ns := iter.Current()
		titles[ns.Title] = ns.ID
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("list kv namespaces: %w", err)
	}
	return titles, nil
}

// CreateKV creates the namespace and returns its ID.
func (c *Client) CreateKV(ctx context.Context, title string) (string, error) {
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

// EnsureR2 creates the bucket if missing. created reports whether this call
// created it.
func (c *Client) EnsureR2(ctx context.Context, name string) (created bool, err error) {
	_, err = c.api.R2.Buckets.Get(ctx, name, r2.BucketGetParams{AccountID: cf.F(c.AccountID)})
	if err == nil {
		return false, nil
	}
	if !isNotFound(err) {
		return false, fmt.Errorf("get r2 bucket %q: %w", name, err)
	}
	_, err = c.api.R2.Buckets.New(ctx, r2.BucketNewParams{
		AccountID: cf.F(c.AccountID),
		Name:      cf.F(name),
	})
	if err != nil {
		return false, fmt.Errorf("create r2 bucket %q: %w", name, err)
	}
	return true, nil
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

// PurgeR2 deletes every object in the bucket so DeleteR2 can succeed.
// Tolerates an absent bucket. Returns the number of objects deleted.
func (c *Client) PurgeR2(ctx context.Context, name string) (int, error) {
	deleted := 0
	for {
		iter := c.api.R2.Buckets.Objects.ListAutoPaging(ctx, name, r2.BucketObjectListParams{
			AccountID: cf.F(c.AccountID),
		})
		var keys []string
		for n := 0; iter.Next(); n++ {
			if n > maxListItems {
				return deleted, fmt.Errorf("purge r2 bucket %q: pagination never terminated", name)
			}
			keys = append(keys, iter.Current().Key)
		}
		if err := iter.Err(); err != nil {
			if isNotFound(err) {
				return deleted, nil
			}
			return deleted, fmt.Errorf("list objects in r2 bucket %q: %w", name, err)
		}
		if len(keys) == 0 {
			return deleted, nil
		}
		for _, key := range keys {
			_, err := c.api.R2.Buckets.Objects.Delete(ctx, name, key, r2.BucketObjectDeleteParams{
				AccountID: cf.F(c.AccountID),
			})
			if err != nil && !isNotFound(err) {
				return deleted, fmt.Errorf("delete object %q from r2 bucket %q: %w", key, name, err)
			}
			deleted++
		}
	}
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

// AccountSubdomain returns the account's workers.dev subdomain name.
func (c *Client) AccountSubdomain(ctx context.Context) (string, error) {
	res, err := c.api.Workers.Subdomains.Get(ctx, workers.SubdomainGetParams{
		AccountID: cf.F(c.AccountID),
	})
	if err != nil {
		return "", fmt.Errorf("get workers.dev subdomain: %w", err)
	}
	return res.Subdomain, nil
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

// IsDOClassExists reports Cloudflare error 10074: a new-class migration
// naming a Durable Object class that already has live objects.
func IsDOClassExists(err error) bool {
	var apierr *cf.Error
	if errors.As(err, &apierr) {
		for _, e := range apierr.Errors {
			if e.Code == 10074 {
				return true
			}
		}
	}
	return false
}

// DeleteWorker removes the script, tolerating absence. Force also removes
// the script's Durable Object namespaces and their storage — the same
// semantics wrangler and the Terraform provider rely on.
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
