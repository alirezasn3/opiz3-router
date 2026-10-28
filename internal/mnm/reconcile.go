package mnm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
)

// TagPrefix starts the comment of every item this program manages. Items
// without it were made by hand and are never touched.
const TagPrefix = "opiz3-router: "

// Tag returns the comment that marks an item as managed under key.
func Tag(key string) string { return TagPrefix + key }

// Changes counts what a reconcile did.
type Changes struct {
	Created, Updated, Deleted int
	Issues                    []Issue
}

func (c Changes) Total() int { return c.Created + c.Updated + c.Deleted }

func (c *Changes) add(o Changes) {
	c.Created += o.Created
	c.Updated += o.Updated
	c.Deleted += o.Deleted
	c.Issues = append(c.Issues, o.Issues...)
}

type managed[T any] interface {
	*T
	meta() *Meta
}

func keyOf(m *Meta) (string, bool) { return strings.CutPrefix(m.Comment, TagPrefix) }

// sameItem compares two items by what they would send to the API, ignoring
// the id.
func sameItem[T any, P managed[T]](a, b T) bool {
	pa, pb := P(&a), P(&b)
	pa.meta().ID, pb.meta().ID = "", ""
	ja, ea := json.Marshal(a)
	jb, eb := json.Marshal(b)
	return ea == nil && eb == nil && bytes.Equal(ja, jb)
}

// reconcile makes the items under path that carry this program's tag equal
// want, keyed by their comment. When ordered, the tagged items also end up in
// the order of want (firewall rules run top to bottom). It returns the id of
// each wanted item by key.
func reconcile[T any, P managed[T]](ctx context.Context, c *Client, path string, want []T, ordered bool) (map[string]string, Changes, error) {
	var ch Changes
	var have []T
	if e := c.Do(ctx, "GET", path, nil, &have); e != nil {
		return nil, ch, fmt.Errorf("list %s: %w", path, e)
	}

	wantKeys := make([]string, len(want))
	for i := range want {
		k, ok := keyOf(P(&want[i]).meta())
		if !ok {
			return nil, ch, fmt.Errorf("%s: wanted item %d is not tagged", path, i)
		}
		wantKeys[i] = k
	}
	ours := map[string]*T{}
	var haveKeys []string
	var stale []*T
	for i := range have {
		k, ok := keyOf(P(&have[i]).meta())
		if !ok {
			continue
		}
		if _, dup := ours[k]; dup || !slices.Contains(wantKeys, k) {
			stale = append(stale, &have[i])
			continue
		}
		ours[k] = &have[i]
		haveKeys = append(haveKeys, k)
	}

	// New items are appended, so the order is only right when the kept items
	// already follow the wanted order and every new item comes after them.
	rebuild := false
	if ordered {
		var kept []string
		seenNew := false
		for _, k := range wantKeys {
			if ours[k] != nil {
				if seenNew {
					rebuild = true
				}
				kept = append(kept, k)
			} else {
				seenNew = true
			}
		}
		rebuild = rebuild || !slices.Equal(kept, haveKeys)
	}
	if rebuild {
		for k, it := range ours {
			stale = append(stale, it)
			delete(ours, k)
		}
	}

	ids := map[string]string{}
	remove := func(it *T) error {
		var res writeResult
		if e := c.Do(ctx, "DELETE", path+"/"+P(it).meta().ID, nil, &res); e != nil {
			return fmt.Errorf("delete %s/%s: %w", path, P(it).meta().ID, e)
		}
		ch.Deleted++
		ch.Issues = append(ch.Issues, res.Issues...)
		return nil
	}
	for _, it := range stale {
		if e := remove(it); e != nil {
			return ids, ch, e
		}
	}
	for i := range want {
		w := want[i]
		if cur := ours[wantKeys[i]]; cur != nil {
			ids[wantKeys[i]] = P(cur).meta().ID
			if sameItem[T, P](w, *cur) {
				continue
			}
			P(&w).meta().ID = ids[wantKeys[i]]
			log.Printf("mini-net-man: updating %s %s", path, wantKeys[i])
			var res writeResult
			if e := c.Do(ctx, "PUT", path+"/"+ids[wantKeys[i]], w, &res); e != nil {
				return ids, ch, fmt.Errorf("update %s/%s: %w", path, ids[wantKeys[i]], e)
			}
			ch.Updated++
			ch.Issues = append(ch.Issues, res.Issues...)
			continue
		}
		var res writeResult
		if e := c.Do(ctx, "POST", path, w, &res); e != nil {
			return ids, ch, fmt.Errorf("create %s %s: %w", path, wantKeys[i], e)
		}
		ids[wantKeys[i]] = res.ID
		ch.Created++
		ch.Issues = append(ch.Issues, res.Issues...)
	}
	return ids, ch, nil
}
