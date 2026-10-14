package audit

import (
	"context"
	"testing"
	"time"
)

func TestRecordStatsPurge(t *testing.T) {
	l, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	day1 := time.Date(2026, 10, 12, 9, 15, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 13, 14, 5, 0, 0, time.UTC)
	rec := func(at time.Time, tenant, model, status string, p, c int, lat int64) {
		r := Record{At: at, ConnectionID: "dev_" + tenant, Gateway: "https://gw.example", Tenant: tenant, SaaSName: tenant + " Inc", TaskID: "t", Model: model,
			Status: status, PromptTokens: p, CompletionTokens: c, UsageReported: p+c > 0, LatencyMS: lat}
		if status == StatusError {
			r.ErrorCode = "UPSTREAM_ERROR"
		}
		if err := l.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rec(day1, "acme", "ollama/qwen3", StatusOK, 100, 50, 1000)
	rec(day1.Add(time.Hour), "acme", "ollama/qwen3", StatusOK, 10, 5, 3000)
	rec(day2, "acme", "codex/gpt-5", StatusOK, 0, 0, 500)
	rec(day2, "beta", "ollama/qwen3", StatusError, 0, 0, 20)
	if err := l.Record(ctx, Record{Status: "weird"}); err == nil {
		t.Fatal("bad status accepted")
	}

	all, _ := l.Stats(ctx, Filter{}, nil, false)
	if len(all) != 1 || all[0].Requests != 4 || all[0].Errors != 1 || all[0].TotalTokens != 165 || all[0].Unmetered != 2 || all[0].MaxLatencyMS != 3000 {
		t.Fatalf("totals %+v", all)
	}
	bySaaS, _ := l.Stats(ctx, Filter{}, []string{BySaaS}, false)
	if len(bySaaS) != 2 || bySaaS[0].SaaS != "acme" || bySaaS[0].SaaSName != "acme Inc" || bySaaS[0].Requests != 3 || bySaaS[0].PromptTokens != 110 || bySaaS[1].Errors != 1 {
		t.Fatalf("by saas %+v", bySaaS)
	}
	byDayModel, _ := l.Stats(ctx, Filter{Tenant: "acme"}, []string{ByDay, ByModel}, false)
	if len(byDayModel) != 2 || byDayModel[0].Day != "2026-10-12" || byDayModel[0].Model != "ollama/qwen3" || byDayModel[0].AvgLatencyMS != 2000 || byDayModel[1].Day != "2026-10-13" {
		t.Fatalf("by day/model %+v", byDayModel)
	}
	byHour, _ := l.Stats(ctx, Filter{Since: day1, Until: day2}, []string{ByHour}, false)
	if len(byHour) != 2 || byHour[0].Hour != "2026-10-12T09:00" || byHour[1].Hour != "2026-10-12T10:00" {
		t.Fatalf("by hour %+v", byHour)
	}
	if _, err := l.Stats(ctx, Filter{}, []string{"prompt; DROP TABLE requests"}, false); err == nil {
		t.Fatal("unknown group accepted")
	}
	rows, _ := l.List(ctx, Filter{Status: StatusError})
	if len(rows) != 1 || rows[0].Tenant != "beta" || rows[0].ErrorCode != "UPSTREAM_ERROR" {
		t.Fatalf("list %+v", rows)
	}
	n, _ := l.Purge(ctx, day2)
	if n != 2 {
		t.Fatalf("purged %d", n)
	}
	rows, _ = l.List(ctx, Filter{})
	if len(rows) != 2 {
		t.Fatalf("after purge %d", len(rows))
	}
}
