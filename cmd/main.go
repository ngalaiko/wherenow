package main

import (
	"bufio"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

//go:embed index.html
var indexHTMLSrc string

var indexTmpl *template.Template

const maxBodyBytes = 65536 // 64 KB

type Entry struct {
	ID         string   `json:"id"`
	Lat        float64  `json:"lat"`
	Lon        float64  `json:"lon"`
	Timestamp  string   `json:"timestamp"`
	Accuracy   *float64 `json:"accuracy"`
	Label      *string  `json:"label"`
	Note       *string  `json:"note"`
	Category   *string  `json:"category"`
	Reason     *string  `json:"reason"`
	ReceivedAt string   `json:"receivedAt,omitempty"`
	UpdatedAt  string   `json:"updatedAt,omitempty"`
	UA         *string  `json:"ua,omitempty"`
}

type CreateEntry struct {
	ID        string   `json:"id"`
	Lat       float64  `json:"lat"`
	Lon       float64  `json:"lon"`
	Timestamp string   `json:"timestamp"`
	Accuracy  *float64 `json:"accuracy"`
	Label     *string  `json:"label"`
	Note      *string  `json:"note"`
	Category  *string  `json:"category"`
	Reason    *string  `json:"reason"`
}

type PatchEntry struct {
	ID       string  `json:"id"`
	Label    *string `json:"label"`
	Note     *string `json:"note"`
	Category *string `json:"category"`
}

type DeleteEntry struct {
	ID string `json:"id"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{Error: code})
}

var (
	token   string
	logFile string
	mu      sync.Mutex // protects logFile reads/writes
)

func main() {
	flag.StringVar(&logFile, "log-file", "/data/geo.log.jsonl", "path to the JSONL log file")
	flag.Parse()

	token = os.Getenv("TOKEN")
	if token == "" {
		log.Fatal("TOKEN env var is required")
	}

	indexTmpl = template.Must(template.New("index").Parse(indexHTMLSrc))

	// ensure the log file exists
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		if f, err := os.Create(logFile); err != nil {
			log.Fatalf("cannot create log file: %v", err)
		} else {
			f.Close()
		}
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/", handleGet)
	mux.HandleFunc("POST /api/", handlePost)
	mux.HandleFunc("PATCH /api/", handlePatch)
	mux.HandleFunc("DELETE /api/", handleDelete)

	log.Printf("listening on :%s, log=%s", port, logFile)
	handler := loggingMiddleware(jsonMiddleware(mux))
	log.Fatal(http.ListenAndServe(":"+port, handler))
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL, sw.status, time.Since(start))
	})
}

func jsonMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		next.ServeHTTP(w, r)
	})
}

// ---------- auth ----------

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(auth, "Bearer ")
}

func checkAuth(w http.ResponseWriter, r *http.Request) bool {
	t := bearerToken(r)
	if t == "" || subtle.ConstantTimeCompare([]byte(token), []byte(t)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

// ---------- index ----------

func handleIndex(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	entries, err := readLocations(200)
	mu.Unlock()

	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	locJSON, err := json.Marshal(entries)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	indexTmpl.Execute(w, template.JS(locJSON))
}

// ---------- GET /api/ ----------

func handleGet(w http.ResponseWriter, r *http.Request) {
	// unauthenticated ping
	if r.URL.Query().Get("ping") == "1" {
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}

	// authenticated ping
	if r.URL.Query().Get("ping") == "auth" {
		if !checkAuth(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}

	if !checkAuth(w, r) {
		return
	}

	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 200 {
		limit = 200
	}

	mu.Lock()
	entries, err := readLocations(limit)
	mu.Unlock()

	if err != nil {
		writeError(w, http.StatusInternalServerError, "log_not_readable")
		return
	}

	if len(entries) == 0 {
		json.NewEncoder(w).Encode(ErrorResponse{Error: "no_location_found"})
		return
	}

	json.NewEncoder(w).Encode(entries)
}

// readLocations reads up to limit upload entries from the end of the log file.
// Must be called with mu held.
func readLocations(limit int) ([]Entry, error) {
	f, err := os.Open(logFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	var entries []Entry
	for i := len(lines) - 1; i >= 0 && len(entries) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		reason := ""
		if entry.Reason != nil {
			reason = *entry.Reason
		}
		if reason == "" {
			reason = "upload"
		}
		if reason != "upload" {
			continue
		}
		entry.Reason = new("upload")
		entry.ReceivedAt = ""
		entry.UpdatedAt = ""
		entry.UA = nil
		entries = append(entries, entry)
	}

	return entries, nil
}

// ---------- POST ----------

func handlePost(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(w, r) {
		return
	}

	var req CreateEntry
	if !readBodyJSON(w, r, &req) {
		return
	}

	if _, err := uuid.Parse(req.ID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_id")
		return
	}
	if req.Lat < -90 || req.Lat > 90 {
		writeError(w, http.StatusBadRequest, "bad_lat")
		return
	}
	if req.Lon < -180 || req.Lon > 180 {
		writeError(w, http.StatusBadRequest, "bad_lon")
		return
	}
	if req.Label != nil && utf8.RuneCountInString(*req.Label) > 60 {
		writeError(w, http.StatusBadRequest, "bad_label")
		return
	}
	if req.Note != nil && utf8.RuneCountInString(*req.Note) > 500 {
		writeError(w, http.StatusBadRequest, "bad_note")
		return
	}
	if req.Category != nil && utf8.RuneCountInString(*req.Category) > 60 {
		writeError(w, http.StatusBadRequest, "bad_category")
		return
	}

	ts := req.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}

	record := Entry{
		ID:         strings.ToLower(req.ID),
		Lat:        req.Lat,
		Lon:        req.Lon,
		Timestamp:  ts,
		Accuracy:   req.Accuracy,
		Label:      req.Label,
		Note:       req.Note,
		Category:   req.Category,
		Reason:     req.Reason,
		ReceivedAt: time.Now().UTC().Format(time.RFC3339),
		UA:         new(r.UserAgent()),
	}

	encoded, jsonErr := json.Marshal(record)
	if jsonErr != nil {
		writeError(w, http.StatusInternalServerError, "encode_failed")
		return
	}

	mu.Lock()
	defer mu.Unlock()

	f, openErr := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if openErr != nil {
		writeError(w, http.StatusInternalServerError, "cannot_open_log")
		return
	}
	defer f.Close()

	if _, writeErr := f.Write(append(encoded, '\n')); writeErr != nil {
		writeError(w, http.StatusInternalServerError, "write_failed")
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// ---------- PATCH ----------

func handlePatch(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(w, r) {
		return
	}

	var req PatchEntry
	if !readBodyJSON(w, r, &req) {
		return
	}

	if _, err := uuid.Parse(req.ID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_id")
		return
	}
	id := strings.ToLower(req.ID)

	if req.Label == nil && req.Note == nil && req.Category == nil {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"id":   id,
			"noop": true,
		})
		return
	}

	if req.Label != nil && utf8.RuneCountInString(*req.Label) > 60 {
		writeError(w, http.StatusBadRequest, "bad_label")
		return
	}
	if req.Note != nil && utf8.RuneCountInString(*req.Note) > 500 {
		writeError(w, http.StatusBadRequest, "bad_note")
		return
	}
	if req.Category != nil && utf8.RuneCountInString(*req.Category) > 60 {
		writeError(w, http.StatusBadRequest, "bad_category")
		return
	}

	mu.Lock()
	defer mu.Unlock()

	updated, writeErr := rewriteLog(func(entry *Entry) (*Entry, bool) {
		if strings.ToLower(entry.ID) != id {
			return entry, false
		}
		if req.Label != nil {
			entry.Label = req.Label
		}
		if req.Note != nil {
			entry.Note = req.Note
		}
		if req.Category != nil {
			entry.Category = req.Category
		}
		entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return entry, true
	})

	if writeErr != nil {
		writeError(w, http.StatusInternalServerError, writeErr.Error())
		return
	}

	if !updated {
		writeError(w, http.StatusNotFound, "id_not_found")
		return
	}

	resp := map[string]any{"ok": true, "id": id}
	if req.Label != nil {
		resp["label"] = *req.Label
	}
	if req.Note != nil {
		resp["note"] = *req.Note
	}
	if req.Category != nil {
		resp["category"] = *req.Category
	}
	json.NewEncoder(w).Encode(resp)
}

// ---------- DELETE ----------

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(w, r) {
		return
	}

	var req DeleteEntry
	if !readBodyJSON(w, r, &req) {
		return
	}

	if _, err := uuid.Parse(req.ID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_id")
		return
	}
	id := strings.ToLower(req.ID)

	mu.Lock()
	defer mu.Unlock()

	deleted := false
	_, writeErr := rewriteLog(func(entry *Entry) (*Entry, bool) {
		if !deleted && strings.ToLower(entry.ID) == id {
			deleted = true
			return nil, true // nil = remove this line
		}
		return entry, false
	})

	if writeErr != nil {
		writeError(w, http.StatusInternalServerError, writeErr.Error())
		return
	}

	if !deleted {
		writeError(w, http.StatusNotFound, "id_not_found")
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"id":      id,
		"deleted": true,
	})
}

// ---------- helpers ----------

func readBodyJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return false
	}

	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes+1))
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return false
	}

	return true
}

// rewriteLog reads the log file, calls fn for each entry.
// fn returns (modified entry, matched). If entry is nil, the line is removed.
// Must be called with mu held.
func rewriteLog(fn func(entry *Entry) (*Entry, bool)) (bool, error) {
	content, err := os.ReadFile(logFile)
	if err != nil {
		return false, fmt.Errorf("cannot_open_log")
	}

	lines := strings.Split(string(content), "\n")
	var out []string
	matched := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		var entry Entry
		if err := json.Unmarshal([]byte(trimmed), &entry); err != nil {
			out = append(out, trimmed)
			continue
		}

		if !matched {
			modified, hit := fn(&entry)
			if hit {
				matched = true
				if modified == nil {
					continue // delete
				}
				encoded, err := json.Marshal(modified)
				if err != nil {
					return false, fmt.Errorf("encode_failed")
				}
				out = append(out, string(encoded))
				continue
			}
		}

		out = append(out, trimmed)
	}

	if !matched {
		return false, nil
	}

	result := strings.Join(out, "\n")
	if result != "" {
		result += "\n"
	}

	if err := os.WriteFile(logFile, []byte(result), 0644); err != nil {
		return false, fmt.Errorf("write_failed")
	}

	return true, nil
}
