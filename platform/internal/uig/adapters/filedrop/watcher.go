package filedrop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/pipeline"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// MarkerSuffix is appended to a processed file's name to record that it was
// processed.
//
// A sibling marker file rather than a database row, and rather than moving the
// file, because the share belongs to the retailer: their nightly job writes
// into it, their operations team looks at it, and a gateway that moved or
// deleted their files would be the first thing blamed for anything missing. A
// marker is inert, visible, and removable by hand when an operator genuinely
// wants a file reprocessed.
const MarkerSuffix = ".usslp-done"

// DefaultSettle is how long a file must have been untouched before it is read.
//
// This is not paranoia. A file arriving over FTP or a mounted SMB share appears
// in the directory the moment the transfer starts, and reading it then yields a
// truncated price book — half a chain's prices, silently. Waiting for the
// modification time to stop moving is the only portable way to tell a finished
// file from one still being written.
const DefaultSettle = 5 * time.Second

// DefaultInterval is the polling period. A nightly drop does not need a tighter
// one, and a tighter one on a network share is a surprising amount of I/O
// against a filer the retailer also uses for other things.
const DefaultInterval = 30 * time.Second

// QuarantineDirName is the subdirectory malformed files are moved into.
const QuarantineDirName = "usslp-quarantine"

// Receipt is the content of a marker file: what was processed, when, and with
// what outcome.
//
// It carries the content digest so that a file replaced under the same name is
// processed again rather than skipped — which is exactly how a retailer
// corrects a bad export, and skipping it would leave the wrong prices live
// while looking like everything had worked.
type Receipt struct {
	FileName    string    `json:"file_name"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time"`
	ProcessedAt time.Time `json:"processed_at"`
	DeliveryID  string    `json:"delivery_id"`
	Status      string    `json:"status"`
	Emitted     int       `json:"emitted"`
	RowFailures int       `json:"row_failures,omitempty"`
	Detail      string    `json:"detail,omitempty"`
}

// WatchSpec is the JSON form of a watcher, carried inside a filedrop binding's
// options so that installing a nightly feed is a binding change rather than a
// deployment change.
type WatchSpec struct {
	// Dir is the directory to poll.
	Dir string `json:"dir"`
	// Pattern restricts which files are picked up, e.g. "PRICE_*.txt".
	Pattern string `json:"pattern,omitempty"`
	// Interval is the polling period.
	Interval string `json:"interval,omitempty"`
	// Settle is how long a file must be untouched before it is read.
	Settle string `json:"settle,omitempty"`
	// QuarantineDir overrides the default subdirectory.
	QuarantineDir string `json:"quarantine_dir,omitempty"`
	// MaxFileBytes bounds a drop.
	MaxFileBytes int64 `json:"max_file_bytes,omitempty"`

	interval time.Duration
	settle   time.Duration
}

func (s *WatchSpec) validate() error {
	if strings.TrimSpace(s.Dir) == "" {
		return errors.New("watch.dir is required")
	}
	if !filepath.IsAbs(s.Dir) {
		// A relative drop directory resolves against whatever the process's
		// working directory happens to be, which differs between a container, a
		// systemd unit and a developer's shell. Requiring an absolute path
		// turns a silently-wrong directory into a start-up error.
		return fmt.Errorf("watch.dir %q must be an absolute path", s.Dir)
	}
	if s.Pattern != "" {
		if _, err := filepath.Match(s.Pattern, "probe"); err != nil {
			return fmt.Errorf("watch.pattern %q: %w", s.Pattern, err)
		}
	}
	if s.Interval != "" {
		d, err := time.ParseDuration(s.Interval)
		if err != nil || d <= 0 {
			return fmt.Errorf("watch.interval %q must be a positive duration", s.Interval)
		}
		s.interval = d
	}
	if s.Settle != "" {
		d, err := time.ParseDuration(s.Settle)
		if err != nil || d < 0 {
			return fmt.Errorf("watch.settle %q must be a duration", s.Settle)
		}
		s.settle = d
	}
	if s.MaxFileBytes < 0 {
		return errors.New("watch.max_file_bytes must not be negative")
	}
	return nil
}

