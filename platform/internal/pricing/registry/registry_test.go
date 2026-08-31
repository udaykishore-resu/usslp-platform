package registry

import (
	"errors"
	"strings"
	"testing"

	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	kv, err := kvstore.OpenWith(kvstore.Options{Dir: t.TempDir(), Sync: kvstore.SyncNever})
	if err != nil {
		t.Fatalf("open kv: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })
	reg, err := New(kv)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return reg
}

func testSlot() Slot {
	return Slot{Tenant: "acme", Store: "store-001", Purpose: PurposeDemand}
}

// trainModel fits a small ensemble so the registry has real bytes to store,
// rather than a placeholder that would let a serialisation bug through.
func trainModel(t *testing.T, seed uint64) (*ml.GBT, ml.Metrics) {
	t.Helper()
	rng := seed
	next := func() float64 {
		rng += 0x9E3779B97F4A7C15
		z := rng
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		return float64(z>>11) / float64(1<<53)
	}
	X := make([][]float64, 400)
	y := make([]float64, 400)
	for i := range X {
		X[i] = []float64{next(), next(), next()}
		y[i] = 2*X[i][0] + 3*X[i][1]*X[i][2]
	}
	m, err := ml.TrainGBT(X, y, ml.GBTParams{Rounds: 40, MaxDepth: 3, Seed: seed})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	metrics, err := ml.Evaluate(m, X, y)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return m, metrics
}

func register(t *testing.T, reg *Registry, slot Slot, m *ml.GBT, metrics ml.Metrics) Metadata {
	t.Helper()
	body, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	q, err := ml.QuantiseGBT(m)
	if err != nil {
		t.Fatalf("quantise: %v", err)
	}
	edge, err := q.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal edge: %v", err)
	}
	md, err := reg.Register(Registration{
		Slot: slot, Kind: ml.KindGBT, Body: body, EdgeBody: edge,
		Metrics: metrics, TrainingRows: 320, HoldoutRows: 80,
		FeatureNames: []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return md
}

func TestRegisterAssignsVersionsAndKeepsBodies(t *testing.T) {
	reg := newTestRegistry(t)
	slot := testSlot()

	m1, mx1 := trainModel(t, 1)
	first := register(t, reg, slot, m1, mx1)
	if first.Version != 1 {
		t.Errorf("first version = %d, want 1", first.Version)
	}
	if first.Stage != StageChallenger {
		t.Errorf("stage = %q, want challenger: nothing serves without a promotion", first.Stage)
	}
	if first.Bytes == 0 || first.EdgeBytes == 0 {
		t.Errorf("bytes = %d/%d, want both recorded", first.Bytes, first.EdgeBytes)
	}

	m2, mx2 := trainModel(t, 2)
	second := register(t, reg, slot, m2, mx2)
	if second.Version != 2 {
		t.Errorf("second version = %d, want 2", second.Version)
	}

	// The stored body must decode to a model that predicts identically.
	body, err := reg.Body(first.ID)
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	var back ml.GBT
	if err := back.UnmarshalBinary(body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	probe := []float64{0.3, 0.6, 0.9}
	if a, b := m1.Predict(probe), back.Predict(probe); a != b {
		t.Errorf("the stored model predicts %v where the trained one predicts %v", b, a)
	}
	// And the edge artefact must be there and decode as the int8 kind.
	edge, err := reg.EdgeBody(first.ID)
	if err != nil {
		t.Fatalf("edge body: %v", err)
	}
	var q ml.QuantisedGBT
	if err := q.UnmarshalBinary(edge); err != nil {
		t.Fatalf("unmarshal edge: %v", err)
	}

	listed, err := reg.List(slot, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d models, want 2", len(listed))
	}
	if listed[0].Version != 2 {
		t.Errorf("the listing is not newest-first: %+v", listed)
	}
	if listed[0].ID != second.ID {
		t.Errorf("the newest listed model is %s, want %s", listed[0].ID, second.ID)
	}
	if a, b := m2.Predict(probe), m1.Predict(probe); a == b {
		t.Error("the two seeds produced identical models, so the test proves nothing about isolation")
	}
}

// TestVersionOrderingSurvivesTenModels is the regression test for a lexical
// index: without zero-padding, version 10 sorts before version 9 and "the
// newest model" is wrong from the tenth training run onwards.
func TestVersionOrderingSurvivesTenModels(t *testing.T) {
	reg := newTestRegistry(t)
	slot := testSlot()
	m, metrics := trainModel(t, 7)
	for i := 0; i < 12; i++ {
		register(t, reg, slot, m, metrics)
	}
	listed, err := reg.List(slot, 20)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 12 {
		t.Fatalf("listed %d models, want 12", len(listed))
	}
	if listed[0].Version != 12 {
		t.Errorf("the newest model is version %d, want 12", listed[0].Version)
	}
	for i := 1; i < len(listed); i++ {
		if listed[i].Version >= listed[i-1].Version {
			t.Fatalf("versions are not descending at %d: %d then %d",
				i, listed[i-1].Version, listed[i].Version)
		}
	}
}

func TestRegisterRefusesModelsWithNoEvidence(t *testing.T) {
	reg := newTestRegistry(t)
	m, _ := trainModel(t, 3)
	body, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Run("no holdout metrics", func(t *testing.T) {
		_, err := reg.Register(Registration{Slot: testSlot(), Kind: ml.KindGBT, Body: body})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("err = %v, want a refusal: a model with no metrics cannot be compared to anything", err)
		}
	})
	t.Run("no body", func(t *testing.T) {
		_, err := reg.Register(Registration{Slot: testSlot(), Kind: ml.KindGBT,
			Metrics: ml.Metrics{MAE: 1, Rows: 100}})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("err = %v, want a refusal", err)
		}
	})
	t.Run("bad slot", func(t *testing.T) {
		_, err := reg.Register(Registration{
			Slot: Slot{Tenant: "acme", Purpose: "nonsense"}, Kind: ml.KindGBT, Body: body,
			Metrics: ml.Metrics{MAE: 1, Rows: 100}})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("err = %v, want a refusal", err)
		}
	})
	t.Run("tenant with reserved characters", func(t *testing.T) {
		_, err := reg.Register(Registration{
			Slot: Slot{Tenant: "ac/me", Store: "s", Purpose: PurposeDemand}, Kind: ml.KindGBT, Body: body,
			Metrics: ml.Metrics{MAE: 1, Rows: 100}})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("err = %v, want a refusal", err)
		}
	})
}

