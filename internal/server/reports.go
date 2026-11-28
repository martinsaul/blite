package server

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"blite/internal/proto"
)

// DefaultReportMaxBytes is the default total disk budget for Config.ReportDir.
const DefaultReportMaxBytes int64 = 2 << 30

// Client log reports, so operators need not ask users for logs. A client
// posts a crash or on-demand log tail; it is stored as one JSON file under
// ReportDir, which an operator reads on the host. Public endpoint, so: size
// cap, per-IP rate limit, file/byte caps and retention.
const (
	maxReport      = 512 << 10
	maxReportZip   = 12 << 20 // a report with a log zip (base64 in the JSON)
	maxReportField = 256
	maxReportNote  = 8 << 10
	maxReportFiles = 2000
	maxReportZips  = 200            // past that, reports are kept without their zip
	reportKeep     = 72 * time.Hour // pruned hourly: gone within 73 h
	reportLogTrunc = 64
)

var reportName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func validReportFields(rep *proto.Report) bool {
	if rep.Platform == "" || len(rep.Platform) > maxReportField ||
		len(rep.Name) > maxReportField || len(rep.Kind) > maxReportField ||
		len(rep.Version) > maxReportField || len(rep.OSVersion) > maxReportField ||
		len(rep.File) > maxReportField || len(rep.Note) > maxReportNote {
		return false
	}
	if len(rep.Log) == 0 || len(rep.Log) > maxReport {
		return false
	}
	if len(rep.Zip) > 0 && (rep.Kind != "user" || !bytes.HasPrefix(rep.Zip, []byte("PK\x03\x04"))) {
		return false
	}
	return true
}

func marshalReport(rep proto.Report) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	err := enc.Encode(rep)
	return buf.Bytes(), err
}

func truncLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.reportIP.Blocked(ip) {
		http.Error(w, "too many reports", http.StatusTooManyRequests)
		return
	}
	s.reportIP.Add(ip)
	var rep proto.Report
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReportZip)).Decode(&rep); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	if !validReportFields(&rep) {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	rep.Received = now.Format(time.RFC3339)
	rep.ZipFile = ""
	var rnd [4]byte
	rand.Read(rnd[:])
	name := fmt.Sprintf("%s-%s-%d-%s-%s.json", now.Format("20060102-150405"),
		reportName.ReplaceAllString(rep.Platform, "_"), rep.Build, reportName.ReplaceAllString(rep.Kind, "_"), hex.EncodeToString(rnd[:]))

	s.reportMu.Lock()
	if z, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.zip")); len(z) >= maxReportZips {
		rep.Zip = nil
	}
	zipData := rep.Zip
	rep.Zip = nil
	if len(zipData) > 0 {
		rep.ZipFile = strings.TrimSuffix(name, ".json") + ".zip"
	}
	b, _ := marshalReport(rep)
	if !s.makeReportSpaceLocked(int64(len(b) + len(zipData))) {
		s.reportMu.Unlock()
		http.Error(w, "report store full", http.StatusServiceUnavailable)
		return
	}
	if n, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.json")); len(n) >= maxReportFiles {
		s.reportMu.Unlock()
		http.Error(w, "report store full", http.StatusServiceUnavailable)
		return
	}
	if len(zipData) > 0 {
		if err := os.WriteFile(filepath.Join(s.cfg.ReportDir, rep.ZipFile), zipData, 0o600); err != nil {
			s.reportMu.Unlock()
			s.cfg.Log.Error("store report zip", "err", err)
			http.Error(w, "could not store", http.StatusInternalServerError)
			return
		}
	}
	if err := os.WriteFile(filepath.Join(s.cfg.ReportDir, name), b, 0o600); err != nil {
		if rep.ZipFile != "" {
			os.Remove(filepath.Join(s.cfg.ReportDir, rep.ZipFile))
		}
		s.reportMu.Unlock()
		s.cfg.Log.Error("store report", "err", err)
		http.Error(w, "could not store", http.StatusInternalServerError)
		return
	}
	s.reportMu.Unlock()

	s.cfg.Log.Info("report", "file", name, "name", truncLog(rep.Name, reportLogTrunc), "kind", truncLog(rep.Kind, reportLogTrunc), "bytes", len(rep.Log), "zip", rep.ZipFile)
	w.WriteHeader(http.StatusAccepted)
}

type reportFile struct {
	path string
	size int64
	mod  time.Time
}

// makeReportSpaceLocked deletes the oldest files in ReportDir until need bytes
// fit inside Config.ReportMaxBytes. Caller holds reportMu.
func (s *Server) makeReportSpaceLocked(need int64) bool {
	budget := s.cfg.ReportMaxBytes
	if budget <= 0 {
		return true
	}
	if need > budget {
		return false
	}
	m, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.json"))
	z, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.zip"))
	paths := append(m, z...)
	var total int64
	files := make([]reportFile, 0, len(paths))
	byPath := make(map[string]int64, len(paths))
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			sz := st.Size()
			total += sz
			files = append(files, reportFile{path: p, size: sz, mod: st.ModTime()})
			byPath[p] = sz
		}
	}
	if total+need <= budget {
		return true
	}
	stem := func(p string) string {
		return strings.TrimSuffix(strings.TrimSuffix(filepath.Base(p), ".json"), ".zip")
	}
	slices.SortFunc(files, func(a, b reportFile) int {
		if c := a.mod.Compare(b.mod); c != 0 {
			return c
		}
		if c := cmp.Compare(stem(a.path), stem(b.path)); c != 0 {
			return c
		}
		// Remove a report's zip before its json when mtimes match (".zip" > ".json").
		return cmp.Compare(filepath.Ext(b.path), filepath.Ext(a.path))
	})
	drop := func(p string) {
		sz, ok := byPath[p]
		if !ok {
			return
		}
		if err := os.Remove(p); err == nil || os.IsNotExist(err) {
			delete(byPath, p)
			total -= sz
		} else {
			s.cfg.Log.Error("report budget delete", "file", filepath.Base(p), "err", err)
		}
	}
	for _, f := range files {
		if total+need <= budget {
			break
		}
		drop(f.path)
		if strings.HasSuffix(f.path, ".json") {
			drop(strings.TrimSuffix(f.path, ".json") + ".zip")
		}
	}
	return total+need <= budget
}

// pruneReports deletes reports past reportKeep.
func (s *Server) pruneReports() {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	m, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.json"))
	z, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.zip"))
	for _, p := range append(m, z...) {
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > reportKeep {
			if err := os.Remove(p); err != nil {
				s.cfg.Log.Error("report past retention not deleted", "file", filepath.Base(p), "err", err)
			}
		}
	}
}
