package app

import "strconv"

// pageNav is the view model for the shared "pager" partial
// (templates/pager.html, layout.css .l-pager). Pages is the total
// page count (>= 1); the partial renders only when Pages > 1.
type pageNav struct {
	Page       int
	Pages      int
	Total      int
	FirstHref  string
	PrevHref   string
	NextHref   string
	LastHref   string
	LandTarget string // optional DOM id for swap.js scroll landing
}

// buildPageNav clamps page into [1, pages] and builds the first,
// previous, next, and last page URLs using hrefFor(p). Returns nil
// when there is at most one page.
func buildPageNav(page, pages, total int, landTarget string, hrefFor func(int) string) *pageNav {
	if pages <= 1 {
		return nil
	}
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	nav := &pageNav{
		Page:       page,
		Pages:      pages,
		Total:      total,
		LandTarget: landTarget,
	}
	if page > 1 {
		nav.FirstHref = hrefFor(1)
		nav.PrevHref = hrefFor(page - 1)
	}
	if page < pages {
		nav.NextHref = hrefFor(page + 1)
		nav.LastHref = hrefFor(pages)
	}
	return nav
}

// pageParam formats a 1-based page number for a query string,
// returning "" for page 1 so the first page stays canonical.
func pageParam(page int) string {
	if page <= 1 {
		return ""
	}
	return strconv.Itoa(page)
}
