package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"

	"blite/internal/proto"
	"blite/internal/redact"
)

// ReportURL is the signal server's report endpoint for a signaling URL.
func ReportURL(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	u.Path, u.RawQuery, u.Fragment = "/v1/report", "", ""
	return u.String(), nil
}

// SendReport posts r to the signal server (POST /v1/report).
func SendReport(ctx context.Context, server string, r proto.Report) error {
	u, err := ReportURL(server)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("server: %s %s", res.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// maxSend is what the server takes in a report's log (its maxReport),
// with some room.
const maxSend = 500 << 10

// Send posts the report as one of kind "diag": the text and the JSON in its
// log, redacted like the app's logs, without a zip. base carries the version,
// build and OS.
func (r *Report) Send(ctx context.Context, server string, base proto.Report) error {
	rep := base
	if rep.Platform == "" {
		rep.Platform = runtime.GOOS
	}
	rep.Name, rep.Kind, rep.File = r.Host+" diag", "diag", "blite-diag"
	rep.Log = redact.Text(r.Full())
	if len(rep.Log) > maxSend { // the JSON repeats the text: keep the text
		rep.Log = redact.Text(r.Text)
		rep.Note = "JSON left out: too large"
	}
	if len(rep.Log) > maxSend {
		rep.Log = rep.Log[len(rep.Log)-maxSend:]
	}
	return SendReport(ctx, server, rep)
}
