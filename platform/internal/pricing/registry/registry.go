// Package registry is the versioned model store: what models exist, which one
// is serving, how each scored on a holdout, and what quantising it cost.
//
// # Why a registry rather than a file on a disk
//
// A pricing model is a decision-making artefact that a regulator may ask about
// two years after it stopped serving. The questions that get asked are "which
// model set this price", "what evidence was there that it was better than the
// one before", and "was the model on the shelf the model that was tested". A
// directory of timestamped files answers none of them. The registry stores the
// model bytes, the metadata, the holdout metrics, the champion/challenger
// comparison that justified each promotion, and the quantisation report for the
// edge artefact, keyed so that every one of those questions is a lookup.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// Stage is a model's deployment state.
type Stage string

// The stages a model moves through.
const (
	// StageChallenger is a newly registered model. It is evaluated but never
	// serves.
	StageChallenger Stage = "challenger"
	// StageChampion is the model currently serving. Exactly one model per
	// (tenant, store, purpose) holds it.
	StageChampion Stage = "champion"
	// StageArchived is a model that has been superseded. It is retained, not
	// deleted, because "which model set this price" must stay answerable.
	StageArchived Stage = "archived"
)

// Purpose distinguishes the models a store runs simultaneously.
type Purpose string

// The model purposes.
const (
	// PurposeDemand is the Tier-2 per-store demand model.
	PurposeDemand Purpose = "demand"
	// PurposeForecast is the Tier-3 sequence model.
	PurposeForecast Purpose = "forecast"
	// PurposeAnomaly is the telemetry isolation forest.
	PurposeAnomaly Purpose = "anomaly"
)

// Errors the registry returns.
var (
	// ErrNotFound is returned for an unknown model or an empty champion slot.
	ErrNotFound = errors.New("registry: model not found")
	// ErrInvalid marks a malformed registration.
	ErrInvalid = errors.New("registry: invalid model registration")
	// ErrPromotionRefused is returned when a promotion would violate the
	// registry's own safety rules, as distinct from an operator declining one.
	ErrPromotionRefused = errors.New("registry: promotion refused")
)

// Metadata is everything known about a model except its weights.
type Metadata struct {
	// ID is the model identifier, assigned at registration.
	ID string `json:"id"`
	// Tenant and Store scope the model. An empty store means a tenant-wide
	// model, which is what a chain with too little per-store history falls back
	// to.
	Tenant canon.TenantID `json:"tenant_id"`
	Store  canon.StoreID  `json:"store_id,omitempty"`
	// Purpose and Kind describe what it is.
	Purpose Purpose      `json:"purpose"`
	Kind    ml.ModelKind `json:"kind"`
	// KindName is the human-readable kind, so an API response is readable
	// without the enum table.
	KindName string `json:"kind_name"`
	// Stage is the deployment state.
	Stage Stage `json:"stage"`
	// Version is a monotonic counter within (tenant, store, purpose).
	Version int64 `json:"version"`
	// CreatedAt is when the model was registered.
	CreatedAt time.Time `json:"created_at"`
	// PromotedAt is when it became champion, if it ever did.
	PromotedAt *time.Time `json:"promoted_at,omitempty"`
	// ArchivedAt is when it was superseded.
	ArchivedAt *time.Time `json:"archived_at,omitempty"`

	// TrainingRows and HoldoutRows record the data the model rests on. A
	// beautiful metric on 40 holdout rows is not evidence, and recording the
	// row count is what lets a reviewer see that.
	TrainingRows int `json:"training_rows"`
	HoldoutRows  int `json:"holdout_rows"`
	// TrainingWindow is the period the training data spans.
	TrainingWindowStart time.Time `json:"training_window_start,omitempty"`
	TrainingWindowEnd   time.Time `json:"training_window_end,omitempty"`
	// FeatureNames is the input contract. A model served against a differently
	// ordered vector produces confident nonsense, so the names are stored and
	// checked at load.
	FeatureNames []string `json:"feature_names,omitempty"`
	// Hyperparameters is the free-form training configuration, retained so a
	// training run can be reproduced.
	Hyperparameters map[string]string `json:"hyperparameters,omitempty"`

	// Metrics are the holdout metrics measured at registration.
	Metrics ml.Metrics `json:"metrics"`
	// Comparison is the champion/challenger verdict recorded at promotion.
	Comparison *ml.ChampionChallenger `json:"comparison,omitempty"`
	// Quantisation is the measured cost of the int8 edge artefact.
	Quantisation *ml.QuantisationReport `json:"quantisation,omitempty"`

	// Bytes is the serialised model size.
	Bytes int `json:"bytes"`
	// EdgeBytes is the serialised int8 artefact size, when one exists.
	EdgeBytes int `json:"edge_bytes,omitempty"`
	// Notes is operator-supplied context.
	Notes string `json:"notes,omitempty"`
}

