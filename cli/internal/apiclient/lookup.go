package apiclient

import (
	"fmt"
	"regexp"
	"strings"
)

// shortID is the start of a hypothesis id, which is a UUID: what an agent copies from a listing.
var shortID = regexp.MustCompile(`^[0-9a-f][0-9a-f-]{3,34}$`)

// maxPrefixMatches is how many ids an ambiguous start lists.
const maxPrefixMatches = 10

// FullHypothesisID is the hypothesis of platform experiment pe whose id starts with id, as git
// takes the start of a commit: id itself when it is no such start or pe is unknown, refused when
// no hypothesis's id starts with it or several do, with the ones that do.
func FullHypothesisID(c *Client, pe, id string) (string, error) {
	if pe == "" || !shortID.MatchString(id) {
		return id, nil
	}
	var out struct {
		Rows [][]string `json:"rows"`
	}
	query := map[string]any{
		"platform_experiment_id": pe,
		"cypher":                 fmt.Sprintf("MATCH (h:Hypothesis) WHERE h.id STARTS WITH $prefix RETURN h.id ORDER BY h.id LIMIT %d", maxPrefixMatches+1),
		"params":                 map[string]any{"prefix": id},
	}
	if err := c.Do("POST", "/hypotheses/query", query, &out); err != nil {
		return "", fmt.Errorf("find the hypothesis whose id starts with %s: %w", id, err)
	}
	var ids []string
	for _, row := range out.Rows {
		if len(row) == 1 {
			ids = append(ids, row[0])
		}
	}
	switch {
	case len(ids) == 1:
		return ids[0], nil
	case len(ids) == 0:
		return "", fmt.Errorf("no hypothesis of %s has an id that starts with %s: hl hypothesis list prints their ids", pe, id)
	case len(ids) > maxPrefixMatches:
		return "", fmt.Errorf("more than %d hypotheses of %s have an id that starts with %s, among them %s: give more of the id", maxPrefixMatches, pe, id, strings.Join(ids[:maxPrefixMatches], ", "))
	}
	return "", fmt.Errorf("%d hypotheses of %s have an id that starts with %s: %s. Give more of the id", len(ids), pe, id, strings.Join(ids, ", "))
}
