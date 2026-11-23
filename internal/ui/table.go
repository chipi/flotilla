package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Every table has sortable headers, filters and pages (the operator, 2026-11-14), on the server: the
// state is the URL, so a page can be linked, reloaded and used without JavaScript.

const perPage = 20

type tableQ struct {
	Path        string
	vals        url.Values
	Sort        string
	Desc        bool
	Page, Total int
	Per         int
	Prefix      string // the keys' prefix when a page has two tables, so each keeps its own sort, filters and page
	Anchor      string // the table's id on the page: its links come back to it, not to the top
}

// readTable reads sort, dir and page from the request, with the table's default order.
func readTable(r *http.Request, defSort string, defDesc bool, sortable func(string) bool) tableQ {
	return readTableAs(r, "", defSort, defDesc, sortable)
}

// readTableAs is readTable for a table whose keys carry prefix ("r" reads rsort, rdir, rpage, rq).
func readTableAs(r *http.Request, prefix, defSort string, defDesc bool, sortable func(string) bool) tableQ {
	v := r.URL.Query()
	t := tableQ{Path: r.URL.Path, vals: v, Sort: defSort, Desc: defDesc, Page: 1, Per: perPage, Prefix: prefix}
	if s := v.Get(prefix + "sort"); s != "" && sortable(s) {
		t.Sort, t.Desc = s, v.Get(prefix+"dir") == "desc"
	}
	if p, err := strconv.Atoi(v.Get(prefix + "page")); err == nil && p > 1 {
		t.Page = p
	}
	return t
}

func (t tableQ) Offset() int { return (t.Page - 1) * t.Per }

// Key is a key's name in the URL for this table.
func (t tableQ) Key(k string) string { return t.Prefix + k }

// Get is a filter's current value.
func (t tableQ) Get(k string) string { return t.vals.Get(t.Key(k)) }

// href is this table's URL with the given changes; an empty value removes the key. Any change other
// than the page goes back to page 1. Another table's keys on the page are kept as they are.
func (t tableQ) href(kv ...string) string {
	v := url.Values{}
	for k, vs := range t.vals {
		v[k] = append([]string(nil), vs...)
	}
	keepPage := false
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] == "page" {
			keepPage = true
		}
		if kv[i+1] == "" {
			v.Del(t.Key(kv[i]))
		} else {
			v.Set(t.Key(kv[i]), kv[i+1])
		}
	}
	if !keepPage {
		v.Del(t.Key("page"))
	}
	if len(v) == 0 {
		return t.Path + t.frag()
	}
	return t.Path + "?" + v.Encode() + t.frag()
}

// With is the URL with one filter set ("" clears it).
func (t tableQ) With(k, v string) string { return t.href(k, v) }

// With2 sets two keys at once.
func (t tableQ) With2(k1, v1, k2, v2 string) string { return t.href(k1, v1, k2, v2) }

// Hidden are the hidden inputs a filter form keeps, so a search keeps the other filters and the sort.
func (t tableQ) Hidden(except ...string) []struct{ K, V string } {
	skip := map[string]bool{t.Key("page"): true}
	for _, e := range except {
		skip[t.Key(e)] = true
	}
	var out []struct{ K, V string }
	for k, vs := range t.vals {
		if skip[k] || len(vs) == 0 || vs[0] == "" {
			continue
		}
		out = append(out, struct{ K, V string }{k, vs[0]})
	}
	return out
}

type thCell struct {
	Label, Href, Arrow, AriaSort string
	On, Sortable                 bool
}

// Th is a header cell: a link that sorts by the column, ascending first, then the other way.
func (t tableQ) Th(col, label string) thCell {
	on := t.Sort == col
	c := thCell{Label: label, Sortable: true, On: on, Arrow: "↕", AriaSort: "none"}
	dir := ""
	if on && !t.Desc {
		dir = "desc"
	}
	c.Href = t.href("sort", col, "dir", dir)
	if on {
		c.Arrow, c.AriaSort = "▲", "ascending"
		if t.Desc {
			c.Arrow, c.AriaSort = "▼", "descending"
		}
	}
	return c
}

// Plain is a header cell that does not sort.
func (t tableQ) Plain(label string) thCell { return thCell{Label: label} }

func (t tableQ) Pages() int {
	if t.Total == 0 {
		return 1
	}
	return (t.Total + t.Per - 1) / t.Per
}

// Range is "51–100 of 312".
func (t tableQ) Range() string {
	if t.Total == 0 {
		return "0 of 0"
	}
	from := t.Offset() + 1
	to := min(t.Offset()+t.Per, t.Total)
	return fmt.Sprintf("%d–%d of %d", from, to, t.Total)
}

func (t tableQ) PrevHref() string {
	if t.Page <= 1 {
		return ""
	}
	return t.href("page", strconv.Itoa(t.Page-1))
}

func (t tableQ) NextHref() string {
	if t.Page >= t.Pages() {
		return ""
	}
	return t.href("page", strconv.Itoa(t.Page+1))
}

// window returns the page of rows a table shows, for tables sorted and filtered in Go.
func window[T any](rows []T, t *tableQ) []T {
	t.Total = len(rows)
	if t.Offset() >= len(rows) {
		if len(rows) > 0 {
			t.Page = t.Pages()
		}
		if t.Offset() >= len(rows) {
			return nil
		}
	}
	return rows[t.Offset():min(t.Offset()+t.Per, len(rows))]
}

func (t tableQ) frag() string {
	if t.Anchor == "" {
		return ""
	}
	return "#" + t.Anchor
}

// Action is the URL a table's search form submits to.
func (t tableQ) Action() string { return t.Path + t.frag() }
