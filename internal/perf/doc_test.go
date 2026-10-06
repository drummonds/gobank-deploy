package perf

import (
	"strings"
	"testing"
)

const benchmarkDoc = `# Performance

Some words.

## Hetzner runs

Made by gobank-deploy's **perf** workflow.

| Date | gobank | Scale | Server | Customers reached | Customers/s | Days run | Account days / 12h | Last day |
|---|---|---|---|---|---|---|---|---|
| | | small | | | | | | |
| | | large | | | | | | |

For comparison, the old demo.

## Laptop baseline (pglike)

| Customers | Accounts |
|---|---|
| 1 | 3 |
`

// A run's row goes into the Hetzner table of gobank's performance doc:
// in place of the empty placeholder row for its scale when there is one,
// else at the end of the table. The rest of the document is untouched.
func TestUpdateBenchmarkFillsThePlaceholderThenAppends(t *testing.T) {
	small := "| 2026-10-06 | v0.12.0 | small | cx23: 4 GB | 60,000 | 142.1 | 3 | 18,388,121 | 4m41s over 119,794 accounts |"
	large := "| 2026-10-06 | v0.12.0 | large | ccx33: 32 GB | 180,000 | 410.0 | 9 | 60,000,000 | 2m01s over 359,000 accounts |"
	got, err := UpdateBenchmark(benchmarkDoc, small, large)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(benchmarkDoc, "| | | small | | | | | | |", small, 1)
	want = strings.Replace(want, "| | | large | | | | | | |", large, 1)
	if got != want {
		t.Errorf("placeholders should be filled in place:\n%s", got)
	}

	again := "| 2026-10-07 | v0.12.1 | small | cx23: 4 GB | 61,000 | 150.0 | 3 | 19,000,000 | 4m30s over 121,000 accounts |"
	got, err = UpdateBenchmark(got, again)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, large+"\n"+again+"\n\nFor comparison") {
		t.Errorf("a later run should be appended at the end of the table:\n%s", got)
	}
	if strings.Count(got, "## Laptop baseline") != 1 || !strings.HasSuffix(got, "| 1 | 3 |\n") {
		t.Errorf("the rest of the document should be untouched:\n%s", got)
	}
}

func TestUpdateBenchmarkRefusesADocWithoutTheTable(t *testing.T) {
	if _, err := UpdateBenchmark("# Something else\n", "| row |"); err == nil {
		t.Error("want an error when the Hetzner runs table is missing")
	}
}
