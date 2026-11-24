// Command blite-diag tests a computer for screen sharing: system and
// display settings, programs that compete for the GPU's encoder, every
// H.264 encoder a share may use, the app's real share pipeline on the
// primary monitor (where frames get lost), and the network. It prints as it
// goes, saves the report next to itself and sends it to the signaling
// server as a "diag" report. The logic is internal/diag's.
//
// Copy it into the app's bin folder (next to blite.exe and the GStreamer
// DLLs) and run it from a console:
//
//	blite-diag [-quick] [-nosend] [-server wss://host/ws] [-skip capture,encode,net] [-window title] [-chain gpu|cpu]
//
// It also reads the app's per-second share readings (metrics-*.bin in the
// logs folder and in a Send logs zip):
//
//	blite-diag -metrics FILE [-csv]
//
// Windows build: as blite.exe (scripts/build-windows.sh), without -H windowsgui.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"blite/internal/diag"
	"blite/internal/media"
	certpin "blite/internal/pin"
	"blite/internal/proto"
)

// Stamped like blite's (scripts/version.sh).
var (
	version       = "dev"
	build         = "0"
	defaultServer = "wss://bellion.app/ws"
)

func main() {
	server := flag.String("server", envOr("BLITE_SERVER", defaultServer), "signaling URL (network test and where the report goes)")
	noSend := flag.Bool("nosend", false, "don't send the report to the server")
	quick := flag.Bool("quick", false, "shorter runs")
	skip := flag.String("skip", "", "steps to leave out, comma-separated: system, procs, encode, capture, net")
	noWait := flag.Bool("nowait", false, "don't wait for Enter at the end")
	window := flag.String("window", "", "the window to test capturing, by part of its title (default: the topmost window besides this console)")
	chain := flag.String("chain", "", "where the share runs scale and convert the capture: gpu or cpu (default: as the app; the runs without an encoder try both)")
	metricsFile := flag.String("metrics", "", "print this metrics file (metrics-*.bin) as a table and exit")
	csv := flag.Bool("csv", false, "with -metrics: print CSV")
	flag.Parse()
	if *metricsFile != "" {
		if err := printMetrics(*metricsFile, *csv, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	switch c := media.Chain(*chain); c {
	case "", media.ChainGPU, media.ChainCPU:
		media.ForceChain = c
	default:
		fmt.Fprintln(os.Stderr, "-chain is gpu or cpu")
		os.Exit(2)
	}
	code := run(*server, *noSend, *quick, *skip, *window)
	if !*noWait {
		fmt.Print("\nPress Enter to close.")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(code)
}

func run(server string, noSend, quick bool, skipList, window string) int {
	diag.SetDPIAware()
	// GStreamer's and our warnings would interleave with the report.
	media.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if u, err := url.Parse(server); err == nil && server == defaultServer {
		// The app pins its built-in server's certificate authorities.
		host := u.Hostname()
		certpin.Install(http.DefaultTransport.(*http.Transport))
		certpin.SetPinned(func(h string) bool { return h == host || h == "www."+host })
	}
	skip := map[string]bool{}
	for _, s := range strings.Split(skipList, ",") {
		if s = strings.TrimSpace(strings.ToLower(s)); s != "" {
			skip[s] = true
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	label := version + " (" + build + ")"
	fmt.Println("This takes a few minutes. Leave the computer alone, except: during the")
	fmt.Println("capture step, keep a 60 fps video or a game moving on the main screen, in the")
	fmt.Println("topmost window (or the one -window names): the step captures it too.")
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	r := diag.Run(ctx, diag.Options{
		Server:  server,
		Quick:   quick,
		Skip:    skip,
		Out:     os.Stdout,
		Version: label,
		Window:  window,
		Checkpoint: func(r *diag.Report) {
			r.Save(dir) // a driver crash in the next step still leaves this
		},
	})
	path, err := r.Save(dir)
	if err != nil {
		fmt.Println("\nCould not save the report:", err)
	} else {
		fmt.Println("\nReport saved to", path)
	}
	if noSend {
		fmt.Println("Not sent (-nosend).")
		return 0
	}
	n, _ := strconv.Atoi(build)
	base := proto.Report{Platform: runtime.GOOS, Version: label + " " + runtime.GOOS + " blite-diag", Build: n}
	if r.System != nil {
		base.OSVersion = r.System.OS
	}
	sctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r.Send(sctx, server, base); err != nil {
		fmt.Println("Could not send the report:", err)
		fmt.Println("Please send the file above to Bellion Share's support instead.")
		return 1
	}
	fmt.Println("Report sent to", server, "- thank you.")
	return 0
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
