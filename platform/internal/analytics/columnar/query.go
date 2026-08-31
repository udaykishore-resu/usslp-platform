package columnar

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Op is a filter comparison.
type Op string

// The supported comparisons. The set is closed and every one is a simple
// predicate on a single column: this is a structured query API, not an
// expression language, so there is no place for a caller to inject anything and
// no parser to get wrong.
const (
	OpEq  Op = "eq"
	OpNe  Op = "ne"
	OpLt  Op = "lt"
	OpLte Op = "lte"
	OpGt  Op = "gt"
	OpGte Op = "gte"
	// OpIn matches any of a set of values, which is how "these forty stores"
	// is expressed without forty queries.
	OpIn Op = "in"
	// OpPrefix matches a string prefix, for hierarchical identifiers like
	// "eu-west-".
	OpPrefix Op = "prefix"
)

// Filter is one predicate.
type Filter struct {
	Column string `json:"column"`
	Op     Op     `json:"op"`
	// Value is the comparand for the scalar operators.
	Value any `json:"value,omitempty"`
	// Values is the set for OpIn.
	Values []any `json:"values,omitempty"`
}

// AggFunc is an aggregation.
type AggFunc string

// The supported aggregations.
const (
	AggCount AggFunc = "count"
	AggSum   AggFunc = "sum"
	AggAvg   AggFunc = "avg"
	AggMin   AggFunc = "min"
	AggMax   AggFunc = "max"
	// AggQuantile needs Q. It is computed with a t-digest, so it is an estimate;
	// the result marks it as one rather than letting a reader assume otherwise.
	AggQuantile AggFunc = "quantile"
	// AggCountDistinct counts distinct values exactly. It is exact rather than
	// sketched because the cardinalities it is asked about — stores, SKUs,
	// labels in a store — are in the thousands, where an exact set costs less
	// than the code to approximate one.
	AggCountDistinct AggFunc = "count_distinct"
)

// Aggregate is one output measure.
type Aggregate struct {
	Func AggFunc `json:"func"`
	// Column is the column aggregated. Ignored by count.
	Column string `json:"column,omitempty"`
	// Q is the quantile in (0, 1), for AggQuantile.
	Q float64 `json:"q,omitempty"`
	// As names the output field. Defaults to func_column.
	As string `json:"as,omitempty"`
}

// Query is a structured analytical query.
//
// # Why structured and not SQL
//
// This API is reachable from the platform's HTTP surface, which means its input
// is attacker-adjacent. A SQL string would need a parser, a planner and an
// injection story; a struct needs none of them, and the set of questions the
// platform's reports actually ask — filter, bucket, group, aggregate — fits in
// one without contortion. The cost is that a genuinely novel question needs a
// code change, which for a fixed set of retail reports is the right trade.
type Query struct {
	// From and To bound the time column, half-open. A zero From or To is
	// unbounded, which is legal and slow and should be rare.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Filters are ANDed.
	Filters []Filter `json:"filters,omitempty"`
	// GroupBy names the dimension columns. Empty means one group over
	// everything.
	GroupBy []string `json:"group_by,omitempty"`
	// Bucket, when non-zero, adds a time dimension of this width.
	Bucket time.Duration `json:"bucket,omitempty"`
	// BucketZone is the IANA location the buckets are aligned in.
	//
	// It matters for any bucket of a day or more: "daily sales" means local
	// days, and a store in a zone with daylight saving has a 23-hour and a
	// 25-hour day each year. Aligning to UTC would split those days across two
	// buckets and put a visible notch in every trend chart twice a year.
	BucketZone string `json:"bucket_zone,omitempty"`
	// Aggregates are the measures. Empty means a bare count.
	Aggregates []Aggregate `json:"aggregates,omitempty"`
	// Limit bounds the returned groups. Zero means DefaultQueryLimit.
	Limit int `json:"limit,omitempty"`
	// OrderBy names an output field to sort by, descending unless Ascending.
	OrderBy   string `json:"order_by,omitempty"`
	Ascending bool   `json:"ascending,omitempty"`
	// Tiers restricts which storage tiers are read. Empty means all of them.
	Tiers []Tier `json:"tiers,omitempty"`
}

// DefaultQueryLimit bounds a result set.
//
// Ten thousand groups is more than any dashboard renders and enough for a
// per-store breakdown of a large estate. The limit exists because a group-by on
// an unexpectedly high-cardinality column — label id, say — would otherwise
// build a fifty-million-entry map inside the query path.
const DefaultQueryLimit = 10000

