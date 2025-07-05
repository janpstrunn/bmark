package main

import (
	"database/sql"
	"fmt"
	"html"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Bookmark struct {
	URI       string
	Title     string
	CreatedAt int64
	UpdatedAt int64
	Tags      []string
	Note      string
}

type Job struct {
	URI       string
	Title     string
	Note      string
	CreatedAt int64
	UpdatedAt int64
	Tags      []string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage:")
		fmt.Println("  importer-exporter import <bookmark.html>")
		fmt.Println("  importer-exporter export [output.html]")
		os.Exit(1)
	}

	mode := os.Args[1]
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Cannot find user home directory: %v", err)
	}
	dbFile := filepath.Join(homeDir, ".local", "share", "bookmarks", "bookmark.db")

	db, err := sql.Open("sqlite3", fmt.Sprintf("%s?_busy_timeout=5000", dbFile))
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(1)

	if err := initializeDatabase(db); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	switch mode {
	case "import":
		if len(os.Args) < 3 {
			fmt.Println("Usage: importer-exporter import <bookmark.html>")
			os.Exit(1)
		}
		bookmarksFile := os.Args[2]
		importBookmarks(db, bookmarksFile)
	case "export":
		outputFile := "exported_bookmarks.html"
		if len(os.Args) >= 3 {
			outputFile = os.Args[2]
		}
		exportBookmarks(db, outputFile)
	default:
		fmt.Println("Invalid mode. Use 'import' or 'export'.")
		os.Exit(1)
	}
}

func importBookmarks(db *sql.DB, bookmarksFile string) {
	data, err := os.ReadFile(bookmarksFile)
	if err != nil {
		log.Fatalf("Failed to read bookmarks file: %v", err)
	}
	content := string(data)

	blocks := strings.Split(content, "<DT>")
	jobs := make(chan Job, len(blocks))
	results := make(chan error, len(blocks))

	var wg sync.WaitGroup
	workerCount := 5
	wg.Add(workerCount)

	for range workerCount {
		go worker(db, jobs, results, &wg)
	}

	go func() {
		parseBlocks(blocks, jobs)
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	successCount := 0
	for err := range results {
		if err != nil {
			log.Printf("Error: %v", err)
		} else {
			successCount++
		}
	}

	fmt.Printf("%d bookmarks successfully imported!\n", successCount)
}

func parseBlocks(blocks []string, jobs chan<- Job) {
	reAnchor := regexp.MustCompile(`(?i)<A\s+([^>]+)>(.*?)</A>`)
	reHref := regexp.MustCompile(`HREF="([^"]+)"`)
	reAddDate := regexp.MustCompile(`ADD_DATE="(\d+)"`)
	reLastMod := regexp.MustCompile(`LAST_MODIFIED="(\d+)"`)
	reTags := regexp.MustCompile(`TAGS="([^"]+)"`)
	reDesc := regexp.MustCompile(`(?i)<DD>([^<]+)`)

	now := time.Now().Unix()

	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}

		anchorMatch := reAnchor.FindStringSubmatch(block)
		if len(anchorMatch) < 3 {
			continue
		}

		attrStr := anchorMatch[1]
		title := htmlUnescape(strings.TrimSpace(anchorMatch[2]))

		uri := extractHref(reHref, attrStr)
		if uri == "" {
			continue
		}

		createdAt := extractTimestamp(reAddDate, attrStr, now)
		updatedAt := extractTimestamp(reLastMod, attrStr, createdAt)
		tags := extractTags(reTags, attrStr)
		note := extractDescription(reDesc, block)

		jobs <- Job{
			URI:       uri,
			Title:     title,
			Note:      note,
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
			Tags:      tags,
		}
	}
}

func worker(db *sql.DB, jobs <-chan Job, results chan<- error, wg *sync.WaitGroup) {
	defer wg.Done()

	for job := range jobs {
		bookmarkID, err := insertBookmark(db, job.URI, job.Title, job.Note, job.CreatedAt, job.UpdatedAt)
		if err != nil {
			results <- fmt.Errorf("failed to insert bookmark %s: %v", job.URI, err)
			continue
		}

		if err := insertTags(db, bookmarkID, job.Tags); err != nil {
			results <- fmt.Errorf("failed to insert tags for bookmark %s: %v", job.URI, err)
			continue
		}

		results <- nil
	}
}