// WatchConfigFor turns a binding's watch spec into a WatchConfig.
func (o *Options) WatchConfigFor(tenant canon.TenantID, bindingID string, log *obs.Logger) (WatchConfig, bool) {
	if o == nil || o.Watch == nil {
		return WatchConfig{}, false
	}
	s := o.Watch
	return WatchConfig{
		Dir:           s.Dir,
		Pattern:       s.Pattern,
		TenantID:      tenant,
		BindingID:     bindingID,
		Interval:      s.interval,
		Settle:        s.settle,
		QuarantineDir: s.QuarantineDir,
		MaxFileBytes:  s.MaxFileBytes,
		Log:           log,
	}, true
}

// WatchConfig configures a directory watcher.
type WatchConfig struct {
	// Dir is the directory to poll.
	Dir string
	// Pattern is a filepath.Match glob restricting which files are picked up,
	// e.g. "PRICE_*.txt". An empty pattern takes everything that is not a
	// marker or a quarantined file.
	Pattern string
	// TenantID and BindingID route the files into the right integration.
	TenantID  canon.TenantID
	BindingID string
	// Interval is the polling period; zero uses DefaultInterval.
	Interval time.Duration
	// Settle is how long a file must be untouched; zero uses DefaultSettle.
	Settle time.Duration
	// QuarantineDir is where malformed files are moved; empty uses a
	// subdirectory of Dir.
	QuarantineDir string
	// MaxFileBytes refuses a file larger than this rather than reading it into
	// memory. Zero uses DefaultMaxFileBytes.
	MaxFileBytes int64
	// Log is the watcher's logger.
	Log *obs.Logger
	// Now injects a clock for tests.
	Now func() time.Time
}

// DefaultMaxFileBytes bounds a drop. Sixty-four megabytes is several million
// price rows — far more than any real nightly file — and refusing anything
// larger stops a runaway export from taking the gateway's memory with it.
const DefaultMaxFileBytes int64 = 64 << 20

// Watcher polls a directory and feeds files through the ingest pipeline.
type Watcher struct {
	cfg  WatchConfig
	pipe *pipeline.Pipeline
	log  *obs.Logger
	now  func() time.Time

	mu    sync.Mutex
	stats WatchStats
}

