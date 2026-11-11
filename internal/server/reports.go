package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"blite/internal/proto"
)

// Client log reports, so operators need not ask users for logs. A client
// posts a crash or on-demand log tail; it is stored as one JSON file under
// ReportDir, which an operator reads on the host. Public endpoint, so: size
// cap, per-IP rate limit, a file cap and retention.
const (
	maxReport      = 512 << 10
	maxReportZip   = 12 << 20 // a report with a log zip (base64 in the JSON)
	maxReportFiles = 2000
	maxReportZips  = 200 // past that, reports are kept without their zip
	reportKeep     = 30 * 24 * time.Hour
)

var reportName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

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
	if rep.Platform == "" || len(rep.Log) == 0 || len(rep.Log) > maxReport ||
		len(rep.Zip) > 0 && (rep.Kind != "user" || !bytes.HasPrefix(rep.Zip, []byte("PK\x03\x04"))) {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	if n, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.json")); len(n) >= maxReportFiles {
		http.Error(w, "report store full", http.StatusServiceUnavailable)
		return
	}
	rep.Received = time.Now().UTC().Format(time.RFC3339)
	var rnd [4]byte
	rand.Read(rnd[:])
	name := fmt.Sprintf("%s-%s-%d-%s-%s.json", time.Now().UTC().Format("20060102-150405"),
		reportName.ReplaceAllString(rep.Platform, "_"), rep.Build, reportName.ReplaceAllString(rep.Kind, "_"), hex.EncodeToString(rnd[:]))
	if z, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.zip")); len(z) >= maxReportZips {
		rep.Zip = nil
	}
	if len(rep.Zip) > 0 {
		rep.ZipFile = strings.TrimSuffix(name, ".json") + ".zip"
		if err := os.WriteFile(filepath.Join(s.cfg.ReportDir, rep.ZipFile), rep.Zip, 0o600); err != nil {
			s.cfg.Log.Error("store report zip", "err", err)
			http.Error(w, "could not store", http.StatusInternalServerError)
			return
		}
		rep.Zip = nil
	}
	b, _ := json.MarshalIndent(rep, "", " ")
	if err := os.WriteFile(filepath.Join(s.cfg.ReportDir, name), b, 0o600); err != nil {
		s.cfg.Log.Error("store report", "err", err)
		http.Error(w, "could not store", http.StatusInternalServerError)
		return
	}
	s.cfg.Log.Info("report", "file", name, "name", rep.Name, "kind", rep.Kind, "bytes", len(rep.Log), "zip", rep.ZipFile)
	w.WriteHeader(http.StatusAccepted)
}

// pruneReports deletes reports past reportKeep.
func (s *Server) pruneReports() {
	m, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.json"))
	z, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.zip"))
	for _, p := range append(m, z...) {
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > reportKeep {
			os.Remove(p)
		}
	}
}
