package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"blite/internal/proto"
)

// Client log reports, so operators need not ask users for logs. A client
// posts a crash or on-demand log tail; it is stored as one JSON file under
// ReportDir, which an operator reads on the host. Public endpoint, so: size
// cap, per-IP rate limit, a file cap and retention.
const (
	maxReport      = 512 << 10
	maxReportFiles = 2000
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReport)).Decode(&rep); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	if rep.Platform == "" || len(rep.Log) == 0 {
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
	b, _ := json.MarshalIndent(rep, "", " ")
	if err := os.WriteFile(filepath.Join(s.cfg.ReportDir, name), b, 0o600); err != nil {
		s.cfg.Log.Error("store report", "err", err)
		http.Error(w, "could not store", http.StatusInternalServerError)
		return
	}
	s.cfg.Log.Info("report", "file", name, "name", rep.Name, "kind", rep.Kind, "bytes", len(rep.Log))
	w.WriteHeader(http.StatusAccepted)
}

// pruneReports deletes reports past reportKeep.
func (s *Server) pruneReports() {
	m, _ := filepath.Glob(filepath.Join(s.cfg.ReportDir, "*.json"))
	for _, p := range m {
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > reportKeep {
			os.Remove(p)
		}
	}
}
