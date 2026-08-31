package columnar

import (
	"fmt"
	"math"
	"sort"
	"testing"
	"time"
)

// telemetrySchema is the shape of the label-telemetry table, which is the
// largest table this store holds and the one the compression ratios are
// measured on.
func telemetrySchema() Schema {
	return Schema{
		Table:      "label_telemetry",
		TimeColumn: "reported_at",
		Columns: []Column{
			{Name: "reported_at", Type: TypeTimestamp},
			{Name: "tenant_id", Type: TypeString},
			{Name: "store_id", Type: TypeString},
			{Name: "label_id", Type: TypeString},
			{Name: "firmware_version", Type: TypeString},
			{Name: "battery_mv", Type: TypeInt64},
			{Name: "battery_pct", Type: TypeInt64},
			{Name: "temperature_c", Type: TypeFloat64},
			{Name: "rssi", Type: TypeInt64},
			{Name: "lqi", Type: TypeInt64},
			{Name: "mesh_hops", Type: TypeInt64},
			{Name: "refresh_count", Type: TypeInt64},
			{Name: "nfc_taps", Type: TypeInt64},
			{Name: "uptime_seconds", Type: TypeInt64},
			{Name: "tamper", Type: TypeBool},
		},
	}
}

// synth is a deterministic generator so a failure is reproducible.
type synth struct{ s uint64 }

func newSynth(seed uint64) *synth { return &synth{s: seed} }

func (r *synth) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *synth) f64() float64 { return float64(r.next()>>11) / float64(1<<53) }
func (r *synth) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func (r *synth) normal() float64 {
	u := r.f64()
	if u < 1e-12 {
		u = 1e-12
	}
	return math.Sqrt(-2*math.Log(u)) * math.Cos(2*math.Pi*r.f64())
}

var epoch = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

// generateTelemetry produces rows shaped like real label telemetry: many labels
// across a handful of stores, reporting every five minutes, with slowly-drifting
// battery and temperature.
//
// All of it is synthetic. The compression figures the tests report characterise
// this generator's output — which is modelled on the real signal shapes but is
// not real fleet data — and the tests say so.
func generateTelemetry(n int, stores, labelsPerStore int, seed uint64) []Row {
	r := newSynth(seed)
	rows := make([]Row, 0, n)
	battery := make([]int64, stores*labelsPerStore)
	for i := range battery {
		battery[i] = 3000 + int64(r.intn(120)) - 60
	}
	firmware := []string{"1.4.2", "1.4.3", "1.5.0"}
	for i := 0; i < n; i++ {
		label := i % (stores * labelsPerStore)
		store := label / labelsPerStore
		// Battery declines slowly and never rises, which is what makes the
		// delta encoding effective on it.
		if r.intn(50) == 0 && battery[label] > 2000 {
			battery[label]--
		}
		temp := 20 + 2*r.normal()
		if store%4 == 0 {
			temp = 4 + 1.5*r.normal()
		}
		rows = append(rows, Row{
			"reported_at":      epoch.Add(time.Duration(i) * 300 * time.Millisecond),
			"tenant_id":        "acme",
			"store_id":         fmt.Sprintf("store-%03d", store),
			"label_id":         fmt.Sprintf("lbl-%06d", label),
			"firmware_version": firmware[label%len(firmware)],
			"battery_mv":       battery[label],
			"battery_pct":      int64(battery[label]-2000) * 100 / 1200,
			"temperature_c":    temp,
			"rssi":             int64(-70 + r.intn(30)),
			"lqi":              int64(150 + r.intn(100)),
			"mesh_hops":        int64(1 + r.intn(3)),
			"refresh_count":    int64(i / 100),
			"nfc_taps":         int64(r.intn(3)),
			"uptime_seconds":   int64(86400 + i),
			"tamper":           r.intn(10000) == 0,
		})
	}
	return rows
}

