package domain

import (
	"sort"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// DefaultWeakLQI is the link quality below which a Zigbee hop is treated as
// weak.
//
// LQI is an 8-bit value; the 802.15.4 radios used in the labels report roughly
// 255 for a metre of clear air and fall off fast through steel shelving and
// chilled cabinet glass. Below about 80 the link still works but the retry rate
// climbs enough that the per-hop budget stops holding, which is the point
// at which the price path's latency slice is at risk rather than the link
// itself. That is the number worth alerting on: the platform's promise is a
// three-second price change, not a full-strength radio.
const DefaultWeakLQI = 80

// MeshNodeView is one label's place in its controller's Zigbee tree, enriched
// with what only the whole topology can tell you.
//
// The controller reports each node's own view — its parent, its own idea of its
// depth. The registry recomputes depth from the tree instead of trusting the
// report, because a node whose parent changed mid-scan reports a depth that its
// parent's report contradicts, and a topology map that disagrees with itself is
// worse than none.
type MeshNodeView struct {
	canon.MeshNode
	// Children are the nodes that named this one as their parent, in stable
	// order.
	Children []canon.LabelID `json:"children,omitempty"`
	// DerivedDepth is hop count from the controller, computed by walking the
	// tree. It is -1 for an orphan, which by definition has no path.
	DerivedDepth int `json:"derived_depth"`
	// DepthDisagrees is set when the node's self-reported depth differs from the
	// derived one. It is the early warning that the mesh is re-parenting, which
	// precedes a link failure often enough to be worth surfacing.
	DepthDisagrees bool `json:"depth_disagrees,omitempty"`
	// Orphaned means the node has no path to the controller: it named a parent
	// that is not in the topology, or it sits in a cycle. An orphaned node is
	// physically present and radio-active but unreachable, which is exactly the
	// failure a shelf audit would otherwise find by hand.
	Orphaned bool `json:"orphaned"`
	// WeakLink means the node's own link quality is below the weak threshold.
	WeakLink bool `json:"weak_link,omitempty"`
}

// MeshTree is the assembled topology of one controller's mesh.
type MeshTree struct {
	SECID   canon.SECID   `json:"sec_id"`
	StoreID canon.StoreID `json:"store_id"`
	// Roots are the nodes parented directly to the controller, in stable order.
	Roots []canon.LabelID `json:"roots"`
	// Nodes are every reported node, ordered by label id so that two renders of
	// the same topology are byte-identical.
	Nodes []MeshNodeView `json:"nodes"`
	// Orphans lists the unreachable nodes, ordered by label id.
	Orphans []canon.LabelID `json:"orphans"`
	// MaxDepth is the deepest hop count reached from the controller.
	MaxDepth int `json:"max_depth"`
	// Routers counts the mains-free labels acting as routers. A mesh with too
	// few routers has no redundancy: one dead label partitions a bay.
	Routers int `json:"routers"`
	// AverageLQI is the mean link quality over reachable nodes.
	AverageLQI float64 `json:"average_lqi"`
	// WeakLinks counts nodes below the weak-LQI threshold.
	WeakLinks int `json:"weak_links"`
	// UpdatedAt is the controller's report time.
	UpdatedAt time.Time `json:"updated_at"`

	index map[canon.LabelID]int
}

// Node returns the view of one label, and whether it was in the report.
func (t *MeshTree) Node(id canon.LabelID) (MeshNodeView, bool) {
	i, ok := t.index[id]
	if !ok {
		return MeshNodeView{}, false
	}
	return t.Nodes[i], true
}

// Size returns the number of nodes in the topology.
func (t *MeshTree) Size() int { return len(t.Nodes) }

// BuildMeshTree assembles a controller's topology report into a tree, deriving
// depth, children, orphans and the link-quality summary.
//
// Orphan detection is the reason this is a graph walk and not a sort. A node is
// orphaned when no path from the controller reaches it, which covers three real
// failures with one rule: it named a parent that is not in the report (the
// parent died between the two nodes' scans), it named a parent that is itself
// orphaned (a whole sub-tree lost its route when one router's battery went), or
// it sits in a cycle (two labels each believing the other is their parent,
// which the Zigbee stack does briefly produce during a re-parent storm). All
// three mean the same thing to the store: those labels will not receive a price
// change, and someone has to walk the aisle.
//
// weakLQI of zero uses DefaultWeakLQI.
func BuildMeshTree(report canon.MeshTopology, weakLQI int) *MeshTree {
	if weakLQI <= 0 {
		weakLQI = DefaultWeakLQI
	}
	tree := &MeshTree{
		SECID:     report.SECID,
		StoreID:   report.StoreID,
		UpdatedAt: report.UpdatedAt.UTC(),
		Roots:     []canon.LabelID{},
		Orphans:   []canon.LabelID{},
		index:     make(map[canon.LabelID]int, len(report.Nodes)),
	}

	// Deduplicate by label, keeping the last report for a node. A controller
	// that re-scanned a node mid-report sends it twice; the later view is the
	// one that reflects the re-parent.
	nodes := make(map[canon.LabelID]canon.MeshNode, len(report.Nodes))
	order := make([]canon.LabelID, 0, len(report.Nodes))
	for _, n := range report.Nodes {
		if n.LabelID == "" {
			continue
		}
		if _, seen := nodes[n.LabelID]; !seen {
			order = append(order, n.LabelID)
		}
		nodes[n.LabelID] = n
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	children := make(map[canon.LabelID][]canon.LabelID, len(nodes))
	var roots []canon.LabelID
	for _, id := range order {
		n := nodes[id]
		if n.ParentID == "" {
			roots = append(roots, id)
			continue
		}
		if n.ParentID == id {
			// A node claiming itself as parent is a corrupt report; treat it as
			// unrooted so the walk below marks it orphaned rather than looping.
			continue
		}
		if _, ok := nodes[n.ParentID]; !ok {
			continue // parent absent: handled as an orphan by the walk
		}
		children[n.ParentID] = append(children[n.ParentID], id)
	}
	for k := range children {
		sort.Slice(children[k], func(i, j int) bool { return children[k][i] < children[k][j] })
	}

	// Breadth-first from the controller. Anything the walk does not reach is
	// unreachable, whatever its report claimed.
	depth := make(map[canon.LabelID]int, len(nodes))
	queue := make([]canon.LabelID, 0, len(nodes))
	for _, r := range roots {
		depth[r] = 1
		queue = append(queue, r)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if _, seen := depth[c]; seen {
				continue
			}
			depth[c] = depth[cur] + 1
			queue = append(queue, c)
		}
	}

	var lqiSum, lqiCount int
	tree.Nodes = make([]MeshNodeView, 0, len(order))
	for _, id := range order {
		n := nodes[id]
		d, reachable := depth[id]
		view := MeshNodeView{
			MeshNode:     n,
			Children:     children[id],
			DerivedDepth: -1,
			Orphaned:     !reachable,
			WeakLink:     n.LQI > 0 && n.LQI < weakLQI,
		}
		if reachable {
			view.DerivedDepth = d
			view.DepthDisagrees = n.Depth > 0 && n.Depth != d
			if d > tree.MaxDepth {
				tree.MaxDepth = d
			}
			if n.LQI > 0 {
				lqiSum += n.LQI
				lqiCount++
			}
		} else {
			tree.Orphans = append(tree.Orphans, id)
		}
		if n.Router {
			tree.Routers++
		}
		if view.WeakLink {
			tree.WeakLinks++
		}
		tree.index[id] = len(tree.Nodes)
		tree.Nodes = append(tree.Nodes, view)
	}
	if lqiCount > 0 {
		tree.AverageLQI = float64(lqiSum) / float64(lqiCount)
	}
	tree.Roots = roots
	if tree.Roots == nil {
		tree.Roots = []canon.LabelID{}
	}
	return tree
}
