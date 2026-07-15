package profilebuilder

import (
	"sort"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// unionFind is a standard disjoint-set with path compression + union by size.
type unionFind struct {
	parent map[string]string
	size   map[string]int
}

func newUnionFind() *unionFind {
	return &unionFind{parent: map[string]string{}, size: map[string]int{}}
}

func (u *unionFind) find(x string) string {
	root, ok := u.parent[x]
	if !ok {
		u.parent[x] = x
		u.size[x] = 1
		return x
	}
	if root == x {
		return x
	}
	r := u.find(root)
	u.parent[x] = r // path compression
	return r
}

func (u *unionFind) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if u.size[ra] < u.size[rb] {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	u.size[ra] += u.size[rb]
}

// Clusters is the materialized person-level view: person id → sorted member
// ids (only clusters with ≥2 members are stored — a singleton IS its own
// person and needs no row), plus the member → person reverse index.
type Clusters struct {
	Members  map[string][]string // person_id → member ids
	PersonOf map[string]string   // member id → person_id
}

// personID derives the stable cluster key: the lexicographically-smallest
// member id, prefixed. Deterministic across runs as long as the smallest
// member stays in the cluster — no registry needed.
func personID(members []string) string {
	return "person:" + members[0] // members is sorted
}

// buildClusters runs union-find over the identity adjacency (the same
// bidirectional map the DSP's read-time resolver preloads), with two guards:
//
//   - minConfidence drops weak (probabilistic) edges before linking, same
//     knob the DSP exposes as dsp.identity_min_confidence.
//   - household ids (hh: prefix) are EXCLUDED: a household edge groups the
//     people behind one IP, it doesn't identify one person — merging via it
//     would collapse roommates into a single profile. Household remains its
//     own grouping level, resolved at serve time via the household EID.
//
// Clusters larger than maxClusterSize are dropped with a count returned —
// a mega-cluster is almost always a linking pathology (shared device,
// polluted probabilistic edges), and expanding memberships across it would
// smear one person's segments over hundreds of strangers.
func buildClusters(adj map[string][]postgres.IdentityLink, minConfidence float64, maxClusterSize int) (Clusters, int) {
	uf := newUnionFind()
	for id, links := range adj {
		if strings.HasPrefix(id, identity.HouseholdIDPrefix) {
			continue
		}
		for _, l := range links {
			if l.Confidence < minConfidence || strings.HasPrefix(l.ID, identity.HouseholdIDPrefix) {
				continue
			}
			uf.union(id, l.ID)
		}
	}

	byRoot := map[string][]string{}
	for id := range uf.parent {
		root := uf.find(id)
		byRoot[root] = append(byRoot[root], id)
	}

	out := Clusters{Members: map[string][]string{}, PersonOf: map[string]string{}}
	dropped := 0
	for _, members := range byRoot {
		if len(members) < 2 {
			continue // singleton — its own person, no materialization needed
		}
		if maxClusterSize > 0 && len(members) > maxClusterSize {
			dropped++
			continue
		}
		sort.Strings(members)
		pid := personID(members)
		out.Members[pid] = members
		for _, m := range members {
			out.PersonOf[m] = pid
		}
	}
	return out, dropped
}
