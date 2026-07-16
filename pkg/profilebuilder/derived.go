package profilebuilder

// derived.go — the derived-segment rule kinds (composite + lookalike),
// evaluated in a SECOND pass after behavioural rules so they see this run's
// behavioural output. Both enroll at person level and expand to every
// cluster member, with replace-by-segment prune — identical membership
// semantics to behavioural rules, only the qualification logic differs.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

// segmentMembers loads a segment's member ids, enforcing that the segment
// belongs to accountID — a composite/lookalike rule must not read another
// tenant's memberships.
func segmentMembers(ctx context.Context, db *sql.DB, accountID, segmentID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT m.user_id
FROM audience_segment_members m
JOIN audience_segments s ON s.id = m.segment_id
WHERE m.segment_id = $1::uuid AND s.account_id = $2::uuid`, segmentID, accountID)
	if err != nil {
		return nil, fmt.Errorf("members of %s: %w", segmentID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out = append(out, uid)
	}
	return out, rows.Err()
}

// personsOf maps member ids to their person keys (the id itself for
// singletons) and returns the person set.
func personsOf(c Clusters, members []string) map[string]bool {
	out := make(map[string]bool, len(members))
	for _, m := range members {
		if pid, ok := c.PersonOf[m]; ok {
			out[pid] = true
		} else {
			out[m] = true
		}
	}
	return out
}

// personMembers returns the member ids a qualified person expands to.
func personMembers(c Clusters, person string) []string {
	if members, ok := c.Members[person]; ok {
		return members
	}
	return []string{person} // singleton: the person key IS the id
}

// evaluateComposite qualifies persons by set logic over the referenced
// segments' person sets and returns the expanded member ids.
func evaluateComposite(ctx context.Context, db *sql.DB, accountID string, rule CompositeRule, c Clusters) ([]string, error) {
	load := func(segIDs []string) ([]map[string]bool, error) {
		sets := make([]map[string]bool, 0, len(segIDs))
		for _, id := range segIDs {
			members, err := segmentMembers(ctx, db, accountID, id)
			if err != nil {
				return nil, err
			}
			sets = append(sets, personsOf(c, members))
		}
		return sets, nil
	}
	allOf, err := load(rule.AllOf)
	if err != nil {
		return nil, err
	}
	anyOf, err := load(rule.AnyOf)
	if err != nil {
		return nil, err
	}
	noneOf, err := load(rule.NoneOf)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, person := range compositeQualified(allOf, anyOf, noneOf) {
		out = append(out, personMembers(c, person)...)
	}
	sort.Strings(out)
	return out, nil
}

// compositeQualified applies the set logic over person sets: every all_of,
// at least one any_of (when present), no none_of. Pure — unit-testable
// without a database.
func compositeQualified(allOf, anyOf, noneOf []map[string]bool) []string {
	// Candidates: the first all_of set (intersection can only shrink it),
	// else the union of any_of.
	candidates := map[string]bool{}
	switch {
	case len(allOf) > 0:
		for p := range allOf[0] {
			candidates[p] = true
		}
	default:
		for _, set := range anyOf {
			for p := range set {
				candidates[p] = true
			}
		}
	}
	var out []string
	for person := range candidates {
		qualified := true
		for _, set := range allOf {
			if !set[person] {
				qualified = false
				break
			}
		}
		if qualified && len(anyOf) > 0 {
			any := false
			for _, set := range anyOf {
				if set[person] {
					any = true
					break
				}
			}
			qualified = any
		}
		if qualified {
			for _, set := range noneOf {
				if set[person] {
					qualified = false
					break
				}
			}
		}
		if qualified {
			out = append(out, person)
		}
	}
	sort.Strings(out)
	return out
}

// personCategories aggregates behaviour rows into per-person category sets.
// The user key mirrors the serve-path precedence (user_id, else household).
func personCategories(rows []datalake.Record, c Clusters) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, rec := range rows {
		key := str(rec["user_id"])
		if key == "" {
			key = str(rec["household_id"])
		}
		if key == "" {
			continue
		}
		person := key
		if pid, ok := c.PersonOf[key]; ok {
			person = pid
		}
		for _, cat := range strings.Split(str(rec["categories"]), ",") {
			cat = strings.ToLower(strings.TrimSpace(cat))
			if cat == "" {
				continue
			}
			if out[person] == nil {
				out[person] = map[string]bool{}
			}
			out[person][cat] = true
		}
	}
	return out
}

// evaluateLookalike scores non-seed persons by overlap with the seed's
// top categories and returns the expanded member ids of qualifiers.
func evaluateLookalike(ctx context.Context, db *sql.DB, accountID string, rule LookalikeRule,
	c Clusters, behaviourRows []datalake.Record,
) ([]string, error) {
	seedMembers, err := segmentMembers(ctx, db, accountID, rule.SeedSegment)
	if err != nil {
		return nil, err
	}
	seedPersons := personsOf(c, seedMembers)
	if len(seedPersons) == 0 {
		return nil, nil // empty seed → nothing to resemble
	}
	byPerson := personCategories(behaviourRows, c)
	var out []string
	for _, person := range lookalikeQualified(seedPersons, byPerson, rule) {
		out = append(out, personMembers(c, person)...)
	}
	sort.Strings(out)
	return out, nil
}

// lookalikeQualified is the pure scoring core: rank the seed's top
// categories, score non-seed persons by shared fraction, return qualifiers
// ordered by score (ties by person id), capped at MaxMembers.
func lookalikeQualified(seedPersons map[string]bool, byPerson map[string]map[string]bool, rule LookalikeRule) []string {

	// Seed profile: categories ranked by how many seed persons exhibit them
	// (ties broken by name for determinism).
	freq := map[string]int{}
	for person := range seedPersons {
		for cat := range byPerson[person] {
			freq[cat]++
		}
	}
	type catCount struct {
		cat string
		n   int
	}
	ranked := make([]catCount, 0, len(freq))
	for cat, n := range freq {
		ranked = append(ranked, catCount{cat, n})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].n != ranked[j].n {
			return ranked[i].n > ranked[j].n
		}
		return ranked[i].cat < ranked[j].cat
	})
	if len(ranked) > rule.TopCategories {
		ranked = ranked[:rule.TopCategories]
	}
	if len(ranked) == 0 {
		return nil // seed has no behavioural profile yet
	}
	seedCats := map[string]bool{}
	for _, rc := range ranked {
		seedCats[rc.cat] = true
	}

	// Score candidates: fraction of the seed's top categories they share.
	type scored struct {
		person string
		score  float64
	}
	var candidates []scored
	for person, cats := range byPerson {
		if seedPersons[person] {
			continue
		}
		hits := 0
		for cat := range cats {
			if seedCats[cat] {
				hits++
			}
		}
		score := float64(hits) / float64(len(seedCats))
		if score >= rule.MinSimilarity {
			candidates = append(candidates, scored{person, score})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].person < candidates[j].person
	})
	if len(candidates) > rule.MaxMembers {
		candidates = candidates[:rule.MaxMembers]
	}

	out := make([]string, 0, len(candidates))
	for _, cand := range candidates {
		out = append(out, cand.person)
	}
	return out
}