func newStore(t *testing.T, schema Schema) *Store {
	t.Helper()
	s, err := Open(Options{Dir: t.TempDir(), Schema: schema})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestIngestQueryRoundTrip is the basic contract: what goes in comes back out,
// through the encoders, the block format and the segment files.
func TestIngestQueryRoundTrip(t *testing.T) {
	s := newStore(t, telemetrySchema())
	rows := generateTelemetry(20000, 4, 50, 42)
	if err := s.Append(rows...); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	res, err := s.Query(Query{
		Aggregates: []Aggregate{
			{Func: AggCount, As: "n"},
			{Func: AggAvg, Column: "battery_mv", As: "avg_battery"},
			{Func: AggMin, Column: "lqi", As: "min_lqi"},
			{Func: AggMax, Column: "lqi", As: "max_lqi"},
			{Func: AggCountDistinct, Column: "store_id", As: "stores"},
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows, want one ungrouped row", len(res.Rows))
	}
	got := res.Rows[0].Values
	if int(got["n"]) != len(rows) {
		t.Errorf("count = %v, want %d", got["n"], len(rows))
	}
	if int(got["stores"]) != 4 {
		t.Errorf("distinct stores = %v, want 4", got["stores"])
	}

	// Compare against the same aggregates computed directly from the input.
	var sum, mn, mx float64
	mn, mx = math.Inf(1), math.Inf(-1)
	for _, r := range rows {
		b := float64(r["battery_mv"].(int64))
		sum += b
		l := float64(r["lqi"].(int64))
		if l < mn {
			mn = l
		}
		if l > mx {
			mx = l
		}
	}
	if math.Abs(got["avg_battery"]-sum/float64(len(rows))) > 1e-6 {
		t.Errorf("avg battery = %v, want %v", got["avg_battery"], sum/float64(len(rows)))
	}
	if got["min_lqi"] != mn || got["max_lqi"] != mx {
		t.Errorf("lqi range = [%v, %v], want [%v, %v]", got["min_lqi"], got["max_lqi"], mn, mx)
	}
}

// TestCompressionRatioIsMeasured reports what the format actually achieves on a
// million rows of synthetic telemetry.
func TestCompressionRatioIsMeasured(t *testing.T) {
	if testing.Short() {
		t.Skip("a million rows is too slow for -short")
	}
	s := newStore(t, telemetrySchema())
	const total = 1_000_000
	const batch = 50_000
	start := time.Now()
	for written := 0; written < total; written += batch {
		if err := s.Append(generateTelemetry(batch, 20, 200, uint64(written))...); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	ingest := time.Since(start)

	st, err := s.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.RowsWritten != total {
		t.Errorf("wrote %d rows, want %d", st.RowsWritten, total)
	}
	onDisk := int64(0)
	for _, v := range st.BytesOnDisk {
		onDisk += v
	}
	t.Logf("synthetic telemetry, %d rows across %d columns: raw %d bytes, compressed %d bytes "+
		"(%.2fx), %d bytes on disk in %d segments, %.1f bytes per row, ingested in %s (%.0f rows/sec)",
		st.RowsWritten, len(telemetrySchema().Columns), st.RawBytes, st.CompressedBytes,
		st.CompressionRatio, onDisk, st.Segments["hot"], float64(onDisk)/float64(total),
		ingest, float64(total)/ingest.Seconds())

	if st.CompressionRatio < 3 {
		t.Errorf("compression ratio %.2f is below the 3x the encodings should give on this shape",
			st.CompressionRatio)
	}

	// And the query latency over the same million rows, which is the other half
	// of the claim.
	qStart := time.Now()
	res, err := s.Query(Query{
		Filters:    []Filter{{Column: "store_id", Op: OpEq, Value: "store-007"}},
		GroupBy:    []string{"firmware_version"},
		Aggregates: []Aggregate{{Func: AggCount, As: "n"}, {Func: AggAvg, Column: "battery_mv", As: "battery"}},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	elapsed := time.Since(qStart)
	// Note that block skipping is weak here *by construction*: the generator
	// round-robins labels across stores, so almost every block contains a row
	// from every store and the per-block distinct set cannot prove absence.
	// TestPredicatePushdownSkipsBlocks measures the clustered case, which is
	// what real ingest produces because a store's telemetry arrives together.
	t.Logf("single-store group-by over %d rows: %s, %d/%d blocks scanned, %d skipped, "+
		"%d rows scanned, %d matched, %d column decodes",
		total, elapsed, res.Stats.BlocksScanned, res.Stats.BlocksScanned+res.Stats.BlocksSkipped,
		res.Stats.BlocksSkipped, res.Stats.RowsScanned, res.Stats.RowsMatched, res.Stats.ColumnsDecoded)

	full := time.Now()
	if _, err := s.Query(Query{
		Aggregates: []Aggregate{{Func: AggQuantile, Column: "temperature_c", Q: 0.99, As: "p99"}},
	}); err != nil {
		t.Fatalf("full scan: %v", err)
	}
	t.Logf("full-scan p99 over %d rows: %s", total, time.Since(full))
}

// TestPredicatePushdownSkipsBlocks asserts the skip count directly, because a
// pushdown that silently stops working is a query that is merely slower and
// nothing else notices.
func TestPredicatePushdownSkipsBlocks(t *testing.T) {
	s := newStore(t, telemetrySchema())
	// Rows are generated in store order within each batch, so any given store's
	// rows cluster into a subset of the blocks.
	for store := 0; store < 8; store++ {
		rows := make([]Row, 0, 20000)
		base := epoch.Add(time.Duration(store) * time.Hour)
		for i := 0; i < 20000; i++ {
			rows = append(rows, Row{
				"reported_at":      base.Add(time.Duration(i) * time.Millisecond),
				"tenant_id":        "acme",
				"store_id":         fmt.Sprintf("store-%03d", store),
				"label_id":         fmt.Sprintf("lbl-%06d", i%500),
				"firmware_version": "1.5.0",
				"battery_mv":       int64(3000),
				"battery_pct":      int64(83),
				"temperature_c":    20.0,
				"rssi":             int64(-60),
				"lqi":              int64(180),
				"mesh_hops":        int64(2),
				"refresh_count":    int64(i),
				"nfc_taps":         int64(0),
				"uptime_seconds":   int64(86400),
				"tamper":           false,
			})
		}
		if err := s.Append(rows...); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	t.Run("an equality filter on a low-cardinality column skips blocks", func(t *testing.T) {
		res, err := s.Query(Query{
			Filters:    []Filter{{Column: "store_id", Op: OpEq, Value: "store-003"}},
			Aggregates: []Aggregate{{Func: AggCount, As: "n"}},
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if res.Stats.BlocksSkipped == 0 {
			t.Fatalf("no blocks were skipped: %+v", res.Stats)
		}
		total := res.Stats.BlocksScanned + res.Stats.BlocksSkipped
		if res.Stats.BlocksScanned > total/4 {
			t.Errorf("scanned %d of %d blocks for one store in eight", res.Stats.BlocksScanned, total)
		}
		if int(res.Rows[0].Values["n"]) != 20000 {
			t.Errorf("count = %v, want 20000", res.Rows[0].Values["n"])
		}
		t.Logf("one store of eight: %d blocks scanned, %d skipped, %d rows scanned for %d matched",
			res.Stats.BlocksScanned, res.Stats.BlocksSkipped, res.Stats.RowsScanned, res.Stats.RowsMatched)
	})

	t.Run("a time range skips segments and blocks", func(t *testing.T) {
		res, err := s.Query(Query{
			From:       epoch.Add(2 * time.Hour),
			To:         epoch.Add(3 * time.Hour),
			Aggregates: []Aggregate{{Func: AggCount, As: "n"}},
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if res.Stats.SegmentsSkipped == 0 && res.Stats.BlocksSkipped == 0 {
			t.Fatalf("a one-hour window over eight hours skipped nothing: %+v", res.Stats)
		}
		if int(res.Rows[0].Values["n"]) != 20000 {
			t.Errorf("count = %v, want the 20000 rows of store-002's hour", res.Rows[0].Values["n"])
		}
		t.Logf("one hour of eight: %d segments scanned, %d skipped; %d blocks scanned, %d skipped",
			res.Stats.SegmentsScanned, res.Stats.SegmentsSkipped,
			res.Stats.BlocksScanned, res.Stats.BlocksSkipped)
	})

	t.Run("a filter that matches nothing skips everything", func(t *testing.T) {
		res, err := s.Query(Query{
			Filters:    []Filter{{Column: "store_id", Op: OpEq, Value: "store-999"}},
			Aggregates: []Aggregate{{Func: AggCount, As: "n"}},
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if res.Stats.BlocksScanned != 0 {
			t.Errorf("scanned %d blocks for a store that does not exist", res.Stats.BlocksScanned)
		}
		if len(res.Rows) != 0 {
			t.Errorf("got %d rows for a store that does not exist", len(res.Rows))
		}
	})

	t.Run("an in filter keeps the blocks it needs", func(t *testing.T) {
		res, err := s.Query(Query{
			Filters:    []Filter{{Column: "store_id", Op: OpIn, Values: []any{"store-001", "store-005"}}},
			GroupBy:    []string{"store_id"},
			Aggregates: []Aggregate{{Func: AggCount, As: "n"}},
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(res.Rows) != 2 {
			t.Fatalf("got %d groups, want 2: %+v", len(res.Rows), res.Rows)
		}
		for _, r := range res.Rows {
			if int(r.Values["n"]) != 20000 {
				t.Errorf("%s has %v rows, want 20000", r.Group["store_id"], r.Values["n"])
			}
		}
		if res.Stats.BlocksSkipped == 0 {
			t.Error("two stores of eight skipped no blocks")
		}
	})
}

// TestQuantileAccuracyAgainstExact measures the t-digest's error on a known
// distribution rather than asserting a bound from the literature.
func TestQuantileAccuracyAgainstExact(t *testing.T) {
	schema := Schema{
		Table: "latency", TimeColumn: "at",
		Columns: []Column{
			{Name: "at", Type: TypeTimestamp},
			{Name: "latency_ms", Type: TypeFloat64},
		},
	}
	s := newStore(t, schema)

	// A log-normal latency distribution, which is the shape delivery latency
	// actually has: a tight body and a long right tail.
	r := newSynth(31337)
	const n = 200000
	exact := make([]float64, 0, n)
	rows := make([]Row, 0, n)
	for i := 0; i < n; i++ {
		v := math.Exp(6.5 + 0.55*r.normal()) // median about 665 ms
		exact = append(exact, v)
		rows = append(rows, Row{
			"at":         epoch.Add(time.Duration(i) * time.Millisecond),
			"latency_ms": v,
		})
	}
	if err := s.Append(rows...); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	sort.Float64s(exact)

	res, err := s.Query(Query{Aggregates: []Aggregate{
		{Func: AggQuantile, Column: "latency_ms", Q: 0.50, As: "p50"},
		{Func: AggQuantile, Column: "latency_ms", Q: 0.90, As: "p90"},
		{Func: AggQuantile, Column: "latency_ms", Q: 0.95, As: "p95"},
		{Func: AggQuantile, Column: "latency_ms", Q: 0.99, As: "p99"},
		{Func: AggQuantile, Column: "latency_ms", Q: 0.999, As: "p999"},
	}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !res.Stats.Approximate {
		t.Error("a quantile result is not marked approximate")
	}

	worst := 0.0
	for _, c := range []struct {
		name string
		q    float64
	}{{"p50", 0.5}, {"p90", 0.9}, {"p95", 0.95}, {"p99", 0.99}, {"p999", 0.999}} {
		want := exact[int(c.q*float64(len(exact)))]
		got := res.Rows[0].Values[c.name]
		relErr := math.Abs(got-want) / want
		if relErr > worst {
			worst = relErr
		}
		t.Logf("synthetic log-normal, %d values: %s estimated %.2f ms against an exact %.2f ms "+
			"(%.3f%% relative error)", n, c.name, got, want, 100*relErr)
		if relErr > 0.02 {
			t.Errorf("%s is %.3f%% out, above the 2%% bar", c.name, 100*relErr)
		}
	}
	t.Logf("worst relative error across the five quantiles: %.3f%%", 100*worst)
}

// TestTimeBucketingAcrossADSTBoundary is the case UTC bucketing gets wrong.
func TestTimeBucketingAcrossADSTBoundary(t *testing.T) {
	schema := Schema{
		Table: "sales", TimeColumn: "at",
		Columns: []Column{
			{Name: "at", Type: TypeTimestamp},
			{Name: "store_id", Type: TypeString},
			{Name: "units", Type: TypeFloat64},
		},
	}
	s := newStore(t, schema)

	// London springs forward at 01:00 on 29 March 2026, so that local day is 23
	// hours long. One row per local hour across four local days, with a unit
	// count that identifies the row.
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	rows := make([]Row, 0, 128)
	perDay := map[string]int{}
	cur := time.Date(2026, 3, 28, 0, 0, 0, 0, london)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, london)
	for cur.Before(end) {
		rows = append(rows, Row{"at": cur.UTC(), "store_id": "lon-1", "units": 1.0})
		perDay[cur.In(london).Format("2006-01-02")]++
		cur = cur.Add(time.Hour)
	}
	if err := s.Append(rows...); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	res, err := s.Query(Query{
		Bucket: 24 * time.Hour, BucketZone: "Europe/London",
		Aggregates: []Aggregate{{Func: AggSum, Column: "units", As: "units"}},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Rows) != 4 {
		t.Fatalf("got %d daily buckets, want 4: %+v", len(res.Rows), res.Rows)
	}
	for _, row := range res.Rows {
		day := row.BucketStart.In(london).Format("2006-01-02")
		want := perDay[day]
		if int(row.Values["units"]) != want {
			t.Errorf("%s has %v rows, want %d", day, row.Values["units"], want)
		}
		// Every bucket must start at local midnight.
		local := row.BucketStart.In(london)
		if local.Hour() != 0 || local.Minute() != 0 {
			t.Errorf("bucket for %s starts at %s local, not midnight", day, local.Format("15:04"))
		}
	}
	// The spring-forward day genuinely has 23 hours, so it must have 23 rows
	// while the others have 24. A UTC-aligned bucketing would split it.
	byDay := map[string]int{}
	for _, row := range res.Rows {
		byDay[row.BucketStart.In(london).Format("2006-01-02")] = int(row.Values["units"])
	}
	if byDay["2026-03-29"] != 23 {
		t.Errorf("the spring-forward day has %d hours, want 23", byDay["2026-03-29"])
	}
	if byDay["2026-03-28"] != 24 || byDay["2026-03-30"] != 24 {
		t.Errorf("the surrounding days have %d and %d hours, want 24 each",
			byDay["2026-03-28"], byDay["2026-03-30"])
	}
	t.Logf("local-day buckets across the spring-forward boundary: %v", byDay)

	// The same query aligned to UTC must produce a different answer, or the
	// zone parameter is not doing anything.
	utcRes, err := s.Query(Query{
		Bucket:     24 * time.Hour,
		Aggregates: []Aggregate{{Func: AggSum, Column: "units", As: "units"}},
	})
	if err != nil {
		t.Fatalf("utc query: %v", err)
	}
	same := len(utcRes.Rows) == len(res.Rows)
	if same {
		for i := range utcRes.Rows {
			if !utcRes.Rows[i].BucketStart.Equal(*res.Rows[i].BucketStart) {
				same = false
				break
			}
		}
	}
	if same {
		t.Error("UTC and London bucketing produced identical boundaries across a DST transition")
	}
}

func TestHourlyBucketing(t *testing.T) {
	schema := Schema{
		Table: "events", TimeColumn: "at",
		Columns: []Column{{Name: "at", Type: TypeTimestamp}, {Name: "n", Type: TypeInt64}},
	}
	s := newStore(t, schema)
	rows := make([]Row, 0, 300)
	for i := 0; i < 300; i++ {
		rows = append(rows, Row{"at": epoch.Add(time.Duration(i) * time.Minute), "n": int64(1)})
	}
	if err := s.Append(rows...); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	res, err := s.Query(Query{Bucket: time.Hour, Aggregates: []Aggregate{{Func: AggCount, As: "n"}}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Rows) != 5 {
		t.Fatalf("got %d hourly buckets for 300 minutes, want 5", len(res.Rows))
	}
	for i, row := range res.Rows {
		if int(row.Values["n"]) != 60 {
			t.Errorf("bucket %d has %v rows, want 60", i, row.Values["n"])
		}
		if i > 0 && !row.BucketStart.After(*res.Rows[i-1].BucketStart) {
			t.Errorf("buckets are not ascending at %d", i)
		}
	}
}

func TestQueryValidationRejectsNonsense(t *testing.T) {
	schema := telemetrySchema()
	tests := []struct {
		name string
		q    Query
	}{
		{"unknown filter column", Query{Filters: []Filter{{Column: "nope", Op: OpEq, Value: 1}}}},
		{"unknown group column", Query{GroupBy: []string{"nope"}}},
		{"unknown aggregate column", Query{Aggregates: []Aggregate{{Func: AggSum, Column: "nope"}}}},
		{"unknown operator", Query{Filters: []Filter{{Column: "lqi", Op: "like", Value: 1}}}},
		{"quantile out of range", Query{Aggregates: []Aggregate{{Func: AggQuantile, Column: "lqi", Q: 1.5}}}},
		{"sum of a string", Query{Aggregates: []Aggregate{{Func: AggSum, Column: "store_id"}}}},
		{"prefix on a number", Query{Filters: []Filter{{Column: "lqi", Op: OpPrefix, Value: "1"}}}},
		{"an empty in filter", Query{Filters: []Filter{{Column: "store_id", Op: OpIn}}}},
		{"an inverted time range", Query{From: epoch.Add(time.Hour), To: epoch}},
		{"an unknown bucket zone", Query{Bucket: time.Hour, BucketZone: "Middle/Earth"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.q.Validate(schema); err == nil {
				t.Error("the query was accepted")
			}
		})
	}
}

func TestRetentionTiering(t *testing.T) {
	// A small segment size, so each hour's data lands in its own files and the
	// tiering has something to move at hour granularity. In production the
	// segment size sets the granularity of retention, and this is that knob.
	s, err := Open(Options{Dir: t.TempDir(), Schema: telemetrySchema(), BlocksPerSegment: 2})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Two batches an hour apart, each large enough to fill a segment.
	for hour := 0; hour < 2; hour++ {
		rows := make([]Row, 0, 20000)
		base := epoch.Add(time.Duration(hour) * time.Hour)
		for i := 0; i < 20000; i++ {
			rows = append(rows, Row{
				"reported_at": base.Add(time.Duration(i) * time.Millisecond),
				"tenant_id":   "acme", "store_id": "s1", "label_id": "l1",
				"firmware_version": "1.5.0", "battery_mv": int64(3000), "battery_pct": int64(80),
				"temperature_c": 20.0, "rssi": int64(-60), "lqi": int64(180), "mesh_hops": int64(1),
				"refresh_count": int64(i), "nfc_taps": int64(0), "uptime_seconds": int64(1),
				"tamper": false,
			})
		}
		if err := s.Append(rows...); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	before, err := s.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if before.Segments["hot"] < 2 {
		t.Fatalf("the hot tier holds %d segments, want at least two", before.Segments["hot"])
	}

	moved, err := s.MoveTier(TierHot, TierWarm, epoch.Add(time.Hour))
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if moved == 0 {
		t.Fatal("nothing moved to warm")
	}
	after, err := s.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if after.Segments["warm"] != moved {
		t.Errorf("warm holds %d segments, want the %d moved", after.Segments["warm"], moved)
	}

	// A query must still find the moved data: tiering changes where the bytes
	// live, not whether they are visible.
	res, err := s.Query(Query{Aggregates: []Aggregate{{Func: AggCount, As: "n"}}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if int(res.Rows[0].Values["n"]) != 40000 {
		t.Errorf("count after tiering = %v, want 40000", res.Rows[0].Values["n"])
	}

	// And a query restricted to the hot tier must not.
	hot, err := s.Query(Query{Tiers: []Tier{TierHot}, Aggregates: []Aggregate{{Func: AggCount, As: "n"}}})
	if err != nil {
		t.Fatalf("hot query: %v", err)
	}
	if int(hot.Rows[0].Values["n"]) >= 40000 {
		t.Errorf("a hot-only query saw %v rows, want fewer than everything", hot.Rows[0].Values["n"])
	}

	dropped, freed, err := s.DropBefore(TierWarm, epoch.Add(time.Hour))
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if dropped == 0 || freed == 0 {
		t.Errorf("dropped %d segments freeing %d bytes", dropped, freed)
	}
	final, err := s.Query(Query{Aggregates: []Aggregate{{Func: AggCount, As: "n"}}})
	if err != nil {
		t.Fatalf("final query: %v", err)
	}
	remaining := int(final.Rows[0].Values["n"])
	// Retention deletes whole segments, and a segment straddling the cut is
	// kept — so the second hour's 20,000 rows survive, plus however much of the
	// first hour shares a segment with them. Asserting the exact figure would
	// be asserting the block boundaries; asserting the bracket is asserting the
	// documented behaviour.
	if remaining < 20000 || remaining >= 40000 {
		t.Errorf("count after dropping the first hour = %d, want the second hour's 20000 "+
			"plus at most part of the first", remaining)
	}
	t.Logf("tiering: %d segments moved hot->warm, %d dropped freeing %d bytes; "+
		"%d of 40000 rows remain (a straddling segment is retained whole)",
		moved, dropped, freed, remaining)
}

func TestAppendRejectsBadRows(t *testing.T) {
	s := newStore(t, Schema{
		Table: "t", TimeColumn: "at",
		Columns: []Column{{Name: "at", Type: TypeTimestamp}, {Name: "v", Type: TypeInt64}},
	})
	if err := s.Append(Row{"at": epoch}); err == nil {
		t.Error("a row missing a column was accepted")
	}
	if err := s.Append(Row{"at": epoch, "v": "not a number"}); err == nil {
		t.Error("a row with the wrong type was accepted")
	}
	if err := s.Append(Row{"at": "not a time", "v": int64(1)}); err == nil {
		t.Error("a row with a bad timestamp was accepted")
	}
}

func TestSchemaValidation(t *testing.T) {
	tests := []struct {
		name string
		s    Schema
	}{
		{"no table", Schema{Columns: []Column{{Name: "a", Type: TypeTimestamp}}, TimeColumn: "a"}},
		{"no columns", Schema{Table: "t", TimeColumn: "a"}},
		{"no time column", Schema{Table: "t", Columns: []Column{{Name: "a", Type: TypeInt64}}}},
		{"time column of the wrong type", Schema{Table: "t", TimeColumn: "a",
			Columns: []Column{{Name: "a", Type: TypeInt64}}}},
		{"duplicate column", Schema{Table: "t", TimeColumn: "a",
			Columns: []Column{{Name: "a", Type: TypeTimestamp}, {Name: "a", Type: TypeInt64}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.s.Validate(); err == nil {
				t.Error("the schema was accepted")
			}
		})
	}
}