// Registry stores models and their metadata.
type Registry struct {
	kv *kvstore.Store
	// mu serialises the read-modify-write sequences — version allocation and
	// champion swaps — that the kvstore's per-key atomicity does not cover.
	// The registry sees a handful of writes per store per day, so a single
	// mutex costs nothing and removes an entire class of race.
	mu sync.Mutex
}

// New builds a registry over a key/value store.
func New(kv *kvstore.Store) (*Registry, error) {
	if kv == nil {
		return nil, fmt.Errorf("%w: nil kv store", ErrInvalid)
	}
	return &Registry{kv: kv}, nil
}

// Slot identifies the champion position a model competes for.
type Slot struct {
	Tenant  canon.TenantID
	Store   canon.StoreID
	Purpose Purpose
}

// Validate rejects slots that would corrupt the key space.
func (s Slot) Validate() error {
	if s.Tenant == "" || !canon.ValidID(string(s.Tenant)) {
		return fmt.Errorf("%w: tenant %q", ErrInvalid, s.Tenant)
	}
	if s.Store != "" && !canon.ValidID(string(s.Store)) {
		return fmt.Errorf("%w: store %q", ErrInvalid, s.Store)
	}
	switch s.Purpose {
	case PurposeDemand, PurposeForecast, PurposeAnomaly:
	default:
		return fmt.Errorf("%w: purpose %q", ErrInvalid, s.Purpose)
	}
	return nil
}

func (s Slot) key() string {
	return string(s.Tenant) + "\x00" + string(s.Store) + "\x00" + string(s.Purpose)
}

// Registration is a model being added to the registry.
type Registration struct {
	Slot Slot
	Kind ml.ModelKind
	// Body is the serialised model, produced by the model's MarshalBinary.
	Body []byte
	// EdgeBody is the serialised int8 artefact, when one exists.
	EdgeBody []byte
	// Metrics are the holdout metrics.
	Metrics ml.Metrics
	// Quantisation is the measured quantisation cost.
	Quantisation *ml.QuantisationReport
	// The remaining fields populate Metadata.
	TrainingRows        int
	HoldoutRows         int
	TrainingWindowStart time.Time
	TrainingWindowEnd   time.Time
	FeatureNames        []string
	Hyperparameters     map[string]string
	Notes               string
}