func TestPromotionMakesAChampionAndArchivesTheOld(t *testing.T) {
	reg := newTestRegistry(t)
	slot := testSlot()
	m, metrics := trainModel(t, 11)
	first := register(t, reg, slot, m, metrics)
	second := register(t, reg, slot, m, metrics)

	if _, err := reg.Champion(slot); !errors.Is(err, ErrNotFound) {
		t.Errorf("a slot with two challengers has a champion: %v", err)
	}

	res, err := reg.Promote(first.ID, PromoteOptions{})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if res.Promoted.Stage != StageChampion || res.Promoted.PromotedAt == nil {
		t.Errorf("promoted model = %+v", res.Promoted)
	}
	if res.Demoted != nil {
		t.Errorf("something was demoted from an empty slot: %+v", res.Demoted)
	}

	// Promoting the serving model again is a no-op, because that is what a
	// retried promotion looks like.
	again, err := reg.Promote(first.ID, PromoteOptions{})
	if err != nil {
		t.Fatalf("re-promote: %v", err)
	}
	if again.Demoted != nil {
		t.Errorf("re-promoting the champion demoted something: %+v", again.Demoted)
	}

	res, err = reg.Promote(second.ID, PromoteOptions{})
	if err != nil {
		t.Fatalf("promote second: %v", err)
	}
	if res.Demoted == nil || res.Demoted.ID != first.ID {
		t.Fatalf("the previous champion was not demoted: %+v", res.Demoted)
	}
	if res.Demoted.Stage != StageArchived || res.Demoted.ArchivedAt == nil {
		t.Errorf("the demoted model is %q, want archived and retained", res.Demoted.Stage)
	}
	champ, err := reg.Champion(slot)
	if err != nil {
		t.Fatalf("champion: %v", err)
	}
	if champ.ID != second.ID {
		t.Errorf("champion = %s, want %s", champ.ID, second.ID)
	}
	// The archived model is retained, not deleted: "which model set this price"
	// must stay answerable.
	if _, err := reg.Get(first.ID); err != nil {
		t.Errorf("the archived model is gone: %v", err)
	}
	if _, err := reg.Body(first.ID); err != nil {
		t.Errorf("the archived model's body is gone: %v", err)
	}
}

