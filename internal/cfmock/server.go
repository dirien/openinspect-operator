// Package cfmock is a minimal in-memory Cloudflare API for tests. It
// implements exactly the endpoints the operator uses, with real SQLite
// backing D1 so migration semantics are genuinely exercised, and exposes
// GET /__state for assertions.
package cfmock

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type workerState struct {
	Bindings  []map[string]any `json:"bindings"`
	Cron      []string         `json:"cron"`
	Subdomain bool             `json:"subdomain"`
	Script    string           `json:"script"`
	Migration map[string]any   `json:"migrations,omitempty"`
}

// Server holds all mock account state.
type Server struct {
	mu        sync.Mutex
	d1Names   map[string]string // name -> uuid
	d1DBs     map[string]*sql.DB
	kv        map[string]string // id -> title
	r2        map[string]bool
	r2Objects map[string]map[string]bool // bucket -> object keys
	workers   map[string]*workerState
}

func New() *Server {
	return &Server{
		d1Names:   map[string]string{},
		d1DBs:     map[string]*sql.DB{},
		kv:        map[string]string{},
		r2:        map[string]bool{},
		r2Objects: map[string]map[string]bool{},
		workers:   map[string]*workerState{},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__state", s.state)
	mux.HandleFunc("GET /accounts/{account}/d1/database", s.d1List)
	mux.HandleFunc("POST /accounts/{account}/d1/database", s.d1Create)
	mux.HandleFunc("DELETE /accounts/{account}/d1/database/{id}", s.d1Delete)
	mux.HandleFunc("POST /accounts/{account}/d1/database/{id}/query", s.d1Query)
	mux.HandleFunc("GET /accounts/{account}/storage/kv/namespaces", s.kvList)
	mux.HandleFunc("POST /accounts/{account}/storage/kv/namespaces", s.kvCreate)
	mux.HandleFunc("DELETE /accounts/{account}/storage/kv/namespaces/{id}", s.kvDelete)
	mux.HandleFunc("GET /accounts/{account}/r2/buckets/{name}", s.r2Get)
	mux.HandleFunc("POST /accounts/{account}/r2/buckets", s.r2Create)
	mux.HandleFunc("DELETE /accounts/{account}/r2/buckets/{name}", s.r2Delete)
	mux.HandleFunc("GET /accounts/{account}/r2/buckets/{name}/objects", s.r2ObjectList)
	mux.HandleFunc("PUT /accounts/{account}/r2/buckets/{name}/objects/{key...}", s.r2ObjectPut)
	mux.HandleFunc("DELETE /accounts/{account}/r2/buckets/{name}/objects/{key...}", s.r2ObjectDelete)
	mux.HandleFunc("GET /accounts/{account}/workers/scripts", s.workerList)
	mux.HandleFunc("PUT /accounts/{account}/workers/scripts/{name}", s.workerUpload)
	mux.HandleFunc("DELETE /accounts/{account}/workers/scripts/{name}", s.workerDelete)
	mux.HandleFunc("PUT /accounts/{account}/workers/scripts/{name}/schedules", s.workerSchedules)
	mux.HandleFunc("POST /accounts/{account}/workers/scripts/{name}/subdomain", s.workerSubdomain)
	mux.HandleFunc("GET /accounts/{account}/workers/subdomain", s.accountSubdomain)
	return authWrap(mux)
}

// authWrap rejects requests without a bearer token, so missing credential
// wiring in the operator fails loudly in tests.
func authWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__state" && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			apiError(w, http.StatusForbidden, 9109, "missing authorization")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func ok(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{}, "messages": []any{}, "result": result,
	})
}

// okPaginated serves a V4 paginated list. Everything fits on page 1; the SDK
// auto-pager stops only on an empty page, so page >= 2 MUST return empty or
// clients loop forever.
func okPaginated(w http.ResponseWriter, r *http.Request, result []map[string]any) {
	if page := r.URL.Query().Get("page"); page != "" && page != "1" {
		result = []map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{}, "messages": []any{}, "result": result,
		"result_info": map[string]any{"page": 1, "per_page": 1000, "count": len(result), "total_count": len(result)},
	})
}

func apiError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": false, "result": nil, "messages": []any{},
		"errors": []map[string]any{{"code": code, "message": msg}},
	})
}

