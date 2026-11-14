package module

// ShareCard fills the shared share dialog (templates/sharecard.html):
// a preview of a shareable image with the ways to pass it on. Image
// is the image's path (loaded only when the dialog opens), Download
// the same image with a download disposition, Link the page the
// image stands for. Width and Height are the image's pixel size.
type ShareCard struct {
	ID, Title, Lead string
	Image, Download string
	Width, Height   int
	Alt, Link       string
	FileName, Note  string
}

// FitShareCard is a fit page's share dialog: the fit's summary card
// (the six glance tiles), its download and the page as the link.
// An empty pagePath leaves the dialog without a target, for a page
// that names one later (the fitting tool, once a fit is shared).
func FitShareCard(pagePath, name, ship string) *ShareCard {
	c := &ShareCard{
		ID:       "fit-share",
		Title:    "Share this fit",
		Lead:     "The card shows the fit's damage, tank, capacitor, speed, lock range and signature.",
		Width:    1200,
		Height:   630,
		Alt:      "Card for " + name + " (" + ship + ")",
		FileName: name + " - " + ship + ".png",
		Note:     "The link unfurls into this card in Discord, Slack and most chat apps.",
	}
	if pagePath != "" {
		c.Image, c.Download, c.Link = pagePath+"card.png", pagePath+"card.png?download=1", pagePath
	}
	return c
}
