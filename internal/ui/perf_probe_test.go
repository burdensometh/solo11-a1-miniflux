//go:build perfprobe

// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The perfprobe tests are not run by the default suite. They need a migrated
// PostgreSQL database (defaults to the local instance created for this work,
// override with PROBE_DATABASE_URL) and the pg_stat_statements extension, and
// they mutate database contents:
//
//	go test -tags perfprobe -count=1 -v -run TestPerfProbe ./internal/ui
//
// PROBE_HTML_DIR optionally stores normalized rendered pages so before/after
// runs can be compared byte for byte.
package ui

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/database"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/template"
)

func probeQueryCount(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var n int64
	err := db.QueryRow(`
		SELECT coalesce(sum(calls), 0)
		FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query NOT ILIKE '%pg_stat_statements%'
	`).Scan(&n)
	if err != nil {
		t.Fatalf("query count: %v", err)
	}
	return n
}

func normalizeHTML(s string) string {
	s = regexp.MustCompile(`nonce="[^"]+"`).ReplaceAllString(s, `nonce="X"`)
	s = regexp.MustCompile(`'nonce-[A-Za-z0-9]+'`).ReplaceAllString(s, `'nonce-X'`)
	s = regexp.MustCompile(`data-csrf-token="[^"]+"`).ReplaceAllString(s, `data-csrf-token="X"`)
	s = regexp.MustCompile(`name="csrf" value="[^"]+"`).ReplaceAllString(s, `name="csrf" value="X"`)
	s = regexp.MustCompile(`2026-\d\d-\d\d \d\d:\d\d:\d\d`).ReplaceAllString(s, `TIMESTAMP`)
	s = regexp.MustCompile(`<meta http-equiv="Content-Security-Policy" content="([^"]*)">`).ReplaceAllStringFunc(s, func(m string) string {
		inner := regexp.MustCompile(`content="([^"]*)"`).FindStringSubmatch(m)[1]
		parts := strings.Split(inner, ";")
		trimmed := make([]string, 0, len(parts))
		for _, p := range parts {
			if strings.TrimSpace(p) != "" {
				trimmed = append(trimmed, strings.TrimSpace(p))
			}
		}
		slices.Sort(trimmed)
		return `<meta http-equiv="Content-Security-Policy" content="` + strings.Join(trimmed, " ; ") + `">`
	})
	return s
}

