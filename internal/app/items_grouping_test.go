package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestGroupSearchRows: rows arriving ordered by category and group fold
// into one section per group, in arrival order, and a group that
// reappears later (a different page) starts its own section.
func TestGroupSearchRows(t *testing.T) {
	rows := []itemSearchRow{
		{ID: 1, Name: "A", GroupID: 10, GroupName: "G10", CategoryID: 1, CategoryName: "C1"},
		{ID: 2, Name: "B", GroupID: 10, GroupName: "G10", CategoryID: 1, CategoryName: "C1"},
		{ID: 3, Name: "C", GroupID: 20, GroupName: "G20", CategoryID: 1, CategoryName: "C1"},
		{ID: 4, Name: "D", GroupID: 30, GroupName: "G30", CategoryID: 2, CategoryName: "C2"},
		{ID: 5, Name: "E"},
		{ID: 6, Name: "F"},
	}
	got := groupSearchRows(rows)
	if len(got) != 4 {
		t.Fatalf("%d sections, want 4: %+v", len(got), got)
	}
	counts := []int{len(got[0].Rows), len(got[1].Rows), len(got[2].Rows), len(got[3].Rows)}
	if counts[0] != 2 || counts[1] != 1 || counts[2] != 1 || counts[3] != 2 {
		t.Fatalf("rows per section = %v, want [2 1 1 2]", counts)
	}
	if got[3].GroupID != 0 || got[3].CategoryID != 0 {
		t.Fatalf("ungrouped rows section = %+v, want group 0 / category 0", got[3])
	}
	if groupSearchRows(nil) != nil {
		t.Fatal("no rows should give no sections")
	}
}

// TestItemsSearchKeepsCategorisation: search and filter results come
// back under Category › Group headings, ordered by category then group
// (not as one flat name-sorted list), the market-only filter drops
// items but keeps the headings of what is left, and the filter controls
// apply themselves as they change.
func TestItemsSearchKeepsCategorisation(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedNext1SDE(t, conn)

	code, body := getPage(t, app, cookie, "/items/?q=tritanium")
	if code != http.StatusOK {
		t.Fatalf("search: status %d", code)
	}
	// One heading per group, categories alphabetical (Far Category
	// before Fixture Category), groups alphabetical within.
	order := []string{
		`<h3 class="item-section"><a href="/items/category/901/">Far Category</a> › <a href="/items/group/930/">Far Group</a></h3>`,
		`<a href="/items/category/900/">Fixture Category</a> › <a href="/items/group/910/">Fixture Minerals</a></h3>`,
		`<a href="/items/category/900/">Fixture Category</a> › <a href="/items/group/920/">Fixture Ores</a></h3>`,
	}
	last := -1
	for _, heading := range order {
		at := strings.Index(body, heading)
		if at < 0 {
			t.Fatalf("results missing heading %q", heading)
		}
		if at < last {
			t.Fatalf("heading %q is out of order", heading)
		}
		last = at
	}
	// Within a group the name that starts with the query comes first.
	mineralsAt := strings.Index(body, `/items/group/910/">Fixture Minerals</a></h3>`)
	oresAt := strings.Index(body, `/items/group/920/">Fixture Ores</a></h3>`)
	if mineralsAt < 0 || oresAt < mineralsAt {
		t.Fatalf("Fixture Minerals (%d) and Fixture Ores (%d) headings not in order", mineralsAt, oresAt)
	}
	minerals := body[mineralsAt:oresAt]
	exact, contains := strings.Index(minerals, ">Tritanium</a>"), strings.Index(minerals, ">Secret Tritanium</a>")
	if exact < 0 || contains < 0 || exact > contains {
		t.Errorf("in Fixture Minerals, a name starting with the query (at %d) should come before one that only contains it (at %d)", exact, contains)
	}

	// Market-only keeps the tree: the relic's group (unpublished, no
	// market group) disappears with its only item; the others keep
	// their headings.
	_, body = getPage(t, app, cookie, "/items/?q=tritanium&market=1")
	if strings.Contains(body, "Fixture Ores") {
		t.Error("market-only still shows the heading of a group with nothing left in it")
	}
	for _, want := range []string{"Far Group", "Fixture Minerals", "/items/group/910/?market=1"} {
		if !strings.Contains(body, want) {
			t.Errorf("market-only results missing %q", want)
		}
	}
	// No flat list: no per-row Group/Category columns any more.
	if strings.Contains(body, "<th>Category</th>") {
		t.Error("results still carry the flat Group/Category columns")
	}

	// The controls apply themselves.
	mustContain(t, "/items/", body,
		`name="market" value="1" data-autosubmit checked`,
		`<select name="category" data-autosubmit>`)
}