func (s *Server) state(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	migrations := map[string][]string{}
	for name, id := range s.d1Names {
		if db := s.d1DBs[id]; db != nil {
			rows, err := db.Query("SELECT name FROM _schema_migrations ORDER BY version")
			if err == nil {
				var applied []string
				for rows.Next() {
					var n string
					_ = rows.Scan(&n)
					applied = append(applied, n)
				}
				_ = rows.Close()
				migrations[name] = applied
			}
		}
	}
	ok(w, map[string]any{
		"d1": s.d1Names, "d1_migrations": migrations, "kv": s.kv, "r2": s.r2, "workers": s.workers,
	})
}

// --- D1 ---

func (s *Server) d1List(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filter := r.URL.Query().Get("name")
	list := []map[string]any{}
	for name, id := range s.d1Names {
		if filter == "" || name == filter {
			list = append(list, map[string]any{"name": name, "uuid": id})
		}
	}
	okPaginated(w, r, list)
}

func (s *Server) d1Create(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.d1Names[body.Name]; exists {
		apiError(w, http.StatusConflict, 7502, "database already exists")
		return
	}
	id := uuid.NewString()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		apiError(w, http.StatusInternalServerError, 7500, err.Error())
		return
	}
	db.SetMaxOpenConns(1) // in-memory sqlite: one conn == one database
	s.d1Names[body.Name] = id
	s.d1DBs[id] = db
	ok(w, map[string]any{"name": body.Name, "uuid": id})
}

func (s *Server) d1Delete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	db, exists := s.d1DBs[id]
	if !exists {
		apiError(w, http.StatusNotFound, 7404, "database not found")
		return
	}
	_ = db.Close()
	delete(s.d1DBs, id)
	for name, nid := range s.d1Names {
		if nid == id {
			delete(s.d1Names, name)
		}
	}
	ok(w, nil)
}

func (s *Server) d1Query(w http.ResponseWriter, r *http.Request) {
	var body struct{ Sql string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	db := s.d1DBs[r.PathValue("id")]
	s.mu.Unlock()
	if db == nil {
		apiError(w, http.StatusNotFound, 7404, "database not found")
		return
	}
	results := []map[string]any{}
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(body.Sql)), "SELECT") {
		rows, err := db.Query(body.Sql)
		if err != nil {
			apiError(w, http.StatusBadRequest, 7500, err.Error())
			return
		}
		defer func() { _ = rows.Close() }()
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = rows.Scan(ptrs...)
			row := map[string]any{}
			for i, c := range cols {
				row[c] = vals[i]
			}
			results = append(results, row)
		}
	} else {
		// D1 executes semicolon-joined statements as a batch.
		for stmt := range strings.SplitSeq(body.Sql, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				apiError(w, http.StatusBadRequest, 7500, fmt.Sprintf("%s: %s", err, stmt))
				return
			}
		}
	}
	ok(w, []map[string]any{{"success": true, "results": results, "meta": map[string]any{}}})
}

// --- KV ---

func (s *Server) kvList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []map[string]any{}
	for id, title := range s.kv {
		list = append(list, map[string]any{"id": id, "title": title})
	}
	okPaginated(w, r, list)
}

func (s *Server) kvCreate(w http.ResponseWriter, r *http.Request) {
	var body struct{ Title string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	s.kv[id] = body.Title
	ok(w, map[string]any{"id": id, "title": body.Title})
}

func (s *Server) kvDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	if _, exists := s.kv[id]; !exists {
		apiError(w, http.StatusNotFound, 10013, "namespace not found")
		return
	}
	delete(s.kv, id)
	ok(w, nil)
}

// --- R2 ---

func (s *Server) r2Get(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("name")
	if !s.r2[name] {
		apiError(w, http.StatusNotFound, 10006, "bucket not found")
		return
	}
	ok(w, map[string]any{"name": name})
}

func (s *Server) r2Create(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.r2[body.Name] = true
	ok(w, map[string]any{"name": body.Name})
}

func (s *Server) r2Delete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("name")
	// Real R2 refuses to delete a non-empty bucket.
	if len(s.r2Objects[name]) > 0 {
		apiError(w, http.StatusConflict, 10008, "The bucket you tried to delete is not empty")
		return
	}
	delete(s.r2, name)
	delete(s.r2Objects, name)
	ok(w, nil)
}

func (s *Server) r2ObjectList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("name")
	if !s.r2[name] {
		apiError(w, http.StatusNotFound, 10006, "bucket not found")
		return
	}
	list := []map[string]any{}
	for key := range s.r2Objects[name] {
		list = append(list, map[string]any{"key": key})
	}
	// Single page; empty cursor terminates the SDK's cursor pager.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{}, "messages": []any{}, "result": list,
		"result_info": map[string]any{"cursor": "", "per_page": 1000},
	})
}

