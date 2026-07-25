package doctor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"monobox.ch/donkey/internal/ui"
)

func testUI(buf *bytes.Buffer) *ui.UI {
	return &ui.UI{Out: buf, Err: buf, Verbose: false, Interactive: false}
}

func TestReportStatuses(t *testing.T) {
	var buf bytes.Buffer
	mk := func(id string, s Status) *Check {
		return &Check{ID: id, Name: id, Run: func(ctx context.Context, env *Env) Result {
			return Result{Status: s, Detail: "detail-" + id}
		}}
	}
	ok := Report(context.Background(), testUI(&buf), NewEnv(nil, "", ".", nil),
		[]*Check{mk("a", OK), mk("b", Warn), mk("c", Info)})
	if !ok {
		t.Fatal("report with no failures should return true")
	}
	ok = Report(context.Background(), testUI(&buf), NewEnv(nil, "", ".", nil), []*Check{mk("d", Fail)})
	if ok {
		t.Fatal("report with a failure should return false")
	}
}

func TestReportShowsFixInstructions(t *testing.T) {
	var buf bytes.Buffer
	c := &Check{
		ID: "conn", Name: "Connected to the donkey server",
		Fix: "Run `donkey login`.",
		Run: func(ctx context.Context, env *Env) Result {
			return Result{Status: Fail, Detail: "no server configured."}
		},
	}
	Report(context.Background(), testUI(&buf), NewEnv(nil, "", ".", nil), []*Check{c})
	out := buf.String()
	if !strings.Contains(out, "no server configured") || !strings.Contains(out, "donkey login") {
		t.Fatalf("output should contain the problem and the fix, got:\n%s", out)
	}
}