// Register stores a model as a challenger and returns its metadata.
//
// A model is never registered directly as champion, even when the slot is
// empty. Promotion is a separate, audited call — the one place an operator
// approves a change to what the shelves are priced by — and collapsing the two
// would mean a training job could put a model into production by existing.
func (r *Registry) Register(reg Registration) (Metadata, error) {
	if err := reg.Slot.Validate(); err != nil {
		return Metadata{}, err
	}
	if len(reg.Body) == 0 {
		return Metadata{}, fmt.Errorf("%w: empty model body", ErrInvalid)
	}
	if reg.Metrics.Rows == 0 {
		return Metadata{}, fmt.Errorf("%w: a model registered without holdout metrics cannot be compared to anything", ErrInvalid)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	version, err := r.nextVersionLocked(reg.Slot)
	if err != nil {
		return Metadata{}, err
	}
	md := Metadata{
		ID:      canon.NewULID(),
		Tenant:  reg.Slot.Tenant,
		Store:   reg.Slot.Store,
		Purpose: reg.Slot.Purpose,
		Kind:    reg.Kind, KindName: reg.Kind.String(),
		Stage: StageChallenger, Version: version,
		CreatedAt:           time.Now().UTC(),
		TrainingRows:        reg.TrainingRows,
		HoldoutRows:         reg.HoldoutRows,
		TrainingWindowStart: reg.TrainingWindowStart,
		TrainingWindowEnd:   reg.TrainingWindowEnd,
		FeatureNames:        reg.FeatureNames,
		Hyperparameters:     reg.Hyperparameters,
		Metrics:             reg.Metrics,
		Quantisation:        reg.Quantisation,
		Bytes:               len(reg.Body),
		EdgeBytes:           len(reg.EdgeBody),
		Notes:               reg.Notes,
	}

	b := r.kv.NewBatch()
	blob, err := json.Marshal(md)
	if err != nil {
		return Metadata{}, err
	}
	b.Put(metaKey(md.ID), blob)
	b.Put(bodyKey(md.ID), reg.Body)
	if len(reg.EdgeBody) > 0 {
		b.Put(edgeKey(md.ID), reg.EdgeBody)
	}
	b.Put(indexKey(reg.Slot, version, md.ID), []byte(md.ID))
	b.Put([]byte(versionKey(reg.Slot)), []byte(fmt.Sprintf("%d", version)))
	if err := b.Write(); err != nil {
		return Metadata{}, err
	}
	return md, nil
}

func (r *Registry) nextVersionLocked(slot Slot) (int64, error) {
	raw, err := r.kv.Get([]byte(versionKey(slot)))
	if errors.Is(err, kvstore.ErrNotFound) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	var v int64
	if _, err := fmt.Sscanf(string(raw), "%d", &v); err != nil {
		return 0, fmt.Errorf("registry: corrupt version counter for %s: %w", slot.key(), err)
	}
	return v + 1, nil
}

// Get returns a model's metadata.
func (r *Registry) Get(id string) (Metadata, error) {
	raw, err := r.kv.Get([]byte(metaKey(id)))
	if errors.Is(err, kvstore.ErrNotFound) {
		return Metadata{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Metadata{}, err
	}
	var md Metadata
	if err := json.Unmarshal(raw, &md); err != nil {
		return Metadata{}, fmt.Errorf("registry: corrupt metadata for %s: %w", id, err)
	}
	return md, nil
}

// Body returns the serialised model.
func (r *Registry) Body(id string) ([]byte, error) {
	raw, err := r.kv.Get([]byte(bodyKey(id)))
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil, fmt.Errorf("%w: body for %s", ErrNotFound, id)
	}
	return raw, err
}

// EdgeBody returns the serialised int8 artefact, which is what the Store
// Gateway Unit downloads.
func (r *Registry) EdgeBody(id string) ([]byte, error) {
	raw, err := r.kv.Get([]byte(edgeKey(id)))
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil, fmt.Errorf("%w: edge artefact for %s", ErrNotFound, id)
	}
	return raw, err
}

