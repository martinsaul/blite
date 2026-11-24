package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"blite/internal/metrics"
)

// printMetrics prints a metrics file from the logs folder (or a Send logs
// zip) as a table, or as CSV.
func printMetrics(path string, csv bool, out io.Writer) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	f, err := metrics.Decode(fh)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if csv {
		return f.CSV(out)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 1, ' ', tabwriter.AlignRight)
	var names []string
	for _, fl := range f.Fields {
		names = append(names, fl.Name)
	}
	fmt.Fprintln(tw, strings.Join(names, "\t")+"\t")
	for _, row := range f.Rows {
		for i, v := range row {
			fmt.Fprint(tw, cell(f.Fields[i], v), "\t")
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

func cell(fl metrics.Field, v int64) string {
	switch {
	case fl.Name == "timeMs":
		return time.UnixMilli(v).Format("01-02 15:04:05")
	case fl.Name == "flags":
		s := ""
		for _, f := range []struct {
			bit  int64
			name string
		}{{metrics.FlagSharing, "S"}, {metrics.FlagPreview, "P"}, {metrics.FlagIdle, "I"}} {
			if v&f.bit != 0 {
				s += f.name
			}
		}
		return s + "."
	case fl.Size == 2 && !fl.Signed && v == metrics.Unknown:
		return "-"
	}
	return fmt.Sprint(v)
}
