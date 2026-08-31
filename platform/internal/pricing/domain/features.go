package domain

import (
	"fmt"
	"time"
)

// FeatureIndex names the position of each feature in the model input vector.
//
// The order is a contract between the training harness, the serialised model
// and the SGU's inference loop. A model trained with one ordering and served
// with another produces confident nonsense — one of the few ML failures that
// never raises an error — so the ordering is a named constant rather than a
// convention, and the serialised model records the count so a mismatch is
// caught at load time.
type FeatureIndex int

// The feature vector. Every entry is a float64 even where the underlying value
// is categorical: the gradient-boosted trees split on thresholds, so an ordinal
// encoding of a genuinely ordinal variable (hour, day, season) is both correct
// and cheaper than one-hot, while the two truly nominal variables (weather
// bucket, local event) are low-cardinality enough that the trees recover the
// partition from threshold splits.
const (
	// FeatPrice is the candidate price in minor units. It is a feature because
	// the demand model must be *causal in price* for the Tier-2 optimiser to
	// mean anything: an optimiser that searches prices against a model with no
	// price input is searching a constant.
	FeatPrice FeatureIndex = iota
	// FeatHourOfDay is 0-23 in the store's local time.
	FeatHourOfDay
	// FeatDayOfWeek is 0 (Sunday) to 6.
	FeatDayOfWeek
	// FeatDaysToExpiry is the shelf life remaining. Negative is past date.
	FeatDaysToExpiry
	// FeatSeason is 0-3 (winter, spring, summer, autumn) for the store's
	// hemisphere, resolved by the caller.
	FeatSeason
	// FeatInventoryLevel is units on hand.
	FeatInventoryLevel
	// FeatDaysOfSupply is inventory divided by recent velocity.
	FeatDaysOfSupply
	// FeatWasteRate is the trailing fraction of units written off.
	FeatWasteRate
	// FeatCompetitorPrice is the tracked competitor price in minor units.
	FeatCompetitorPrice
	// FeatVelocity7, 14 and 30 are units per day over trailing windows.
	FeatVelocity7
	FeatVelocity14
	FeatVelocity30
	// FeatElasticity is the estimated own-price elasticity. Feeding the
	// elasticity estimate to the tree model as a feature lets the trees learn
	// *where* the elasticity estimate is trustworthy rather than assuming it
	// everywhere.
	FeatElasticity
	// FeatWeatherBucket is a coarse local weather class.
	FeatWeatherBucket
	// FeatLocalEvent is 1 when a local event (match, festival, holiday) is in
	// progress near the store.
	FeatLocalEvent

	// NumFeatures is the vector width. Keep it last.
	NumFeatures
)

// FeatureNames are the stable names used by the feature store, the model
// metadata and the API.
var FeatureNames = [NumFeatures]string{
	FeatPrice:           "price_minor",
	FeatHourOfDay:       "hour_of_day",
	FeatDayOfWeek:       "day_of_week",
	FeatDaysToExpiry:    "days_to_expiry",
	FeatSeason:          "season",
	FeatInventoryLevel:  "inventory_level",
	FeatDaysOfSupply:    "days_of_supply",
	FeatWasteRate:       "waste_rate",
	FeatCompetitorPrice: "competitor_price_minor",
	FeatVelocity7:       "velocity_7d",
	FeatVelocity14:      "velocity_14d",
	FeatVelocity30:      "velocity_30d",
	FeatElasticity:      "elasticity",
	FeatWeatherBucket:   "weather_bucket",
	FeatLocalEvent:      "local_event",
}

// FeatureIndexByName resolves a feature name to its position, for the feature
// store and for model-metadata validation.
func FeatureIndexByName(name string) (FeatureIndex, error) {
	for i, n := range FeatureNames {
		if n == name {
			return FeatureIndex(i), nil
		}
	}
	return 0, fmt.Errorf("pricing: unknown feature %q", name)
}

// Features is the typed form of one model input row.
//
// The struct exists alongside the raw vector because callers assemble features
// from a dozen sources and positional construction of a fifteen-wide float
// slice is a bug waiting to happen; Vector converts once, at the boundary.
type Features struct {
	PriceMinor      float64 `json:"price_minor"`
	HourOfDay       float64 `json:"hour_of_day"`
	DayOfWeek       float64 `json:"day_of_week"`
	DaysToExpiry    float64 `json:"days_to_expiry"`
	Season          float64 `json:"season"`
	InventoryLevel  float64 `json:"inventory_level"`
	DaysOfSupply    float64 `json:"days_of_supply"`
	WasteRate       float64 `json:"waste_rate"`
	CompetitorPrice float64 `json:"competitor_price_minor"`
	Velocity7       float64 `json:"velocity_7d"`
	Velocity14      float64 `json:"velocity_14d"`
	Velocity30      float64 `json:"velocity_30d"`
	Elasticity      float64 `json:"elasticity"`
	WeatherBucket   float64 `json:"weather_bucket"`
	LocalEvent      bool    `json:"local_event"`
}

// Vector renders the features in model order.
func (f Features) Vector() []float64 {
	v := make([]float64, NumFeatures)
	f.FillVector(v)
	return v
}

// FillVector writes the features into a caller-owned buffer.
//
// The optimiser evaluates dozens of candidate prices per recommendation and the
// Tier-2 budget is 15 milliseconds; reusing one buffer across those candidates
// removes the only per-candidate allocation in the loop.
func (f Features) FillVector(v []float64) {
	if len(v) < int(NumFeatures) {
		panic("pricing: feature buffer too small")
	}
	v[FeatPrice] = f.PriceMinor
	v[FeatHourOfDay] = f.HourOfDay
	v[FeatDayOfWeek] = f.DayOfWeek
	v[FeatDaysToExpiry] = f.DaysToExpiry
	v[FeatSeason] = f.Season
	v[FeatInventoryLevel] = f.InventoryLevel
	v[FeatDaysOfSupply] = f.DaysOfSupply
	v[FeatWasteRate] = f.WasteRate
	v[FeatCompetitorPrice] = f.CompetitorPrice
	v[FeatVelocity7] = f.Velocity7
	v[FeatVelocity14] = f.Velocity14
	v[FeatVelocity30] = f.Velocity30
	v[FeatElasticity] = f.Elasticity
	v[FeatWeatherBucket] = f.WeatherBucket
	v[FeatLocalEvent] = boolFeature(f.LocalEvent)
}

func boolFeature(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// WithCalendar fills the time-derived features from an instant already
// converted to the store's local time.
//
// It takes a local time rather than a location because the caller has already
// had to resolve the store's zone to answer other questions, and doing the
// lookup twice is both slower and a chance for the two answers to disagree.
func (f Features) WithCalendar(local time.Time, southernHemisphere bool) Features {
	f.HourOfDay = float64(local.Hour())
	f.DayOfWeek = float64(int(local.Weekday()))
	f.Season = float64(Season(local, southernHemisphere))
	return f
}

// Season maps a local date to a meteorological season index, 0 = winter.
//
// Meteorological rather than astronomical seasons, because retail demand
// follows whole months (the barbecue season starts when the calendar says June,
// not when the solstice does) and month boundaries make the feature stable.
func Season(local time.Time, southernHemisphere bool) int {
	m := int(local.Month())
	idx := ((m % 12) / 3) // Dec,Jan,Feb -> 0; Mar,Apr,May -> 1; ...
	if southernHemisphere {
		idx = (idx + 2) % 4
	}
	return idx
}