// Result is a query's output.
type Result struct {
	// Rows are the grouped, aggregated results.
	Rows []ResultRow `json:"rows"`
	// Stats describe the work done, which is what makes a slow query
	// diagnosable without a profiler.
	Stats QueryStats `json:"stats"`
}

// ResultRow is one group.
type ResultRow struct {
	// Group holds the dimension values, keyed by column name.
	Group map[string]string `json:"group,omitempty"`
	// BucketStart is the time bucket, when one was requested.
	BucketStart *time.Time `json:"bucket_start,omitempty"`
	// Values holds the aggregates, keyed by output name.
	Values map[string]float64 `json:"values"`
	// Rows is how many source rows fell into this group.
	Rows int64 `json:"rows"`
}

// QueryStats is the work a query did.
type QueryStats struct {
	// SegmentsScanned and SegmentsSkipped count files.
	SegmentsScanned int `json:"segments_scanned"`
	SegmentsSkipped int `json:"segments_skipped"`
	// BlocksScanned and BlocksSkipped count blocks. The skip count is the
	// number that proves predicate pushdown is working, which is why it is
	// reported on every query rather than only in a test.
	BlocksScanned int `json:"blocks_scanned"`
	BlocksSkipped int `json:"blocks_skipped"`
	// RowsScanned and RowsMatched count rows decoded and rows that passed the
	// filters.
	RowsScanned int64 `json:"rows_scanned"`
	RowsMatched int64 `json:"rows_matched"`
	// ColumnsDecoded is how many column decodes ran. A column-store query that
	// decodes every column has lost its main advantage, and this is where that
	// shows.
	ColumnsDecoded int `json:"columns_decoded"`
	// Groups is the group count before the limit.
	Groups int `json:"groups"`
	// Truncated reports that the limit dropped groups.
	Truncated bool `json:"truncated,omitempty"`
	// Elapsed is the wall-clock time.
	Elapsed time.Duration `json:"elapsed_ns"`
	// Approximate is true when any aggregate was estimated rather than exact.
	Approximate bool `json:"approximate,omitempty"`
}

// ErrQuery marks a query that cannot be executed.
var ErrQuery = errors.New("columnar: invalid query")

