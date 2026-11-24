package diag

import "testing"

func TestReportURL(t *testing.T) {
	if u, _ := ReportURL("wss://blite.example.com/ws"); u != "https://blite.example.com/v1/report" {
		t.Fatal(u)
	}
}