// List returns the models in a slot, newest version first.
func (r *Registry) List(slot Slot, limit int) ([]Metadata, error) {
	if err := slot.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	prefix := []byte("mi\x00" + slot.key() + "\x00")
	it := r.kv.Scan(prefix)
	defer it.Close()
	ids := make([]string, 0, 16)
	for it.Next() {
		ids = append(ids, string(it.Value()))
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	out := make([]Metadata, 0, len(ids))
	for _, id := range ids {
		md, err := r.Get(id)
		if err != nil {
			// A body without metadata is a torn write from a crash between two
			// batches; skipping it beats failing the whole listing.
			continue
		}
		out = append(out, md)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Champion returns the model currently serving a slot.
func (r *Registry) Champion(slot Slot) (Metadata, error) {
	if err := slot.Validate(); err != nil {
		return Metadata{}, err
	}
	raw, err := r.kv.Get([]byte(championKey(slot)))
	if errors.Is(err, kvstore.ErrNotFound) {
		return Metadata{}, fmt.Errorf("%w: no champion for %s", ErrNotFound, slot.key())
	}
	if err != nil {
		return Metadata{}, err
	}
	return r.Get(string(raw))
}

// PromotionResult records what a promotion did.
type PromotionResult struct {
	Promoted Metadata  `json:"promoted"`
	Demoted  *Metadata `json:"demoted,omitempty"`
	// Comparison is the champion/challenger verdict, absent when the slot was
	// empty and there was nothing to compare against.
	Comparison *ml.ChampionChallenger `json:"comparison,omitempty"`
	// Forced is true when the caller overrode a negative verdict.
	Forced bool `json:"forced"`
}

// PromoteOptions tune a promotion.
type PromoteOptions struct {
	// Comparison is the champion/challenger verdict computed by the caller on a
	// shared holdout. The registry does not compute it itself because it does
	// not hold the evaluation data — but it does refuse a promotion whose
	// verdict says no, unless Force is set.
	Comparison *ml.ChampionChallenger
	// Force promotes against a negative verdict. It exists because an operator
	// sometimes has information the holdout does not — a known data outage in
	// the evaluation window, a regulatory change — and because the alternative
	// is that they edit the database by hand and nothing is recorded.
	Force bool
	// MaxQuantisationDeltaPct refuses to promote a model whose int8 artefact
	// lost more than this much accuracy. Zero disables the check.
	MaxQuantisationDeltaPct float64
}

// Promote makes a model the champion of its slot.
func (r *Registry) Promote(id string, opts PromoteOptions) (PromotionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	md, err := r.Get(id)
	if err != nil {
		return PromotionResult{}, err
	}
	slot := Slot{Tenant: md.Tenant, Store: md.Store, Purpose: md.Purpose}

	if opts.MaxQuantisationDeltaPct > 0 && md.Quantisation != nil &&
		md.Quantisation.MAEDeltaPct > opts.MaxQuantisationDeltaPct {
		return PromotionResult{}, fmt.Errorf("%w: quantising %s cost %.1f%% of its accuracy, above the %.1f%% limit",
			ErrPromotionRefused, id, md.Quantisation.MAEDeltaPct, opts.MaxQuantisationDeltaPct)
	}
	if opts.Comparison != nil && !opts.Comparison.Promote && !opts.Force {
		return PromotionResult{}, fmt.Errorf("%w: %s", ErrPromotionRefused, opts.Comparison.Rationale)
	}

	res := PromotionResult{Comparison: opts.Comparison, Forced: opts.Force}
	b := r.kv.NewBatch()

	if cur, err := r.kv.Get([]byte(championKey(slot))); err == nil {
		prevID := string(cur)
		if prevID == id {
			// Promoting the serving model is a no-op rather than an error: it
			// is what a retry of a partially-applied promotion looks like.
			res.Promoted = md
			return res, nil
		}
		if prev, err := r.Get(prevID); err == nil {
			now := time.Now().UTC()
			prev.Stage = StageArchived
			prev.ArchivedAt = &now
			blob, err := json.Marshal(prev)
			if err != nil {
				return PromotionResult{}, err
			}
			b.Put(metaKey(prev.ID), blob)
			res.Demoted = &prev
		}
	} else if !errors.Is(err, kvstore.ErrNotFound) {
		return PromotionResult{}, err
	}

	now := time.Now().UTC()
	md.Stage = StageChampion
	md.PromotedAt = &now
	md.Comparison = opts.Comparison
	blob, err := json.Marshal(md)
	if err != nil {
		return PromotionResult{}, err
	}
	b.Put(metaKey(md.ID), blob)
	b.Put([]byte(championKey(slot)), []byte(md.ID))
	if err := b.Write(); err != nil {
		return PromotionResult{}, err
	}
	res.Promoted = md
	return res, nil
}

// LoadChampionGBT loads and decodes the serving demand model for a slot.
//
// The feature-name check is the load-time guard against the silent failure that
// motivates storing them: a model trained on one vector layout and served
// against another returns plausible numbers computed from the wrong columns.
func (r *Registry) LoadChampionGBT(slot Slot, expectFeatures []string) (*ml.GBT, Metadata, error) {
	md, err := r.Champion(slot)
	if err != nil {
		return nil, Metadata{}, err
	}
	if md.Kind != ml.KindGBT {
		return nil, md, fmt.Errorf("%w: champion for %s is a %s", ErrInvalid, slot.key(), md.KindName)
	}
	if len(md.FeatureNames) > 0 && len(expectFeatures) > 0 {
		if strings.Join(md.FeatureNames, ",") != strings.Join(expectFeatures, ",") {
			return nil, md, fmt.Errorf("%w: model %s was trained on [%s] but the caller supplies [%s]",
				ErrInvalid, md.ID, strings.Join(md.FeatureNames, ","), strings.Join(expectFeatures, ","))
		}
	}
	body, err := r.Body(md.ID)
	if err != nil {
		return nil, md, err
	}
	var m ml.GBT
	if err := m.UnmarshalBinary(body); err != nil {
		return nil, md, err
	}
	return &m, md, nil
}

// LoadChampionForest loads and decodes the serving anomaly model.
func (r *Registry) LoadChampionForest(slot Slot) (*ml.IsolationForest, Metadata, error) {
	md, err := r.Champion(slot)
	if err != nil {
		return nil, Metadata{}, err
	}
	if md.Kind != ml.KindIsoForest {
		return nil, md, fmt.Errorf("%w: champion for %s is a %s", ErrInvalid, slot.key(), md.KindName)
	}
	body, err := r.Body(md.ID)
	if err != nil {
		return nil, md, err
	}
	var f ml.IsolationForest
	if err := f.UnmarshalBinary(body); err != nil {
		return nil, md, err
	}
	return &f, md, nil
}

// LoadChampionLSTM loads and decodes the serving forecast model.
func (r *Registry) LoadChampionLSTM(slot Slot) (*ml.LSTM, Metadata, error) {
	md, err := r.Champion(slot)
	if err != nil {
		return nil, Metadata{}, err
	}
	if md.Kind != ml.KindLSTM {
		return nil, md, fmt.Errorf("%w: champion for %s is a %s", ErrInvalid, slot.key(), md.KindName)
	}
	body, err := r.Body(md.ID)
	if err != nil {
		return nil, md, err
	}
	var n ml.LSTM
	if err := n.UnmarshalBinary(body); err != nil {
		return nil, md, err
	}
	return &n, md, nil
}

// Key layout. The prefixes are two bytes plus a NUL so that no prefix is a
// prefix of another, which is what keeps a Scan over one namespace from walking
// into the next.
func metaKey(id string) []byte  { return []byte("mm\x00" + id) }
func bodyKey(id string) []byte  { return []byte("mb\x00" + id) }
func edgeKey(id string) []byte  { return []byte("me\x00" + id) }
func championKey(s Slot) string { return "mc\x00" + s.key() }
func versionKey(s Slot) string  { return "mv\x00" + s.key() }

// indexKey orders a slot's models by version. The version is rendered
// zero-padded so that lexical order is numeric order — without the padding,
// version 10 sorts before version 9 and the "newest model" query is wrong from
// the tenth training run onwards.
func indexKey(s Slot, version int64, id string) []byte {
	return []byte(fmt.Sprintf("mi\x00%s\x00%012d\x00%s", s.key(), version, id))
}