func (s *Server) r2ObjectPut(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("name")
	if !s.r2[name] {
		apiError(w, http.StatusNotFound, 10006, "bucket not found")
		return
	}
	if s.r2Objects[name] == nil {
		s.r2Objects[name] = map[string]bool{}
	}
	s.r2Objects[name][r.PathValue("key")] = true
	ok(w, map[string]any{"key": r.PathValue("key")})
}

func (s *Server) r2ObjectDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.r2Objects[r.PathValue("name")], r.PathValue("key"))
	ok(w, nil)
}

// --- Workers ---

func (s *Server) workerList(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []map[string]any{}
	for name := range s.workers {
		list = append(list, map[string]any{"id": name})
	}
	ok(w, list)
}

func (s *Server) workerUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		apiError(w, http.StatusBadRequest, 10021, "bad multipart: "+err.Error())
		return
	}
	var meta struct {
		MainModule string           `json:"main_module"`
		Bindings   []map[string]any `json:"bindings"`
		Migrations map[string]any   `json:"migrations"`
	}
	metaValues := r.MultipartForm.Value["metadata"]
	if len(metaValues) > 0 {
		if err := json.Unmarshal([]byte(metaValues[0]), &meta); err != nil {
			apiError(w, http.StatusBadRequest, 10021, "bad metadata json")
			return
		}
	} else if fhs := r.MultipartForm.File["metadata"]; len(fhs) > 0 {
		f, _ := fhs[0].Open()
		blob, _ := io.ReadAll(f)
		_ = f.Close()
		if err := json.Unmarshal(blob, &meta); err != nil {
			apiError(w, http.StatusBadRequest, 10021, "bad metadata json")
			return
		}
	} else {
		apiError(w, http.StatusBadRequest, 10021, "missing metadata part")
		return
	}

	// The SDK posts module parts under generated field names; the real API
	// matches modules by part filename, so do the same.
	var script []byte
	for _, fhs := range r.MultipartForm.File {
		for _, fh := range fhs {
			if fh.Filename == meta.MainModule {
				f, _ := fh.Open()
				script, _ = io.ReadAll(f)
				_ = f.Close()
			}
		}
	}
	if len(script) == 0 {
		apiError(w, http.StatusBadRequest, 10021, "missing main module "+meta.MainModule)
		return
	}

	name := r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.workers[name]
	st := &workerState{Bindings: meta.Bindings, Script: string(script), Migration: meta.Migrations}
	if prev != nil {
		st.Cron = prev.Cron
		st.Subdomain = prev.Subdomain
	}
	s.workers[name] = st
	ok(w, map[string]any{"id": name})
}

func (s *Server) workerDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("name")
	if _, exists := s.workers[name]; !exists {
		apiError(w, http.StatusNotFound, 10007, "worker not found")
		return
	}
	delete(s.workers, name)
	ok(w, nil)
}

func (s *Server) workerSchedules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Body []struct{ Cron string } `json:"body"`
	}
	blob, _ := io.ReadAll(r.Body)
	// The SDK sends either {"body": [...]} or a bare array.
	if err := json.Unmarshal(blob, &body); err != nil || body.Body == nil {
		var bare []struct{ Cron string }
		if err := json.Unmarshal(blob, &bare); err != nil {
			apiError(w, http.StatusBadRequest, 10021, "bad schedules body")
			return
		}
		body.Body = bare
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	worker, exists := s.workers[r.PathValue("name")]
	if !exists {
		apiError(w, http.StatusNotFound, 10007, "worker not found")
		return
	}
	worker.Cron = []string{}
	for _, c := range body.Body {
		worker.Cron = append(worker.Cron, c.Cron)
	}
	ok(w, map[string]any{"schedules": worker.Cron})
}

func (s *Server) workerSubdomain(w http.ResponseWriter, r *http.Request) {
	var body struct{ Enabled bool }
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	worker, exists := s.workers[r.PathValue("name")]
	if !exists {
		apiError(w, http.StatusNotFound, 10007, "worker not found")
		return
	}
	worker.Subdomain = body.Enabled
	ok(w, map[string]any{"enabled": body.Enabled})
}

// accountSubdomain serves the account-level workers.dev subdomain name.
func (s *Server) accountSubdomain(w http.ResponseWriter, _ *http.Request) {
	ok(w, map[string]any{"subdomain": "cfmock", "enabled": true})
}