// WatchStats is the watcher's own counters, surfaced on the binding health
// endpoint. A file poller that has silently stopped finding files looks
// identical to one whose retailer has stopped sending them, and these are what
// tell the two apart.
type WatchStats struct {
	Scans       uint64    `json:"scans"`
	Seen        uint64    `json:"files_seen"`
	Processed   uint64    `json:"files_processed"`
	Skipped     uint64    `json:"files_skipped"`
	Quarantined uint64    `json:"files_quarantined"`
	LastScanAt  time.Time `json:"last_scan_at,omitempty"`
	LastFileAt  time.Time `json:"last_file_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// NewWatcher creates a watcher, creating the drop and quarantine directories if
// they do not exist.
func NewWatcher(cfg WatchConfig, pipe *pipeline.Pipeline) (*Watcher, error) {
	if cfg.Dir == "" {
		return nil, errors.New("uig/filedrop: watcher needs a directory")
	}
	if pipe == nil {
		return nil, errors.New("uig/filedrop: watcher needs a pipeline")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Settle < 0 {
		cfg.Settle = DefaultSettle
	}
	if cfg.Settle == 0 {
		cfg.Settle = DefaultSettle
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = DefaultMaxFileBytes
	}
	if cfg.QuarantineDir == "" {
		cfg.QuarantineDir = filepath.Join(cfg.Dir, QuarantineDirName)
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Pattern != "" {
		if _, err := filepath.Match(cfg.Pattern, "probe"); err != nil {
			return nil, fmt.Errorf("uig/filedrop: pattern %q: %w", cfg.Pattern, err)
		}
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("uig/filedrop: creating drop directory: %w", err)
	}
	if err := os.MkdirAll(cfg.QuarantineDir, 0o750); err != nil {
		return nil, fmt.Errorf("uig/filedrop: creating quarantine directory: %w", err)
	}
	return &Watcher{cfg: cfg, pipe: pipe, log: cfg.Log, now: cfg.Now}, nil
}

// Stats returns a snapshot of the watcher's counters.
func (w *Watcher) Stats() WatchStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// Run polls until ctx is cancelled. It performs one scan immediately so that a
// gateway restarting after an outage picks up whatever accumulated while it was
// down, rather than waiting out a polling interval with a store's prices stale.
func (w *Watcher) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	if _, err := w.ScanOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.log.Error("uig/filedrop: scan failed", "dir", w.cfg.Dir, "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := w.ScanOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.log.Error("uig/filedrop: scan failed", "dir", w.cfg.Dir, "error", err)
			}
		}
	}
}

// ScanOnce processes every eligible file in the directory once, returning how
// many it processed. It is exported so that an operator endpoint and the test
// suite can drive a scan without waiting for a tick.
func (w *Watcher) ScanOnce(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.recordError(err)
		return 0, fmt.Errorf("uig/filedrop: reading %s: %w", w.cfg.Dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !w.eligible(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	// Sorted so that a retailer writing PRICE_01, PRICE_02, PRICE_03 has them
	// applied in the order they intended. Directory order is not an order.
	sort.Strings(names)

	w.mu.Lock()
	w.stats.Scans++
	w.stats.LastScanAt = w.now().UTC()
	w.stats.Seen += uint64(len(names))
	w.mu.Unlock()

	processed := 0
	for _, name := range names {
		select {
		case <-ctx.Done():
			return processed, ctx.Err()
		default:
		}
		done, err := w.processFile(ctx, name)
		if err != nil {
			w.recordError(err)
			w.log.Error("uig/filedrop: processing file failed",
				"dir", w.cfg.Dir, "file", name, "error", err)
			continue
		}
		if done {
			processed++
		}
	}
	return processed, nil
}

func (w *Watcher) eligible(name string) bool {
	if strings.HasSuffix(name, MarkerSuffix) || strings.HasSuffix(name, ".tmp") ||
		strings.HasPrefix(name, ".") {
		return false
	}
	if w.cfg.Pattern == "" {
		return true
	}
	ok, err := filepath.Match(w.cfg.Pattern, name)
	return err == nil && ok
}

// processFile handles one file, returning whether it was ingested.
func (w *Watcher) processFile(ctx context.Context, name string) (bool, error) {
	path := filepath.Join(w.cfg.Dir, name)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Another replica, or the retailer's own housekeeping, removed it
			// between the listing and now. Not an error.
			return false, nil
		}
		return false, err
	}
	if w.now().Sub(info.ModTime()) < w.cfg.Settle {
		// Still being written. It will be picked up on the next scan.
		w.bumpSkipped()
		return false, nil
	}
	if info.Size() > w.cfg.MaxFileBytes {
		detail := fmt.Sprintf("file is %d bytes, above the %d byte limit", info.Size(), w.cfg.MaxFileBytes)
		if err := w.quarantineFile(path, name, detail); err != nil {
			return false, err
		}
		w.bumpQuarantined()
		return false, nil
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	if skip, err := w.alreadyProcessed(path, digest); err != nil {
		return false, err
	} else if skip {
		w.bumpSkipped()
		return false, nil
	}

	d := &adapter.Delivery{
		ID:         canon.NewULID(),
		TenantID:   w.cfg.TenantID,
		BindingID:  w.cfg.BindingID,
		Body:       body,
		ReceivedAt: w.now(),
		// Path carries the file's name into the audit stream's raw copy, so an
		// auditor reading pos-integration can see which nightly drop a price
		// came from without joining anything.
		Path: name,
		// No Method: a file from the local share has no HTTP transport, and the
		// adapter's Verify uses exactly that to distinguish it from an upload
		// that must be signed.
		Headers:     http.Header{},
		ContentType: "text/plain",
	}
	d.Headers.Set(HeaderFileName, name)
	d.Headers.Set(HeaderFileModTime, info.ModTime().UTC().Format(time.RFC3339))
	d.Headers.Set(HeaderFileSHA256, digest)
	d.Headers.Set(HeaderFileSize, strconv.FormatInt(info.Size(), 10))
	res := w.pipe.Ingest(ctx, d)

	receipt := Receipt{
		FileName:    name,
		SHA256:      digest,
		Size:        info.Size(),
		ModTime:     info.ModTime().UTC(),
		ProcessedAt: w.now().UTC(),
		DeliveryID:  res.DeliveryID,
		Status:      string(res.Status),
		Emitted:     res.Emitted,
		RowFailures: len(res.RowFailures),
		Detail:      res.Detail,
	}

	if res.HTTPStatus >= 400 {
		// The file could not be used. It is moved out of the drop directory so
		// the next scan does not retry it forever, and its receipt travels with
		// it so a support engineer can see why without opening the gateway's
		// logs.
		if err := w.quarantineWithReceipt(path, name, receipt); err != nil {
			return false, err
		}
		w.bumpQuarantined()
		return false, nil
	}

	// The marker is written *after* the pipeline has durably published. A crash
	// in between means the file is scanned again on restart — and the
	// idempotency guard, keyed on the file's name and content digest, turns
	// that second pass into zero new events. Marking first would risk the
	// opposite and far worse outcome: a file recorded as processed whose prices
	// never reached a shelf.
	if err := writeMarkerAtomically(path+MarkerSuffix, receipt); err != nil {
		return false, err
	}
	w.mu.Lock()
	w.stats.Processed++
	w.stats.LastFileAt = w.now().UTC()
	w.mu.Unlock()
	w.log.Info("uig/filedrop: file processed",
		"file", name, "status", string(res.Status), "emitted", res.Emitted,
		"row_failures", len(res.RowFailures), "delivery_id", res.DeliveryID)
	return true, nil
}

// alreadyProcessed reports whether a valid marker exists for this exact
// content.
func (w *Watcher) alreadyProcessed(path, digest string) (bool, error) {
	raw, err := os.ReadFile(path + MarkerSuffix)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var r Receipt
	if err := json.Unmarshal(raw, &r); err != nil {
		// An unreadable marker is treated as no marker. Reprocessing is safe —
		// the idempotency guard catches it — whereas trusting a corrupt marker
		// would silently skip a file.
		return false, nil
	}
	return r.SHA256 == digest, nil
}

// writeMarkerAtomically writes the receipt via a temporary file and a rename.
//
// The rename is the point. A marker half-written when the process is killed
// would be read as "processed" on restart if it happened to unmarshal, and a
// price book skipped because of a torn write is not a failure anyone would
// diagnose quickly. rename(2) within a directory is atomic on every filesystem
// the gateway runs on, so the marker either exists complete or does not exist.
func writeMarkerAtomically(dst string, r Receipt) error {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".usslp-marker-*")
	if err != nil {
		return fmt.Errorf("uig/filedrop: creating marker: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("uig/filedrop: writing marker: %w", err)
	}
	// Flushed before the rename so that a power loss cannot leave a marker that
	// exists but is empty.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("uig/filedrop: syncing marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("uig/filedrop: closing marker: %w", err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return fmt.Errorf("uig/filedrop: marker permissions: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("uig/filedrop: publishing marker: %w", err)
	}
	return nil
}

func (w *Watcher) quarantineFile(path, name, detail string) error {
	return w.quarantineWithReceipt(path, name, Receipt{
		FileName:    name,
		ProcessedAt: w.now().UTC(),
		Status:      "quarantined",
		Detail:      detail,
	})
}

// quarantineWithReceipt moves a file out of the drop directory alongside an
// explanation.
//
// The timestamped name means two bad files a week apart do not overwrite each
// other, which matters because the second one is usually the evidence that the
// first was not a one-off.
func (w *Watcher) quarantineWithReceipt(path, name string, r Receipt) error {
	stamp := w.now().UTC().Format("20060102T150405Z")
	dst := filepath.Join(w.cfg.QuarantineDir, stamp+"_"+name)
	if err := moveFile(path, dst); err != nil {
		return fmt.Errorf("uig/filedrop: quarantining %s: %w", name, err)
	}
	if err := writeMarkerAtomically(dst+".error.json", r); err != nil {
		return err
	}
	w.log.Warn("uig/filedrop: file quarantined",
		"file", name, "quarantine_path", dst, "detail", r.Detail)
	return nil
}

// moveFile renames, falling back to copy-and-remove when the quarantine
// directory is on a different filesystem — which it routinely is, because the
// drop directory is a mounted share and the quarantine is local disk.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	in.Close()
	return os.Remove(src)
}

func (w *Watcher) bumpSkipped() {
	w.mu.Lock()
	w.stats.Skipped++
	w.mu.Unlock()
}

func (w *Watcher) bumpQuarantined() {
	w.mu.Lock()
	w.stats.Quarantined++
	w.mu.Unlock()
}

func (w *Watcher) recordError(err error) {
	w.mu.Lock()
	w.stats.LastError = err.Error()
	w.mu.Unlock()
}
