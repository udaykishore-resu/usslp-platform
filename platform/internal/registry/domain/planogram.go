package domain

import (
	"fmt"
	"sort"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// PositionKey is a physical slot on a shelf edge: which shelf unit, which rail
// within it, and which slot along the rail counting from the left.
//
// It is the coordinate a merchandiser thinks in and the coordinate a planogram
// file is keyed by. It is deliberately *not* the label identifier: a label is a
// piece of hardware clipped onto a rail, and the whole point of a planogram
// upload is that hardware moves between slots while the slots stay put.
type PositionKey struct {
	Shelf    string `json:"shelf"`
	Rail     string `json:"rail"`
	Position int    `json:"position"`
}

// String renders the coordinate for logs and diff output.
func (k PositionKey) String() string {
	return fmt.Sprintf("%s/%s/%d", k.Shelf, k.Rail, k.Position)
}

// Less orders coordinates so that a diff, a listing and a seeded store are all
// emitted in the same stable order. Determinism here is what makes the
// planogram endpoints diffable by an operator and the seed endpoint
// reproducible.
func (k PositionKey) Less(o PositionKey) bool {
	if k.Shelf != o.Shelf {
		return k.Shelf < o.Shelf
	}
	if k.Rail != o.Rail {
		return k.Rail < o.Rail
	}
	return k.Position < o.Position
}

// Position binds one label to one SKU at one coordinate.
type Position struct {
	PositionKey
	// LabelID is the label clipped into this slot.
	LabelID canon.LabelID `json:"label_id"`
	// SKU is the product faced at this slot.
	SKU canon.SKU `json:"sku"`
	// Facings is how many identical facings this one label prices. A facing
	// count above one does not mean more labels; it means the slot is wider and
	// the SKU occupies more of the rail, which the Label Service uses when it
	// decides between the standard and the wide render template.
	Facings int `json:"facings"`
	// Template names the display template, matching canon.RenderSpec.Template:
	// "standard", "promo", "unit_price", "clearance".
	Template string `json:"display_template"`
	// SECID is the controller whose zone covers this slot. A planogram upload is
	// allowed to move a label between controllers — a shelf reset does exactly
	// that — which is the case that leaves a stale retained MQTT message behind
	// and that the registry must clear.
	SECID canon.SECID `json:"sec_id"`
	// Zone is the controller's own subdivision, used for zone-wide promotion
	// broadcasts.
	Zone string `json:"zone,omitempty"`
}

// Assignment is the planogram binding as it is denormalised onto a device, so
// that a single device lookup answers "what is this label showing and why"
// without loading the store's whole planogram.
type Assignment struct {
	PositionKey
	SKU      canon.SKU `json:"sku"`
	Facings  int       `json:"facings"`
	Template string    `json:"display_template"`
	// Sequence is monotonic per label. It is copied into every LabelAssigned
	// event so that a consumer receiving assignments out of order can discard
	// the stale one.
	Sequence   int64     `json:"sequence"`
	AssignedAt time.Time `json:"assigned_at"`
}

// Planogram is one store's complete shelf layout at one revision.
type Planogram struct {
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id"`
	// Revision increments on every accepted upload. It is what an operator
	// quotes when asking "which layout was live when this price printed".
	Revision  int64      `json:"revision"`
	Positions []Position `json:"positions"`
	UpdatedAt time.Time  `json:"updated_at"`
	// Source names the system that uploaded it, e.g. "spaceman", "blue-yonder".
	Source string `json:"source,omitempty"`
}

// Validate rejects a planogram that could not be applied consistently.
//
// The two uniqueness rules are the ones that matter. A coordinate appearing
// twice means the file disagrees with itself about what is on the shelf. A
// label appearing twice means one piece of hardware is being asked to display
// two prices, which — because the Label Service fans out per assignment event —
// would produce a label that flickers between two SKUs forever.
func (p *Planogram) Validate() error {
	if !canon.ValidID(string(p.TenantID)) {
		return fmt.Errorf("%w: planogram tenant id %q", ErrInvalid, p.TenantID)
	}
	if !canon.ValidID(string(p.StoreID)) {
		return fmt.Errorf("%w: planogram store id %q", ErrInvalid, p.StoreID)
	}
	seenKey := make(map[PositionKey]struct{}, len(p.Positions))
	seenLabel := make(map[canon.LabelID]struct{}, len(p.Positions))
	for i := range p.Positions {
		pos := &p.Positions[i]
		switch {
		case pos.Shelf == "":
			return fmt.Errorf("%w: position %d has no shelf", ErrInvalid, i)
		case pos.Rail == "":
			return fmt.Errorf("%w: position %d has no rail", ErrInvalid, i)
		case pos.Position <= 0:
			return fmt.Errorf("%w: position %d has slot %d, slots are numbered from 1",
				ErrInvalid, i, pos.Position)
		case !canon.ValidID(string(pos.LabelID)):
			return fmt.Errorf("%w: position %s has label id %q", ErrInvalid, pos.PositionKey, pos.LabelID)
		case pos.SKU == "":
			return fmt.Errorf("%w: position %s has no sku", ErrInvalid, pos.PositionKey)
		case !canon.ValidID(string(pos.SECID)):
			return fmt.Errorf("%w: position %s has sec id %q", ErrInvalid, pos.PositionKey, pos.SECID)
		}
		if pos.Facings <= 0 {
			pos.Facings = 1
		}
		if pos.Template == "" {
			pos.Template = "standard"
		}
		if _, dup := seenKey[pos.PositionKey]; dup {
			return fmt.Errorf("%w: coordinate %s appears twice", ErrInvalid, pos.PositionKey)
		}
		if _, dup := seenLabel[pos.LabelID]; dup {
			return fmt.Errorf("%w: label %s is bound to two coordinates", ErrInvalid, pos.LabelID)
		}
		seenKey[pos.PositionKey] = struct{}{}
		seenLabel[pos.LabelID] = struct{}{}
	}
	return nil
}

// Sort orders the positions by coordinate in place, so that two uploads of the
// same layout in different line orders produce byte-identical stored state and
// an empty diff.
func (p *Planogram) Sort() {
	sort.SliceStable(p.Positions, func(i, j int) bool {
		return p.Positions[i].PositionKey.Less(p.Positions[j].PositionKey)
	})
}

// ByLabel indexes the positions by the label clipped into them.
func (p *Planogram) ByLabel() map[canon.LabelID]Position {
	out := make(map[canon.LabelID]Position, len(p.Positions))
	for _, pos := range p.Positions {
		out[pos.LabelID] = pos
	}
	return out
}

// ByCoordinate indexes the positions by shelf coordinate.
func (p *Planogram) ByCoordinate() map[PositionKey]Position {
	out := make(map[PositionKey]Position, len(p.Positions))
	for _, pos := range p.Positions {
		out[pos.PositionKey] = pos
	}
	return out
}

// ChangeKind classifies one line of a planogram diff.
type ChangeKind string

// The four kinds of planogram change.
const (
	// ChangeAdded is a label that the new planogram binds and the old one did
	// not. It is hardware entering service on the shelf.
	ChangeAdded ChangeKind = "added"
	// ChangeMoved is a label that exists in both revisions at different
	// coordinates. It is the case that can move a label between controllers and
	// therefore the case that leaves a stale retained MQTT message behind.
	ChangeMoved ChangeKind = "moved"
	// ChangeChanged is a label at the same coordinate whose SKU, facing count,
	// template, controller or zone changed. A price change is not a planogram
	// change and never appears here.
	ChangeChanged ChangeKind = "changed"
	// ChangeRemoved is a coordinate the new planogram no longer declares — a
	// rail taken out, a bay re-bricked.
	ChangeRemoved ChangeKind = "removed"
)

// Change is one line of a planogram diff. From and To are both present for a
// move or an amendment, only To for an addition, only From for a removal.
type Change struct {
	Kind    ChangeKind    `json:"kind"`
	LabelID canon.LabelID `json:"label_id,omitempty"`
	From    *Position     `json:"from,omitempty"`
	To      *Position     `json:"to,omitempty"`
	// Detail names the fields that differ, for an amendment.
	Detail string `json:"detail,omitempty"`
}

// Diff is the result of comparing two planogram revisions.
//
// Orphaned deserves its own field rather than being folded into Removed because
// the two mean different things operationally, and conflating them is how a
// shelf reset ends with forty labels displaying last week's prices. Removed is
// a *coordinate* the new layout no longer declares — often because the shelving
// was re-bricked and the same labels now live at new coordinates. Orphaned is a
// *label* the new layout does not mention at all: it is physically clipped to a
// rail somewhere in the building, it still has a battery and a radio, and until
// the registry unassigns it the Label Service will keep repricing it for a SKU
// that is no longer there.
type Diff struct {
	Added   []Change `json:"added"`
	Moved   []Change `json:"moved"`
	Changed []Change `json:"changed"`
	Removed []Change `json:"removed"`
	// Orphaned lists the labels the new revision does not bind at all.
	Orphaned []canon.LabelID `json:"orphaned"`
}

// Empty reports whether the two revisions describe the same shelf.
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Moved) == 0 && len(d.Changed) == 0 &&
		len(d.Removed) == 0 && len(d.Orphaned) == 0
}

