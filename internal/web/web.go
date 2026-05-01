// Package web serves a local HTTP UI for aii searches.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/store"
)

//go:embed assets/*
var assets embed.FS

func Serve(ctx context.Context, db *store.DB, addr string) error {
	mux := http.NewServeMux()

	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		apiSearch(db, w, r)
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		apiSession(db, w, r)
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		apiStats(db, w, r)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		apiSessions(db, w, r)
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: logWrap(mux), ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	log.Printf("aii web UI: http://%s", ln.Addr())

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func logWrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Truncate(time.Millisecond))
	})
}

// --- handlers ----------------------------------------------------------

func apiSearch(db *store.DB, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := q.Get("q")
	if strings.TrimSpace(query) == "" {
		writeJSON(w, []any{})
		return
	}
	agent := normalizeAgent(q.Get("agent"))
	workspace := q.Get("workspace")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var since int64
	if s := q.Get("since"); s != "" {
		t, err := parseSince(s)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		since = t
	}

	results, err := db.Search(store.Query{
		Text:      query,
		Agent:     agent,
		Workspace: workspace,
		SinceUnix: since,
		Limit:     limit,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, results)
}

func apiSession(db *store.DB, w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimPrefix(r.URL.Path, "/api/session/")
	if uid == "" {
		http.Error(w, "missing uid", 400)
		return
	}
	s, err := db.SessionByUIDAny(uid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s == nil {
		http.NotFound(w, r)
		return
	}
	rows, err := db.SessionMessages(s.ID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	type msg struct {
		Ordinal int    `json:"ordinal"`
		Role    string `json:"role"`
		TS      int64  `json:"ts"`
		Content string `json:"content"`
	}
	out := struct {
		UID       string `json:"uid"`
		Agent     string `json:"agent"`
		Workspace string `json:"workspace"`
		Title     string `json:"title"`
		Summary   string `json:"summary,omitempty"`
		StartedAt int64  `json:"started_at"`
		EndedAt   int64  `json:"ended_at"`
		Messages  []msg  `json:"messages"`
	}{UID: s.UID, Agent: s.Agent, Workspace: s.Workspace, Title: s.Title, Summary: s.Summary,
		StartedAt: s.StartedAt, EndedAt: s.EndedAt}
	for _, r := range rows {
		out.Messages = append(out.Messages, msg{Ordinal: r.Ordinal, Role: r.Role, TS: r.TS, Content: r.Content})
	}
	writeJSON(w, out)
}

// apiSessions powers the browse view: a metadata-only listing of
// sessions filtered by agent + time window. No FTS query, so an empty
// `q` in the UI can land here and show "what's recent" immediately.
func apiSessions(db *store.DB, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	agent := normalizeAgent(q.Get("agent"))
	workspace := q.Get("workspace")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	order := q.Get("order")

	var since, until int64
	if s := q.Get("since"); s != "" {
		t, err := parseSince(s)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		since = t
	}
	if s := q.Get("until"); s != "" {
		t, err := parseSince(s)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		until = t
	}

	endedMidTask := q.Get("ended_mid_task") == "1" || q.Get("ended_mid_task") == "true"
	items, err := db.ListSessions(store.SessionFilter{
		Agent:        agent,
		Workspace:    workspace,
		SinceUnix:    since,
		UntilUnix:    until,
		Limit:        limit,
		Offset:       offset,
		Order:        order,
		EndedMidTask: endedMidTask,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	total, err := db.CountSessions(store.SessionFilter{
		Agent: agent, Workspace: workspace, SinceUnix: since, UntilUnix: until,
		EndedMidTask: endedMidTask,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	type item struct {
		UID          string `json:"uid"`
		Agent        string `json:"agent"`
		Title        string `json:"title"`
		Summary      string `json:"summary,omitempty"`
		Workspace    string `json:"workspace"`
		StartedAt    int64  `json:"started_at"`
		EndedAt      int64  `json:"ended_at"`
		MessageCount int64  `json:"message_count"`
	}
	out := struct {
		Total int64  `json:"total"`
		Items []item `json:"items"`
	}{Total: total, Items: make([]item, 0, len(items))}
	for _, s := range items {
		out.Items = append(out.Items, item{
			UID: s.UID, Agent: s.Agent, Title: s.Title, Summary: s.Summary,
			Workspace: s.Workspace, StartedAt: s.StartedAt, EndedAt: s.EndedAt,
			MessageCount: s.MessageCount,
		})
	}
	writeJSON(w, out)
}

func apiStats(db *store.DB, w http.ResponseWriter, _ *http.Request) {
	ss, err := db.Stats()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, ss)
}

// --- helpers -----------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("encode: %v", err)
	}
}

func normalizeAgent(a string) string {
	switch strings.ToLower(a) {
	case "", "all":
		return ""
	case "cc", "claude", "claude_code":
		return "claude_code"
	case "codex":
		return "codex"
	case "cursor":
		return "cursor"
	}
	return a
}

func parseSince(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if len(s) > 1 {
		unit := s[len(s)-1]
		if unit == 'd' || unit == 'h' || unit == 'm' || unit == 'w' {
			var n int
			if _, err := fmt.Sscanf(s[:len(s)-1], "%d", &n); err == nil && n > 0 {
				var dur time.Duration
				switch unit {
				case 'm':
					dur = time.Duration(n) * time.Minute
				case 'h':
					dur = time.Duration(n) * time.Hour
				case 'd':
					dur = time.Duration(n) * 24 * time.Hour
				case 'w':
					dur = time.Duration(n) * 7 * 24 * time.Hour
				}
				return time.Now().Add(-dur).Unix(), nil
			}
		}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Unix(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("invalid since %q", s)
}