// Validate checks a query against the schema before any I/O.
func (q Query) Validate(schema Schema) error {
	for _, f := range q.Filters {
		i := schema.Index(f.Column)
		if i < 0 {
			return fmt.Errorf("%w: unknown column %q in a filter", ErrQuery, f.Column)
		}
		switch f.Op {
		case OpEq, OpNe, OpLt, OpLte, OpGt, OpGte:
			if f.Value == nil {
				return fmt.Errorf("%w: filter on %s needs a value", ErrQuery, f.Column)
			}
		case OpIn:
			if len(f.Values) == 0 {
				return fmt.Errorf("%w: an `in` filter on %s needs values", ErrQuery, f.Column)
			}
		case OpPrefix:
			if schema.Columns[i].Type != TypeString {
				return fmt.Errorf("%w: prefix filters apply to strings, and %s is a %s",
					ErrQuery, f.Column, schema.Columns[i].Type)
			}
		default:
			return fmt.Errorf("%w: unknown operator %q", ErrQuery, f.Op)
		}
	}
	for _, g := range q.GroupBy {
		if schema.Index(g) < 0 {
			return fmt.Errorf("%w: unknown column %q in group by", ErrQuery, g)
		}
	}
	for _, a := range q.Aggregates {
		switch a.Func {
		case AggCount:
		case AggSum, AggAvg, AggMin, AggMax, AggQuantile, AggCountDistinct:
			i := schema.Index(a.Column)
			if i < 0 {
				return fmt.Errorf("%w: unknown column %q in a %s aggregate", ErrQuery, a.Column, a.Func)
			}
			if a.Func == AggQuantile && (a.Q <= 0 || a.Q >= 1) {
				return fmt.Errorf("%w: quantile q=%v is outside (0, 1)", ErrQuery, a.Q)
			}
			if a.Func != AggCountDistinct && schema.Columns[i].Type == TypeString {
				return fmt.Errorf("%w: %s cannot be applied to the string column %s", ErrQuery, a.Func, a.Column)
			}
		default:
			return fmt.Errorf("%w: unknown aggregate %q", ErrQuery, a.Func)
		}
	}
	if q.Bucket < 0 {
		return fmt.Errorf("%w: a negative bucket width", ErrQuery)
	}
	if q.BucketZone != "" {
		if _, err := time.LoadLocation(q.BucketZone); err != nil {
			return fmt.Errorf("%w: unknown bucket zone %q", ErrQuery, q.BucketZone)
		}
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.To.After(q.From) {
		return fmt.Errorf("%w: the time range %s..%s is empty", ErrQuery, q.From, q.To)
	}
	return nil
}

// Query executes a query.
func (s *Store) Query(q Query) (Result, error) {
	start := time.Now()
	if err := q.Validate(s.schema); err != nil {
		return Result{}, err
	}
	if q.Limit <= 0 {
		q.Limit = DefaultQueryLimit
	}
	var loc *time.Location = time.UTC
	if q.BucketZone != "" {
		l, err := time.LoadLocation(q.BucketZone)
		if err != nil {
			return Result{}, fmt.Errorf("%w: %v", ErrQuery, err)
		}
		loc = l
	}

	var fromNanos, toNanos int64
	if !q.From.IsZero() {
		fromNanos = q.From.UnixNano()
	}
	if !q.To.IsZero() {
		toNanos = q.To.UnixNano()
	}

	segs, skippedFiles, err := s.segments(fromNanos, toNanos, q.Tiers)
	if err != nil {
		return Result{}, err
	}
	stats := QueryStats{SegmentsSkipped: skippedFiles}

	// Resolve the columns the query actually needs, once. Decoding only these
	// is the whole point of the layout.
	needed := map[int]bool{}
	timeIdx := s.schema.Index(s.schema.TimeColumn)
	if q.Bucket > 0 || fromNanos != 0 || toNanos != 0 {
		needed[timeIdx] = true
	}
	for _, f := range q.Filters {
		needed[s.schema.Index(f.Column)] = true
	}
	for _, g := range q.GroupBy {
		needed[s.schema.Index(g)] = true
	}
	for _, a := range q.Aggregates {
		if a.Func != AggCount {
			needed[s.schema.Index(a.Column)] = true
		}
	}

	groups := map[string]*groupState{}
	order := make([]string, 0, 64)
	keyBuf := make([]byte, 0, 128)

	for _, seg := range segs {
		stats.SegmentsScanned++
		for _, raw := range seg.blocks {
			block, _, err := DecodeBlockHeader(s.schema, raw)
			if err != nil {
				return Result{}, fmt.Errorf("columnar: %s: %w", seg.path, err)
			}
			if s.skipBlock(block, q, fromNanos, toNanos, timeIdx) {
				stats.BlocksSkipped++
				continue
			}
			stats.BlocksScanned++
			stats.RowsScanned += int64(block.Rows)

			cols := make(map[int]ColumnValues, len(needed))
			for idx := range needed {
				v, err := block.Decode(s.schema, idx)
				if err != nil {
					return Result{}, err
				}
				cols[idx] = v
				stats.ColumnsDecoded++
			}

			for row := 0; row < block.Rows; row++ {
				if timeIdx >= 0 {
					if tv, ok := cols[timeIdx]; ok {
						ts := tv.Ints[row]
						if fromNanos != 0 && ts < fromNanos {
							continue
						}
						if toNanos != 0 && ts >= toNanos {
							continue
						}
					}
				}
				if !s.rowMatches(q.Filters, cols, row) {
					continue
				}
				stats.RowsMatched++

				keyBuf = keyBuf[:0]
				var bucketStart *time.Time
				if q.Bucket > 0 {
					bs := bucketOf(time.Unix(0, cols[timeIdx].Ints[row]), q.Bucket, loc)
					bucketStart = &bs
					keyBuf = append(keyBuf, []byte(bs.UTC().Format(time.RFC3339Nano))...)
					keyBuf = append(keyBuf, 0)
				}
				for _, g := range q.GroupBy {
					keyBuf = append(keyBuf, []byte(stringAt(cols[s.schema.Index(g)], row))...)
					keyBuf = append(keyBuf, 0)
				}
				key := string(keyBuf)

				st, ok := groups[key]
				if !ok {
					st = newGroupState(q, bucketStart)
					for _, g := range q.GroupBy {
						st.group[g] = stringAt(cols[s.schema.Index(g)], row)
					}
					groups[key] = st
					order = append(order, key)
				}
				st.observe(q, s.schema, cols, row)
			}
		}
	}

	stats.Groups = len(groups)
	out := make([]ResultRow, 0, len(order))
	for _, key := range order {
		st := groups[key]
		row := ResultRow{Group: st.group, BucketStart: st.bucket, Rows: st.rows,
			Values: st.finish(q, &stats)}
		out = append(out, row)
	}

	sortResult(out, q)
	if len(out) > q.Limit {
		out = out[:q.Limit]
		stats.Truncated = true
	}
	stats.Elapsed = time.Since(start)
	return Result{Rows: out, Stats: stats}, nil
}

// skipBlock decides whether a block can be excluded from its header alone.
//
// This is predicate pushdown. Every skip is a block's worth of decompression
// and filtering that never happens, and on a query scoped to one store out of
// two thousand it is the difference between a report that returns in
// milliseconds and one that returns in minutes.
func (s *Store) skipBlock(b *Block, q Query, fromNanos, toNanos int64, timeIdx int) bool {
	if timeIdx >= 0 {
		st := b.Stats[timeIdx]
		if fromNanos != 0 && st.MaxInt < fromNanos {
			return true
		}
		if toNanos != 0 && st.MinInt >= toNanos {
			return true
		}
	}
	for _, f := range q.Filters {
		idx := s.schema.Index(f.Column)
		col := s.schema.Columns[idx]
		st := b.Stats[idx]
		switch col.Type {
		case TypeTimestamp, TypeInt64:
			v, ok := asInt(f.Value)
			switch f.Op {
			case OpEq:
				if ok && (v < st.MinInt || v > st.MaxInt) {
					return true
				}
			case OpGt:
				if ok && st.MaxInt <= v {
					return true
				}
			case OpGte:
				if ok && st.MaxInt < v {
					return true
				}
			case OpLt:
				if ok && st.MinInt >= v {
					return true
				}
			case OpLte:
				if ok && st.MinInt > v {
					return true
				}
			case OpIn:
				// The block can be skipped only if every wanted value is
				// outside its range.
				anyInRange := false
				for _, raw := range f.Values {
					if iv, ok := asInt(raw); ok && iv >= st.MinInt && iv <= st.MaxInt {
						anyInRange = true
						break
					}
				}
				if !anyInRange && len(f.Values) > 0 {
					return true
				}
			}
		case TypeFloat64:
			v, ok := asFloat(f.Value)
			switch f.Op {
			case OpEq:
				if ok && (v < st.MinFloat || v > st.MaxFloat) {
					return true
				}
			case OpGt:
				if ok && st.MaxFloat <= v {
					return true
				}
			case OpGte:
				if ok && st.MaxFloat < v {
					return true
				}
			case OpLt:
				if ok && st.MinFloat >= v {
					return true
				}
			case OpLte:
				if ok && st.MinFloat > v {
					return true
				}
			}
		case TypeString:
			// The distinct set proves absence exactly, which is far stronger
			// than a range check on strings. It is only usable when the set was
			// small enough to keep whole.
			if st.DistinctTruncated || st.Distinct == nil {
				continue
			}
			switch f.Op {
			case OpEq:
				if v, ok := f.Value.(string); ok && !containsString(st.Distinct, v) {
					return true
				}
			case OpIn:
				any := false
				for _, raw := range f.Values {
					if v, ok := raw.(string); ok && containsString(st.Distinct, v) {
						any = true
						break
					}
				}
				if !any && len(f.Values) > 0 {
					return true
				}
			case OpPrefix:
				if v, ok := f.Value.(string); ok {
					any := false
					for _, d := range st.Distinct {
						if strings.HasPrefix(d, v) {
							any = true
							break
						}
					}
					if !any {
						return true
					}
				}
			}
		}
	}
	return false
}

func containsString(sorted []string, v string) bool {
	i := sort.SearchStrings(sorted, v)
	return i < len(sorted) && sorted[i] == v
}

// rowMatches applies the filters to one decoded row.
func (s *Store) rowMatches(filters []Filter, cols map[int]ColumnValues, row int) bool {
	for _, f := range filters {
		idx := s.schema.Index(f.Column)
		v := cols[idx]
		if !matchOne(f, v, row) {
			return false
		}
	}
	return true
}

func matchOne(f Filter, v ColumnValues, row int) bool {
	switch v.Type {
	case TypeTimestamp, TypeInt64:
		x := v.Ints[row]
		switch f.Op {
		case OpEq:
			c, ok := asInt(f.Value)
			return ok && x == c
		case OpNe:
			c, ok := asInt(f.Value)
			return !ok || x != c
		case OpLt:
			c, ok := asInt(f.Value)
			return ok && x < c
		case OpLte:
			c, ok := asInt(f.Value)
			return ok && x <= c
		case OpGt:
			c, ok := asInt(f.Value)
			return ok && x > c
		case OpGte:
			c, ok := asInt(f.Value)
			return ok && x >= c
		case OpIn:
			for _, raw := range f.Values {
				if c, ok := asInt(raw); ok && x == c {
					return true
				}
			}
			return false
		}
	case TypeFloat64:
		x := v.Floats[row]
		switch f.Op {
		case OpEq:
			c, ok := asFloat(f.Value)
			return ok && x == c
		case OpNe:
			c, ok := asFloat(f.Value)
			return !ok || x != c
		case OpLt:
			c, ok := asFloat(f.Value)
			return ok && x < c
		case OpLte:
			c, ok := asFloat(f.Value)
			return ok && x <= c
		case OpGt:
			c, ok := asFloat(f.Value)
			return ok && x > c
		case OpGte:
			c, ok := asFloat(f.Value)
			return ok && x >= c
		case OpIn:
			for _, raw := range f.Values {
				if c, ok := asFloat(raw); ok && x == c {
					return true
				}
			}
			return false
		}
	case TypeString:
		x := v.Strings[row]
		switch f.Op {
		case OpEq:
			c, ok := f.Value.(string)
			return ok && x == c
		case OpNe:
			c, ok := f.Value.(string)
			return !ok || x != c
		case OpPrefix:
			c, ok := f.Value.(string)
			return ok && strings.HasPrefix(x, c)
		case OpIn:
			for _, raw := range f.Values {
				if c, ok := raw.(string); ok && x == c {
					return true
				}
			}
			return false
		}
	case TypeBool:
		x := v.Bools[row]
		switch f.Op {
		case OpEq:
			c, ok := f.Value.(bool)
			return ok && x == c
		case OpNe:
			c, ok := f.Value.(bool)
			return !ok || x != c
		}
	}
	return false
}

// asInt coerces a JSON-decoded value to an int64. JSON numbers arrive as
// float64, which is why the float case is here and is not a mistake.
func asInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	case time.Time:
		return t.UnixNano(), true
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	}
	return 0, false
}

