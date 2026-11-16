package server

import (
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

// The reports export as CSV for whoever works through them in a
// spreadsheet. The JSON a report answers with is the form for programs; the
// CSV is one row per fact, with the contacts repeated on each, so a row
// still says who to write to after it is filtered, sorted or pasted
// somewhere else.

// reportWriter is where a report's body goes: compressed for a client that
// takes it. A report repeats the same few words for every app a finding is
// in, so a fleet's worth is megabytes that shrink to a tenth. The caller
// sets the content type first and closes the writer when it is done.
func reportWriter(w http.ResponseWriter, r *http.Request) io.WriteCloser {
	w.Header().Add("Vary", "Accept-Encoding")
	for encoding := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		if name, _, _ := strings.Cut(strings.TrimSpace(encoding), ";"); name == "gzip" {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusOK)
			return gzip.NewWriter(w)
		}
	}
	w.WriteHeader(http.StatusOK)
	return nopCloser{w}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// writeReport answers with a report as JSON.
func writeReport(w http.ResponseWriter, r *http.Request, report any) {
	w.Header().Set("Content-Type", "application/json")
	out := reportWriter(w, r)
	_ = json.NewEncoder(out).Encode(report)
	_ = out.Close()
}

// writeCSV sends rows as a download named after the report and the day.
func writeCSV(w http.ResponseWriter, r *http.Request, report string, header []string, rows [][]string) {
	name := fmt.Sprintf("donkey-%s-%s.csv", report, time.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	body := reportWriter(w, r)
	// The mark tells a spreadsheet the file is UTF-8; without it, the dashes
	// and arrows in a note come out as mojibake.
	_, _ = body.Write([]byte{0xEF, 0xBB, 0xBF})
	out := csv.NewWriter(body)
	_ = out.Write(header)
	for _, row := range rows {
		for i := range row {
			row[i] = csvCell(row[i])
		}
		_ = out.Write(row)
	}
	out.Flush()
	_ = body.Close()
}

// csvCell keeps a spreadsheet from running a cell. A value that opens with
// =, +, - or @ is read as a formula, and these cells carry words other
// people chose: a waiver's reason, a package's name, the assistant's reading
// of an app's code. The apostrophe makes the cell text; it shows in the
// sheet, which is the price of a file that is safe to open.
func csvCell(value string) string {
	value = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(value)
	if value != "" && strings.ContainsRune("=+-@\t", rune(value[0])) {
		return "'" + value
	}
	return value
}

// contactList is a role's holders in one cell: people first, then groups,
// marked as such so a group's name is never read as somebody's address.
func contactList(access config.AccessConfig) string {
	entries := append([]string{}, access.Users...)
	for _, group := range access.Groups {
		entries = append(entries, "group:"+group)
	}
	return strings.Join(entries, "; ")
}

// reportFormat reads the format a report was asked for: JSON, or CSV.
func reportFormat(r *http.Request) (csv bool, err error) {
	switch format := r.URL.Query().Get("format"); format {
	case "", "json":
		return false, nil
	case "csv":
		return true, nil
	default:
		return false, fmt.Errorf("format %q is not one this report comes in — use json or csv", format)
	}
}