func extractHref(re *regexp.Regexp, attrStr string) string {
	if m := re.FindStringSubmatch(attrStr); m != nil {
		return m[1]
	}
	return ""
}

func extractTimestamp(re *regexp.Regexp, attrStr string, defaultValue int64) int64 {
	if m := re.FindStringSubmatch(attrStr); m != nil {
		if timestamp, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return timestamp
		}
	}
	return defaultValue
}

func extractTags(re *regexp.Regexp, attrStr string) []string {
	if m := re.FindStringSubmatch(attrStr); m != nil && m[1] != "" {
		tags := strings.Split(m[1], ",")
		var cleanedTags []string
		for _, tag := range tags {
			tag = strings.TrimSpace(tag)
			if tag != "" {
				cleanedTags = append(cleanedTags, tag)
			}
		}
		return cleanedTags
	}
	return []string{}
}

func extractDescription(re *regexp.Regexp, block string) string {
	if m := re.FindStringSubmatch(block); m != nil {
		return htmlUnescape(strings.TrimSpace(m[1]))
	}
	return ""
}

func insertBookmark(db *sql.DB, uri, title, note string, createdAt, updatedAt int64) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		INSERT OR IGNORE INTO bookmarks (url, title, note, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		uri, title, note, createdAt, updatedAt)
	if err != nil {
		return 0, fmt.Errorf("failed to insert or ignore bookmark: %w", err)
	}

	var bookmarkID int64
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected > 0 {

		bookmarkID, err = res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("failed to get last insert ID: %w", err)
		}
	} else {

		err = tx.QueryRow("SELECT id FROM bookmarks WHERE url = ?", uri).Scan(&bookmarkID)
		if err != nil {
			return 0, fmt.Errorf("failed to retrieve existing bookmark ID: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return bookmarkID, nil
}

func insertTags(db *sql.DB, bookmarkID int64, tags []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction for tags: %w", err)
	}
	defer tx.Rollback()

	for _, tag := range tags {
		if tag == "" {
			continue
		}

		var tagID int64
		err := tx.QueryRow("SELECT id FROM tags WHERE tag = ?", tag).Scan(&tagID)
		if err != nil {
			if err == sql.ErrNoRows {
				res, err := tx.Exec("INSERT OR IGNORE INTO tags (tag) VALUES (?)", tag)
				if err != nil {
					return fmt.Errorf("failed to insert or ignore tag %s: %w", tag, err)
				}
				tagID, err = res.LastInsertId()
				if err != nil {
					return fmt.Errorf("failed to get last insert ID for tag %s: %w", tag, err)
				}

				if tagID == 0 {
					err = tx.QueryRow("SELECT id FROM tags WHERE tag = ?", tag).Scan(&tagID)
					if err != nil {
						return fmt.Errorf("failed to retrieve existing tag ID for %s: %w", tag, err)
					}
				}

			} else {
				return fmt.Errorf("failed to query tag ID for %s: %w", tag, err)
			}
		}

		_, err = tx.Exec(`
			INSERT OR IGNORE INTO bookmark_tags (bookmark_id, tag_id)
			VALUES (?, ?)`,
			bookmarkID, tagID)
		if err != nil {
			return fmt.Errorf("failed to link bookmark %d to tag %d: %w", bookmarkID, tagID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit tags transaction: %w", err)
	}

	return nil
}

func htmlUnescape(s string) string {
	replacements := []struct{ old, new string }{
		{"&amp;", "&"},
		{"&lt;", "<"},
		{"&gt;", ">"},
		{"&quot;", `"`},
		{"&#39;", "'"},
	}
	for _, r := range replacements {
		s = strings.ReplaceAll(s, r.old, r.new)
	}
	return s
}

func exportBookmarks(db *sql.DB, outputFile string) {
	rows, err := db.Query(`
		SELECT b.url, b.title, b.created_at, b.updated_at, b.note, GROUP_CONCAT(t.tag, ',') as tags
		FROM bookmarks b
		LEFT JOIN bookmark_tags bt ON b.id = bt.bookmark_id
		LEFT JOIN tags t ON bt.tag_id = t.id
		GROUP BY b.id
	`)
	if err != nil {
		log.Fatalf("Failed to query bookmarks for export: %v", err)
	}
	defer rows.Close()

	file, err := os.Create(outputFile)
	if err != nil {
		log.Fatalf("Failed to create output file %s: %v", outputFile, err)
	}
	defer file.Close()

	fmt.Fprintln(file, `<!DOCTYPE NETSCAPE-Bookmark-file-1>`)
	fmt.Fprintln(file, ``)
	fmt.Fprintln(file, `<META HTTP-EQUIV="Content-Type" CONTENT="text/html; charset=UTF-8">`)
	fmt.Fprintln(file, `<TITLE>Bookmarks</TITLE>`)
	fmt.Fprintln(file, `<H1>Bookmarks</H1>`)
	fmt.Fprintln(file, `<DL><p>`)

	bookmarkCount := 0
	for rows.Next() {
		var uri, title, note string
		var createdAt, updatedAt int64
		var tags sql.NullString

		err := rows.Scan(&uri, &title, &createdAt, &updatedAt, &note, &tags)
		if err != nil {
			log.Printf("Row error during export: %v", err)
			continue
		}

		titleEsc := html.EscapeString(title)
		noteEsc := html.EscapeString(note)
		uriEsc := html.EscapeString(uri)

		var tagsEsc string
		if tags.Valid {
			tagsEsc = html.EscapeString(tags.String)
		} else {
			tagsEsc = ""
		}

		attr := fmt.Sprintf(`HREF="%s" ADD_DATE="%d" LAST_MODIFIED="%d"`, uriEsc, createdAt, updatedAt)
		if tagsEsc != "" {
			attr += fmt.Sprintf(` TAGS="%s"`, tagsEsc)
		}
		fmt.Fprintf(file, `<DT><A %s>%s</A>`, attr, titleEsc)

		if noteEsc != "" {
			fmt.Fprintf(file, `<DD>%s`, noteEsc)
		}
		fmt.Fprintln(file, "")

		bookmarkCount++
	}

	fmt.Fprintln(file, `</DL><p>`)

	if bookmarkCount == 0 {
		fmt.Println("No bookmarks found in database.")
	} else {
		fmt.Printf("Exported %d bookmarks to: %s\n", bookmarkCount, outputFile)
	}
}

func initializeDatabase(db *sql.DB) error {
	tables := []string{
		`CREATE TABLE IF NOT EXISTS bookmarks (
			id INTEGER PRIMARY KEY NOT NULL,
			url TEXT NOT NULL UNIQUE,
			title TEXT,
			note TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS tags (
			id INTEGER PRIMARY KEY NOT NULL,
			tag TEXT NOT NULL UNIQUE
		);`,
		`CREATE TABLE IF NOT EXISTS bookmark_tags (
			bookmark_id INTEGER,
			tag_id INTEGER,
			PRIMARY KEY (bookmark_id, tag_id),
			FOREIGN KEY (bookmark_id) REFERENCES bookmarks(id) ON DELETE CASCADE,
			FOREIGN KEY (tag_id) REFERENCES tags(id) ON DELETE CASCADE
		);`,
	}

	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_url ON bookmarks (url);`,
		`CREATE INDEX IF NOT EXISTS idx_tag ON tags (tag);`,
		`CREATE INDEX IF NOT EXISTS idx_bookmark_id ON bookmark_tags (bookmark_id);`,
		`CREATE INDEX IF NOT EXISTS idx_tag_id ON bookmark_tags (tag_id);`,
	}

	for _, table := range tables {
		if _, err := db.Exec(table); err != nil {
			return fmt.Errorf("failed to create table: %v", err)
		}
	}

	for _, index := range indexes {
		if _, err := db.Exec(index); err != nil {
			return fmt.Errorf("failed to create index: %v", err)
		}
	}

	return nil
}