// Total returns the number of changed lines, which is the number an operator
// sees before confirming an upload.
func (d Diff) Total() int {
	return len(d.Added) + len(d.Moved) + len(d.Changed) + len(d.Removed)
}

// DiffPlanograms compares two revisions of a store's layout.
//
// A nil old planogram means "the store had no layout", so every position is an
// addition — which is exactly what the first upload for a new store should
// report rather than an error.
//
// Results are emitted in coordinate order so that two runs over the same inputs
// produce identical output; an operator diffing two upload reports must see
// only real changes.
func DiffPlanograms(old, next *Planogram) Diff {
	var d Diff
	d.Added = []Change{}
	d.Moved = []Change{}
	d.Changed = []Change{}
	d.Removed = []Change{}
	d.Orphaned = []canon.LabelID{}
	if next == nil {
		next = &Planogram{}
	}
	if old == nil {
		old = &Planogram{}
	}

	oldByLabel := old.ByLabel()
	newByLabel := next.ByLabel()
	oldByKey := old.ByCoordinate()
	newByKey := next.ByCoordinate()

	// Walk the new revision in coordinate order: additions, moves and
	// amendments are all decided from the label's previous binding.
	nextSorted := append([]Position(nil), next.Positions...)
	sort.SliceStable(nextSorted, func(i, j int) bool {
		return nextSorted[i].PositionKey.Less(nextSorted[j].PositionKey)
	})
	for i := range nextSorted {
		to := nextSorted[i]
		from, existed := oldByLabel[to.LabelID]
		if !existed {
			toCopy := to
			d.Added = append(d.Added, Change{Kind: ChangeAdded, LabelID: to.LabelID, To: &toCopy})
			continue
		}
		fromCopy, toCopy := from, to
		if from.PositionKey != to.PositionKey {
			d.Moved = append(d.Moved, Change{
				Kind: ChangeMoved, LabelID: to.LabelID, From: &fromCopy, To: &toCopy,
				Detail: describeAmendment(from, to),
			})
			continue
		}
		if detail := describeAmendment(from, to); detail != "" {
			d.Changed = append(d.Changed, Change{
				Kind: ChangeChanged, LabelID: to.LabelID, From: &fromCopy, To: &toCopy, Detail: detail,
			})
		}
	}

	// Coordinates the new revision no longer declares.
	oldKeys := make([]PositionKey, 0, len(oldByKey))
	for k := range oldByKey {
		oldKeys = append(oldKeys, k)
	}
	sort.Slice(oldKeys, func(i, j int) bool { return oldKeys[i].Less(oldKeys[j]) })
	for _, k := range oldKeys {
		if _, still := newByKey[k]; still {
			continue
		}
		from := oldByKey[k]
		fromCopy := from
		d.Removed = append(d.Removed, Change{Kind: ChangeRemoved, LabelID: from.LabelID, From: &fromCopy})
	}

	// Labels the new revision does not bind anywhere.
	orphans := make([]canon.LabelID, 0)
	for _, k := range oldKeys {
		label := oldByKey[k].LabelID
		if _, still := newByLabel[label]; !still {
			orphans = append(orphans, label)
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i] < orphans[j] })
	d.Orphaned = orphans
	return d
}

// describeAmendment returns a human-readable list of the fields that differ
// between two bindings of the same label, or "" when only the coordinate moved.
// It is what an operator reads in the upload report, so it names the fields in
// the order they matter: what is displayed first, where it is routed second.
func describeAmendment(from, to Position) string {
	var parts []string
	if from.SKU != to.SKU {
		parts = append(parts, fmt.Sprintf("sku %s→%s", from.SKU, to.SKU))
	}
	if from.Facings != to.Facings {
		parts = append(parts, fmt.Sprintf("facings %d→%d", from.Facings, to.Facings))
	}
	if from.Template != to.Template {
		parts = append(parts, fmt.Sprintf("template %s→%s", from.Template, to.Template))
	}
	if from.SECID != to.SECID {
		parts = append(parts, fmt.Sprintf("sec %s→%s", from.SECID, to.SECID))
	}
	if from.Zone != to.Zone {
		parts = append(parts, fmt.Sprintf("zone %s→%s", from.Zone, to.Zone))
	}
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}
