package perf

import (
	"errors"
	"strings"
)

// benchmarkHeader is the first cell of the Hetzner runs table in gobank's
// benchmark.md; the table is the rows that follow it.
const benchmarkHeader = "| Date |"

// UpdateBenchmark puts rows into the Hetzner runs table of gobank's
// performance doc (benchmark.md): each row replaces the table's empty
// placeholder for its scale — the third cell, a row whose date cell is
// blank — when there is one, else goes at the end of the table. The rest
// of the document is left as it is. A doc without the table is an error.
func UpdateBenchmark(doc string, rows ...string) (string, error) {
	lines := strings.Split(doc, "\n")
	header := -1
	for i, l := range lines {
		if strings.HasPrefix(l, benchmarkHeader) {
			header = i
			break
		}
	}
	if header < 0 {
		return "", errors.New("benchmark.md: no Hetzner runs table (a line starting with \"| Date |\")")
	}
	// The table is the header, its separator, and the rows up to the first
	// line that is not a table row.
	end := header + 1
	for end < len(lines) && strings.HasPrefix(lines[end], "|") {
		end++
	}
	for _, row := range rows {
		scale := cell(row, 2)
		placed := false
		for i := header + 2; i < end; i++ {
			if cell(lines[i], 0) == "" && cell(lines[i], 2) == scale {
				lines[i] = row
				placed = true
				break
			}
		}
		if !placed {
			lines = append(lines[:end], append([]string{row}, lines[end:]...)...)
			end++
		}
	}
	return strings.Join(lines, "\n"), nil
}

// cell is the n-th cell of a markdown table row, trimmed.
func cell(row string, n int) string {
	cells := strings.Split(strings.Trim(row, "|"), "|")
	if n >= len(cells) {
		return ""
	}
	return strings.TrimSpace(cells[n])
}