func TestPerfProbeQueryCounts(t *testing.T) {
	dsn := os.Getenv("PROBE_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://miniflux@127.0.0.1:55432/miniflux_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`TRUNCATE users, categories, feeds, entries, icons, feed_icons, enclosures RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}

	var userID, categoryID int64
	if err := db.QueryRow(`INSERT INTO users (username) VALUES ('probe') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO categories (user_id, title) VALUES ($1, 'probe-cat') RETURNING id`, userID).Scan(&categoryID); err != nil {
		t.Fatal(err)
	}

	const n = 25
	for i := 0; i < n; i++ {
		var feedID, iconID int64
		externalID := fmt.Sprintf("probe-icon-%02d", i)
		if err := db.QueryRow(`
			INSERT INTO icons (hash, mime_type, content, external_id)
			VALUES ($1, 'image/png', $2, $3) RETURNING id`,
			fmt.Sprintf("probe-hash-%02d", i), []byte{0x89, 'P', 'N', 'G'}, externalID,
		).Scan(&iconID); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`
			INSERT INTO feeds (user_id, category_id, feed_url, site_url, title, rewrite_rules, user_agent, cookie, hide_globally)
			VALUES ($1, $2, $3, $4, $5, '', '', '', false) RETURNING id`,
			userID, categoryID, fmt.Sprintf("https://feed-%02d.example/feed", i), fmt.Sprintf("https://feed-%02d.example/", i), fmt.Sprintf("Feed %02d", i),
		).Scan(&feedID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO feed_icons (feed_id, icon_id) VALUES ($1, $2)`, feedID, iconID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`
			INSERT INTO entries (user_id, feed_id, hash, published_at, title, url, content, author, comments_url, status, starred, created_at, changed_at)
			VALUES ($1, $2, $3, now(), $4, $5, '', '', '', 'unread', false, now(), now())`,
			userID, feedID, fmt.Sprintf("probe-entry-hash-%02d", i), fmt.Sprintf("Entry %02d", i), fmt.Sprintf("https://entry-%02d.example/", i),
		); err != nil {
			t.Fatal(err)
		}
	}
	// Statuses and tags are arranged so every list page contains rows that
	// actually render an icon, which is the query pattern being measured.
	if _, err := db.Exec(`UPDATE entries SET status = 'read' WHERE id IN (SELECT id FROM entries WHERE feed_id <> 1 ORDER BY id LIMIT 5)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE entries SET starred = true WHERE id IN (SELECT id FROM entries WHERE feed_id <> 1 ORDER BY id DESC LIMIT 5)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE entries SET tags = ARRAY['tag']::text[], share_code = 'share-' || id`); err != nil {
		t.Fatal(err)
	}

	if err := db.QueryRow(`UPDATE users SET entries_per_page = 25 WHERE id = $1 RETURNING id`, userID).Scan(&userID); err != nil {
		t.Fatal(err)
	}

	config.Opts = config.NewConfigOptions()

	store := storage.NewStorage(db)
	tpl := template.NewEngine("")
	tpl.ParseTemplates()
	h := &handler{store: store, tpl: tpl}

	session, _ := model.NewWebSession("probe", "127.0.0.1")
	session.SetUser(&model.User{ID: userID})

	newRequest := func(target string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		// The handlers are invoked directly; provide the route parameters
		// the real mux would have parsed from the path.
		if strings.HasPrefix(target, "/category/") {
			req.SetPathValue("categoryID", "1")
		}
		if strings.HasPrefix(target, "/feed/") {
			req.SetPathValue("feedID", "1")
		}
		if strings.HasPrefix(target, "/tags/") {
			req.SetPathValue("tagName", "tag")
		}
		ctx := context.WithValue(req.Context(), request.WebSessionContextKey, session)
		ctx = context.WithValue(ctx, request.UserIDContextKey, userID)
		return req.WithContext(ctx)
	}

	pages := []struct {
		name   string
		target string
		render func(w http.ResponseWriter, r *http.Request)
	}{
		{"unread", "/unread", h.showUnreadPage},
		{"history", "/history", h.showHistoryPage},
		{"starred", "/starred", h.showStarredPage},
		{"category_entries", "/category/1/entries", h.showCategoryEntriesPage},
		{"category_entries_all", "/category/1/entries/all", h.showCategoryEntriesAllPage},
		{"category_entries_starred", "/category/1/entries/starred", h.showCategoryEntriesStarredPage},
		{"feed_entries", "/feed/1/entries", h.showFeedEntriesPage},
		{"feed_entries_all", "/feed/1/entries/all", h.showFeedEntriesAllPage},
		{"tag_entries", "/tags/tag/entries/all", h.showTagEntriesAllPage},
		{"shared_entries", "/shares", h.sharedEntries},
		{"search", "/search?q=Entry", h.showSearchPage},
		{"feeds", "/feeds", h.showFeedsPage},
		{"category_feeds", "/category/1/feeds", h.showCategoryFeedsPage},
	}

	iconRe := regexp.MustCompile(`/feed-icon/([^"]+)`)

	type pageResult struct {
		norm     string
		pageQ    int64
		iconQ    int64
		iconURLs int
		iconIDs  []string
	}
	results := make(map[string]pageResult)

	for _, p := range pages {
		if _, err := db.Exec(`SELECT pg_stat_statements_reset()`); err != nil {
			t.Fatal(err)
		}
		before := probeQueryCount(t, db)
		rec := httptest.NewRecorder()
		p.render(rec, newRequest(p.target))
		after := probeQueryCount(t, db)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", p.name, rec.Code, rec.Body.String()[:min(rec.Body.Len(), 300)])
		}
		body := rec.Body.String()
		matches := iconRe.FindAllStringSubmatch(body, -1)
		iconIDs := make([]string, 0, len(matches))
		for _, m := range matches {
			iconIDs = append(iconIDs, m[1])
		}
		beforeIcons := probeQueryCount(t, db)
		for _, id := range iconIDs {
			iconReq := httptest.NewRequest(http.MethodGet, "/feed-icon/"+id, nil)
			iconReq.SetPathValue("externalIconID", id)
			iconRec := httptest.NewRecorder()
			h.showFeedIcon(iconRec, iconReq)
			if iconRec.Code != http.StatusOK {
				t.Fatalf("%s icon %q status=%d", p.name, id, iconRec.Code)
			}
			if want := []byte{0x89, 'P', 'N', 'G'}; string(iconRec.Body.Bytes()) != string(want) {
				t.Fatalf("%s icon %q content mismatch", p.name, id)
			}
		}
		afterIcons := probeQueryCount(t, db)
		results[p.name] = pageResult{
			norm:     normalizeHTML(body),
			pageQ:    after - before,
			iconQ:    afterIcons - beforeIcons,
			iconURLs: len(iconIDs),
			iconIDs:  iconIDs,
		}
	}
	var totalPageQ, totalIconQ int64
	for _, p := range pages {
		r := results[p.name]
		totalPageQ += r.pageQ
		totalIconQ += r.iconQ
		t.Logf("%-24s rows_icon_urls=%2d page_queries=%2d icon_queries=%2d total=%2d", p.name, r.iconURLs, r.pageQ, r.iconQ, r.pageQ+r.iconQ)
	}
	t.Logf("SUMS page_queries=%d icon_queries=%d total=%d", totalPageQ, totalIconQ, totalPageQ+totalIconQ)

	// Persist normalized HTML for cross-run (before/after) byte comparison.
	for _, p := range pages {
		dir := os.Getenv("PROBE_HTML_DIR")
		if dir == "" {
			continue
		}
		path := dir + "/" + p.name + ".html"
		if err := os.WriteFile(path, []byte(results[p.name].norm), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPerfProbeUnreadPageStatements reproduces the reported scenario exactly:
// 25 unread entries, each with its own feed icon, on the /unread page. It
// prints the per-statement breakdown measured by pg_stat_statements, which is
// the same source as PostgreSQL's slow query log.
func TestPerfProbeUnreadPageStatements(t *testing.T) {
	dsn := os.Getenv("PROBE_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://miniflux@127.0.0.1:55432/miniflux_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`TRUNCATE users, categories, feeds, entries, icons, feed_icons, enclosures RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}

	var userID, categoryID int64
	if err := db.QueryRow(`INSERT INTO users (username, entries_per_page) VALUES ('stmt', 25) RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO categories (user_id, title) VALUES ($1, 'cat') RETURNING id`, userID).Scan(&categoryID); err != nil {
		t.Fatal(err)
	}

	const rows = 25
	for i := 0; i < rows; i++ {
		var feedID, iconID int64
		externalID := fmt.Sprintf("stmt-icon-%02d", i)
		if err := db.QueryRow(`
			INSERT INTO icons (hash, mime_type, content, external_id)
			VALUES ($1, 'image/png', $2, $3) RETURNING id`,
			fmt.Sprintf("stmt-hash-%02d", i), []byte{0x89, 'P', 'N', 'G'}, externalID,
		).Scan(&iconID); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`
			INSERT INTO feeds (user_id, category_id, feed_url, site_url, title, rewrite_rules, user_agent, cookie, hide_globally)
			VALUES ($1, $2, $3, $4, $5, '', '', '', false) RETURNING id`,
			userID, categoryID, fmt.Sprintf("https://s-feed-%02d.example/feed", i), fmt.Sprintf("https://s-feed-%02d.example/", i), fmt.Sprintf("S Feed %02d", i),
		).Scan(&feedID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO feed_icons (feed_id, icon_id) VALUES ($1, $2)`, feedID, iconID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`
			INSERT INTO entries (user_id, feed_id, hash, published_at, title, url, content, author, comments_url, status, starred, created_at, changed_at)
			VALUES ($1, $2, $3, now(), $4, $5, '', '', '', 'unread', false, now(), now())`,
			userID, feedID, fmt.Sprintf("stmt-entry-%02d", i), fmt.Sprintf("S Entry %02d", i), fmt.Sprintf("https://s-entry-%02d.example/", i),
		); err != nil {
			t.Fatal(err)
		}
	}

	config.Opts = config.NewConfigOptions()
	store := storage.NewStorage(db)
	tpl := template.NewEngine("")
	tpl.ParseTemplates()
	h := &handler{store: store, tpl: tpl}

	session, _ := model.NewWebSession("probe", "127.0.0.1")
	session.SetUser(&model.User{ID: userID})

	req := httptest.NewRequest(http.MethodGet, "/unread", nil)
	ctx := context.WithValue(req.Context(), request.WebSessionContextKey, session)
	ctx = context.WithValue(ctx, request.UserIDContextKey, userID)
	req = req.WithContext(ctx)

	if _, err := db.Exec(`SELECT pg_stat_statements_reset()`); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.showUnreadPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unread page status=%d", rec.Code)
	}

	iconIDs := regexp.MustCompile(`/feed-icon/([^"]+)`).FindAllStringSubmatch(rec.Body.String(), -1)
	if len(iconIDs) != rows {
		t.Fatalf("expected %d icon urls, got %d", rows, len(iconIDs))
	}
	for _, m := range iconIDs {
		iconReq := httptest.NewRequest(http.MethodGet, "/feed-icon/"+m[1], nil)
		iconReq.SetPathValue("externalIconID", m[1])
		iconRec := httptest.NewRecorder()
		h.showFeedIcon(iconRec, iconReq)
		if iconRec.Code != http.StatusOK {
			t.Fatalf("icon %q status=%d", m[1], iconRec.Code)
		}
	}

	type statementRow struct {
		calls        int64
		meanMs       float64
		totalMs      float64
		rows         int64
		queryPreview string
	}
	var statements []statementRow
	rs, err := db.Query(`
		SELECT calls, mean_exec_time, total_exec_time, rows,
		       regexp_replace(left(query, 140), '\s+', ' ', 'g')
		FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query NOT ILIKE '%pg_stat_statements%'
		ORDER BY calls DESC, total_exec_time DESC
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	for rs.Next() {
		var row statementRow
		if err := rs.Scan(&row.calls, &row.meanMs, &row.totalMs, &row.rows, &row.queryPreview); err != nil {
			t.Fatal(err)
		}
		statements = append(statements, row)
	}

	var totalCalls int64
	for _, s := range statements {
		totalCalls += s.calls
		t.Logf("calls=%3d rows=%4d mean=%8.3fms total=%9.3fms  %s", s.calls, s.rows, s.meanMs, s.totalMs, s.queryPreview)
	}
	t.Logf("TOTAL_SQL_CALLS=%d for 25 unread rows (page + icon requests)", totalCalls)
}
