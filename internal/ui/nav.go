package ui

// The navigation (mockup v3's Nav): four groups, the pages of the active group, one badge, and the
// autonomy pill. Pages that are not built yet are in it as drawn and say so when opened.

type navPage struct {
	Label, Href string
	Current     bool
}

type navGroup struct {
	Label, Href, Icon string
	Current           bool
	Badge             int // only Decide carries it: what waits for the operator
	Pages             []navPage
}

var groupIcons = map[string]string{
	"Decide": "M3 12l5 5L21 4",
	"Watch":  "M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12zM12 15a3 3 0 100-6 3 3 0 000 6z",
	"Govern": "M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6l8-3z",
	"Record": "M5 4h11l3 3v13H5zM9 9h6M9 13h6M9 17h4",
}

// nav marks the active page by its label; waiting is the badge's count.
func nav(active string, waiting int) []navGroup {
	groups := []navGroup{
		{Label: "Decide", Badge: waiting, Pages: []navPage{{Label: "Now", Href: "/"}, {Label: "Approvals", Href: "/approvals"}}},
		{Label: "Watch", Pages: []navPage{{Label: "Cases", Href: "/cases"}, {Label: "Runs", Href: "/runs"},
			{Label: "Fleets", Href: "/fleets"}, {Label: "Hosts", Href: "/hosts"}}},
		{Label: "Govern", Pages: []navPage{{Label: "Autonomy", Href: "/autonomy"}, {Label: "Models and spend", Href: "/models"},
			{Label: "Knowledge", Href: "/knowledge"}}},
		{Label: "Record", Pages: []navPage{{Label: "Brief", Href: "/brief"}, {Label: "History", Href: "/history"}}},
	}
	for i := range groups {
		groups[i].Href, groups[i].Icon = groups[i].Pages[0].Href, groupIcons[groups[i].Label]
		for j := range groups[i].Pages {
			if groups[i].Pages[j].Label == active {
				groups[i].Pages[j].Current, groups[i].Current = true, true
			}
		}
	}
	return groups
}