func TestPromotionRespectsTheChampionChallengerVerdict(t *testing.T) {
	reg := newTestRegistry(t)
	slot := testSlot()
	m, metrics := trainModel(t, 13)
	md := register(t, reg, slot, m, metrics)

	negative := &ml.ChampionChallenger{Promote: false, Rationale: "challenger is 4% worse"}

	t.Run("a negative verdict blocks the promotion", func(t *testing.T) {
		_, err := reg.Promote(md.ID, PromoteOptions{Comparison: negative})
		if !errors.Is(err, ErrPromotionRefused) {
			t.Fatalf("err = %v, want ErrPromotionRefused", err)
		}
		if !strings.Contains(err.Error(), "4% worse") {
			t.Errorf("the refusal does not carry the rationale: %v", err)
		}
	})

	t.Run("force overrides it and is recorded", func(t *testing.T) {
		res, err := reg.Promote(md.ID, PromoteOptions{Comparison: negative, Force: true})
		if err != nil {
			t.Fatalf("forced promote: %v", err)
		}
		if !res.Forced {
			t.Error("the promotion does not record that it was forced")
		}
		stored, err := reg.Get(md.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Comparison == nil || stored.Comparison.Rationale != negative.Rationale {
			t.Errorf("the verdict was not retained on the promoted model: %+v", stored.Comparison)
		}
	})
}

func TestPromotionRefusesAnExpensiveQuantisation(t *testing.T) {
	reg := newTestRegistry(t)
	slot := testSlot()
	m, metrics := trainModel(t, 17)
	body, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bad := ml.QuantisationReport{FloatMAE: 1.0, Int8MAE: 1.5, MAEDelta: 0.5, MAEDeltaPct: 50}
	md, err := reg.Register(Registration{
		Slot: slot, Kind: ml.KindGBT, Body: body, Metrics: metrics, Quantisation: &bad,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err = reg.Promote(md.ID, PromoteOptions{MaxQuantisationDeltaPct: 10})
	if !errors.Is(err, ErrPromotionRefused) {
		t.Fatalf("err = %v, want a refusal: the edge artefact lost half its accuracy", err)
	}
	// The same model promotes fine when the check is disabled, which is what a
	// cloud-only deployment with no edge artefact wants.
	if _, err := reg.Promote(md.ID, PromoteOptions{}); err != nil {
		t.Errorf("promote with the check disabled: %v", err)
	}
}

func TestLoadChampionRefusesAFeatureMismatch(t *testing.T) {
	reg := newTestRegistry(t)
	slot := testSlot()
	m, metrics := trainModel(t, 19)
	md := register(t, reg, slot, m, metrics)
	if _, err := reg.Promote(md.ID, PromoteOptions{}); err != nil {
		t.Fatalf("promote: %v", err)
	}

	if _, _, err := reg.LoadChampionGBT(slot, []string{"a", "b", "c"}); err != nil {
		t.Fatalf("loading with the right feature names failed: %v", err)
	}
	// A model trained on one vector layout and served against another returns
	// plausible numbers computed from the wrong columns, and this is the only
	// place that can be caught.
	_, _, err := reg.LoadChampionGBT(slot, []string{"c", "b", "a"})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want a refusal on a reordered feature vector", err)
	}
	if err != nil && !strings.Contains(err.Error(), "trained on") {
		t.Errorf("the refusal does not say what the mismatch is: %v", err)
	}
}

func TestLoadChampionRefusesTheWrongKind(t *testing.T) {
	reg := newTestRegistry(t)
	slot := Slot{Tenant: "acme", Store: "store-001", Purpose: PurposeAnomaly}
	forest, err := ml.TrainIsolationForest(fleetSample(), ml.IsoForestParams{Trees: 20})
	if err != nil {
		t.Fatalf("train forest: %v", err)
	}
	body, err := forest.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	md, err := reg.Register(Registration{
		Slot: slot, Kind: ml.KindIsoForest, Body: body,
		Metrics: ml.Metrics{MAE: 0, Rows: 100},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := reg.Promote(md.ID, PromoteOptions{}); err != nil {
		t.Fatalf("promote: %v", err)
	}

	if _, _, err := reg.LoadChampionForest(slot); err != nil {
		t.Errorf("loading the forest failed: %v", err)
	}
	if _, _, err := reg.LoadChampionGBT(slot, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want a refusal: the champion is a forest, not an ensemble", err)
	}
}

func fleetSample() [][]float64 {
	rng := uint64(4242)
	next := func() float64 {
		rng += 0x9E3779B97F4A7C15
		z := rng
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		return float64(z>>11) / float64(1<<53)
	}
	rows := make([][]float64, 300)
	for i := range rows {
		rows[i] = []float64{3000 + 100*next(), 20 + 4*next(), 3 + next(), 180 + 20*next()}
	}
	return rows
}

func TestSlotsAreIsolated(t *testing.T) {
	reg := newTestRegistry(t)
	m, metrics := trainModel(t, 23)

	slots := []Slot{
		{Tenant: "acme", Store: "s1", Purpose: PurposeDemand},
		{Tenant: "acme", Store: "s2", Purpose: PurposeDemand},
		{Tenant: "rival", Store: "s1", Purpose: PurposeDemand},
		{Tenant: "acme", Store: "s1", Purpose: PurposeForecast},
		// An empty store is a tenant-wide model, which is what a chain with too
		// little per-store history falls back to.
		{Tenant: "acme", Store: "", Purpose: PurposeDemand},
	}
	for _, slot := range slots {
		md := register(t, reg, slot, m, metrics)
		if _, err := reg.Promote(md.ID, PromoteOptions{}); err != nil {
			t.Fatalf("promote %v: %v", slot, err)
		}
	}
	for _, slot := range slots {
		listed, err := reg.List(slot, 10)
		if err != nil {
			t.Fatalf("list %v: %v", slot, err)
		}
		if len(listed) != 1 {
			t.Errorf("slot %v lists %d models, want 1 — the key layout is not isolating slots",
				slot, len(listed))
		}
		champ, err := reg.Champion(slot)
		if err != nil {
			t.Fatalf("champion %v: %v", slot, err)
		}
		if champ.Tenant != slot.Tenant || champ.Store != slot.Store || champ.Purpose != slot.Purpose {
			t.Errorf("slot %v returned a champion from %v", slot, champ)
		}
	}
}

func TestGetAndBodyReturnNotFoundForUnknownIDs(t *testing.T) {
	reg := newTestRegistry(t)
	if _, err := reg.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get err = %v, want ErrNotFound", err)
	}
	if _, err := reg.Body("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Body err = %v, want ErrNotFound", err)
	}
	if _, err := reg.EdgeBody("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("EdgeBody err = %v, want ErrNotFound", err)
	}
	if _, err := reg.Champion(testSlot()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Champion err = %v, want ErrNotFound", err)
	}
}