// stringAt renders a column value as a group key.
func stringAt(v ColumnValues, row int) string {
	switch v.Type {
	case TypeTimestamp, TypeInt64:
		return formatInt(v.Ints[row])
	case TypeFloat64:
		return formatFloat(v.Floats[row])
	case TypeString:
		return v.Strings[row]
	case TypeBool:
		if v.Bools[row] {
			return "true"
		}
		return "false"
	}
	return ""
}

func formatInt(v int64) string {
	return fmt.Sprintf("%d", v)
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%g", v)
}

// numericAt returns a column's value as a float, for aggregation.
func numericAt(v ColumnValues, row int) (float64, bool) {
	switch v.Type {
	case TypeTimestamp, TypeInt64:
		return float64(v.Ints[row]), true
	case TypeFloat64:
		return v.Floats[row], true
	case TypeBool:
		if v.Bools[row] {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// bucketOf aligns an instant to a bucket boundary in a location.
//
// # Why buckets of a day or more are aligned in local time
//
// Truncating a UTC instant to a multiple of 24 hours gives UTC days, which are
// not the days a retailer trades in. Aligning in the store's own zone gives
// local days, including the 23-hour and 25-hour ones that daylight saving
// produces — and those genuinely are one trading day each, so a chart of daily
// sales should show one bar for them, not two partial ones.
//
// Sub-day buckets are aligned from the local midnight of the instant's own day
// for the same reason: an hourly chart in a zone with a 30-minute offset should
// have bars on the hour locally, not on the hour in UTC.
func bucketOf(t time.Time, width time.Duration, loc *time.Location) time.Time {
	if width <= 0 {
		return t.UTC()
	}
	local := t.In(loc)
	if width >= 24*time.Hour {
		days := int(width / (24 * time.Hour))
		if days < 1 {
			days = 1
		}
		midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		if days == 1 {
			return midnight
		}
		// Multi-day buckets are anchored to the Unix epoch's local midnight so
		// that consecutive queries agree about where a week starts.
		epoch := time.Date(1970, 1, 1, 0, 0, 0, 0, loc)
		elapsed := int(midnight.Sub(epoch) / (24 * time.Hour))
		return epoch.AddDate(0, 0, elapsed-elapsed%days)
	}
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	since := local.Sub(midnight)
	return midnight.Add(since - since%width)
}

// groupState accumulates one group's aggregates.
type groupState struct {
	group  map[string]string
	bucket *time.Time
	rows   int64

	sums     []float64
	counts   []int64
	mins     []float64
	maxs     []float64
	digests  []*TDigest
	distinct []map[string]struct{}
}

func newGroupState(q Query, bucket *time.Time) *groupState {
	n := len(q.Aggregates)
	st := &groupState{
		group: make(map[string]string, len(q.GroupBy)), bucket: bucket,
		sums: make([]float64, n), counts: make([]int64, n),
		mins: make([]float64, n), maxs: make([]float64, n),
		digests: make([]*TDigest, n), distinct: make([]map[string]struct{}, n),
	}
	for i, a := range q.Aggregates {
		st.mins[i] = math.Inf(1)
		st.maxs[i] = math.Inf(-1)
		switch a.Func {
		case AggQuantile:
			st.digests[i] = NewTDigest(DefaultCompression)
		case AggCountDistinct:
			st.distinct[i] = map[string]struct{}{}
		}
	}
	return st
}

func (st *groupState) observe(q Query, schema Schema, cols map[int]ColumnValues, row int) {
	st.rows++
	for i, a := range q.Aggregates {
		if a.Func == AggCount {
			st.counts[i]++
			continue
		}
		v := cols[schema.Index(a.Column)]
		if a.Func == AggCountDistinct {
			st.distinct[i][stringAt(v, row)] = struct{}{}
			continue
		}
		x, ok := numericAt(v, row)
		if !ok {
			continue
		}
		st.counts[i]++
		st.sums[i] += x
		if x < st.mins[i] {
			st.mins[i] = x
		}
		if x > st.maxs[i] {
			st.maxs[i] = x
		}
		if a.Func == AggQuantile {
			st.digests[i].Add(x)
		}
	}
}

func (st *groupState) finish(q Query, stats *QueryStats) map[string]float64 {
	out := make(map[string]float64, len(q.Aggregates)+1)
	for i, a := range q.Aggregates {
		name := a.As
		if name == "" {
			name = string(a.Func)
			if a.Column != "" {
				name += "_" + a.Column
			}
			if a.Func == AggQuantile {
				name = fmt.Sprintf("p%g_%s", a.Q*100, a.Column)
			}
		}
		switch a.Func {
		case AggCount:
			out[name] = float64(st.counts[i])
		case AggSum:
			out[name] = st.sums[i]
		case AggAvg:
			if st.counts[i] > 0 {
				out[name] = st.sums[i] / float64(st.counts[i])
			}
		case AggMin:
			if st.counts[i] > 0 {
				out[name] = st.mins[i]
			}
		case AggMax:
			if st.counts[i] > 0 {
				out[name] = st.maxs[i]
			}
		case AggQuantile:
			out[name] = st.digests[i].Quantile(a.Q)
			stats.Approximate = true
		case AggCountDistinct:
			out[name] = float64(len(st.distinct[i]))
		}
	}
	if len(q.Aggregates) == 0 {
		out["count"] = float64(st.rows)
	}
	return out
}

// sortResult orders the rows.
//
// The default — time bucket ascending, then group key — is what a chart wants.
// An explicit OrderBy sorts by an aggregate, which is what a "worst ten stores"
// report wants. Either way the order is total, because two rows that compare
// equal on the named field are broken by their group key: a report whose row
// order changes between identical runs is one nobody trusts.
func sortResult(rows []ResultRow, q Query) {
	groupKey := func(r ResultRow) string {
		parts := make([]string, 0, len(q.GroupBy))
		for _, g := range q.GroupBy {
			parts = append(parts, r.Group[g])
		}
		return strings.Join(parts, "\x00")
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if q.OrderBy != "" {
			a, b := rows[i].Values[q.OrderBy], rows[j].Values[q.OrderBy]
			if a != b {
				if q.Ascending {
					return a < b
				}
				return a > b
			}
			return groupKey(rows[i]) < groupKey(rows[j])
		}
		if rows[i].BucketStart != nil && rows[j].BucketStart != nil &&
			!rows[i].BucketStart.Equal(*rows[j].BucketStart) {
			return rows[i].BucketStart.Before(*rows[j].BucketStart)
		}
		return groupKey(rows[i]) < groupKey(rows[j])
	})
}
