// The review screen: every setting in one scrolling list, grouped,
// with a filter, inline editing and a check of the whole set.
package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const installRowLabel = "Install EveSynapse"

// reviewItems are the selectable rows after the filter, in order.
// The last item is always the install action, with an empty key
// and an empty group.
func (m model) reviewItems() []reviewRow {
	q := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	var out []reviewRow
	for _, r := range m.a.reviewRows(m.showAll || q != "") {
		if q == "" || strings.Contains(strings.ToLower(r.setting+" "+r.group+" "+r.help), q) {
			out = append(out, r)
		}
	}
	return append(out, reviewRow{setting: installRowLabel})
}

func isInstallRow(r reviewRow) bool { return r.setting == installRowLabel }

func (m *model) clampCursor() {
	n := len(m.reviewItems())
	m.cursor = max(0, min(m.cursor, n-1))
}

// reviewLine is one display line: a group heading or an item.
type reviewLine struct {
	heading string
	item    int // index into reviewItems; -1 on a heading
}

func reviewLines(items []reviewRow) []reviewLine {
	var out []reviewLine
	group := ""
	for i, r := range items {
		if !isInstallRow(r) && r.group != group {
			group = r.group
			out = append(out, reviewLine{heading: group, item: -1})
		}
		if isInstallRow(r) {
			out = append(out, reviewLine{item: -1})
		}
		out = append(out, reviewLine{item: i})
	}
	return out
}

// reviewChromeRows is everything on the screen except the list:
// the title, filter, detail pane and key hints.
const reviewChromeRows = 12

func (m model) listRows() int {
	_, h := m.size()
	blockers, warnings := m.a.reviewProblems()
	shown := min(len(blockers)+len(warnings), 3)
	return max(3, h-reviewChromeRows-shown-m.reviewHeaderRows())
}

// reviewHeaderRows is the art's share of the review screen. The list
// is what this screen is for, so the art only appears with room.
func (m model) reviewHeaderRows() int {
	if l, ok := m.reviewHeader(); ok {
		_, r := l.size()
		return r + 1
	}
	return 0
}

func (m model) reviewHeader() (headerLayout, bool) {
	w, h := m.size()
	switch {
	case h >= 44:
		return pickHeader(w, 11)
	case h >= 34:
		return pickHeader(w, 6)
	case h >= 26:
		return pickHeader(w, 3)
	}
	return headerLayout{}, false
}

func (m *model) keepCursorVisible() {
	lines := reviewLines(m.reviewItems())
	at := 0
	for i, l := range lines {
		if l.item == m.cursor {
			at = i
			break
		}
	}
	rows := m.listRows()
	if at < m.scroll {
		m.scroll = at
		// Show the group heading along with its first row.
		if at > 0 && lines[at-1].item < 0 {
			m.scroll = at - 1
		}
	}
	if at >= m.scroll+rows {
		m.scroll = at - rows + 1
	}
	m.scroll = max(0, min(m.scroll, max(0, len(lines)-rows)))
}

// handleReviewKey handles the review screen's keys.
func (m model) handleReviewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		m.quitting = true
		return m, tea.Quit
	}
	if m.filtering {
		switch key {
		case "enter", "down":
			m.filtering = false
			m.filter.Blur()
		case "esc":
			m.filtering = false
			m.filter.Blur()
			m.filter.SetValue("")
		default:
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.cursor, m.scroll = 0, 0
			return m, cmd
		}
		m.clampCursor()
		m.keepCursorVisible()
		return m, nil
	}

	items := m.reviewItems()
	cur := items[max(0, min(m.cursor, len(items)-1))]
	m.notice = ""
	switch key {
	case "up", "k":
		m.cursor--
	case "down", "j":
		m.cursor++
	case "pgup":
		m.cursor -= m.listRows()
	case "pgdown", " ":
		m.cursor += m.listRows()
	case "home":
		m.cursor = 0
	case "end":
		m.cursor = len(items) - 1
	case "tab":
		m.cursor = nextGroup(items, m.cursor, 1)
	case "shift+tab":
		m.cursor = nextGroup(items, m.cursor, -1)
	case "/":
		m.filtering = true
		return m, m.filter.Focus()
	case "esc":
		m.filter.SetValue("")
	case "a":
		m.showAll = !m.showAll
		m.cursor, m.scroll = 0, 0
	case "x":
		switch {
		case cur.key == "" || cur.edit != "":
			m.notice = "This line has its own screen: press Enter."
		case cur.key == "ADDR":
			m.a.setValue(cur.key, "")
			m.notice = cur.key + " will use the default, which accepts outside connections."
		default:
			m.a.setValue(cur.key, "")
			m.notice = cur.key + " will be cleared."
		}
	case "u":
		if cur.key != "" && cur.edit == "" {
			m.a.undoValue(cur.key)
			m.notice = "Change to " + cur.key + " undone."
		}
	case "g":
		if s, ok := settingFor(cur.key); ok && s.Generate > 0 {
			v, err := generateValue(s.Generate)
			if err != nil {
				m.notice = "Could not generate a value: " + err.Error()
			} else {
				m.a.setValue(s.Key, v)
				m.notice = "Generated a new " + s.Key + "."
			}
		}
	case "i":
		return m.startInstall()
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "enter":
		switch {
		case isInstallRow(cur):
			return m.startInstall()
		case cur.edit == "source":
			m.returnToReview = true
			m.a.given["source"] = false
			return m, m.enterStage(stageSource)
		case cur.edit == "db":
			m.returnToReview = true
			m.a.given["databaseURL"] = false
			if isLinux() {
				return m, m.enterStage(stageDB)
			}
			return m, m.enterStage(stageDBURL)
		default:
			m.openEditor(cur.key)
			return m, textinput.Blink
		}
	}
	m.clampCursor()
	m.keepCursorVisible()
	return m, nil
}

// nextGroup moves to the first row of the next or previous group.
func nextGroup(items []reviewRow, cur, dir int) int {
	if cur >= len(items) {
		return len(items) - 1
	}
	group := items[cur].group
	i := cur
	for i+dir >= 0 && i+dir < len(items) {
		i += dir
		if items[i].group != group {
			if dir > 0 {
				return i
			}
			// Back to the top of that group.
			g := items[i].group
			for i > 0 && items[i-1].group == g {
				i--
			}
			return i
		}
	}
	if dir < 0 {
		return 0
	}
	return len(items) - 1
}

// startInstall begins the install unless a blocker stands.
func (m model) startInstall() (tea.Model, tea.Cmd) {
	blockers, _ := m.a.reviewProblems()
	if len(blockers) > 0 {
		m.notice = "Fix this first: " + blockers[0]
		return m, nil
	}
	if m.dryRun {
		m.quitting = true
		return m, tea.Quit
	}
	m.stage = stageInstall
	m.step = 0
	if m.a.source == sourceDownload {
		m.step = 1
	}
	m.totalSteps = len(installStepsFor(m.a))
	return m.runInstall()
}

// openEditor opens the edit screen of one setting. A secret starts
// blank, because its current value is not kept.
func (m *model) openEditor(key string) {
	s, ok := settingFor(key)
	if !ok {
		return
	}
	v := ""
	if s.Kind != kindSecret {
		v = m.a.effective(key)
	}
	m.editKey = key
	m.errMsg = ""
	m.startPrompt(stageEdit, s.Default, v, s.Kind == kindSecret)
}

// handleEditKey handles the setting editor's keys.
func (m model) handleEditKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s, _ := settingFor(m.editKey)
	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "esc":
		m.errMsg = ""
		return m, m.enterReview()
	case "tab":
		if len(s.Choices) > 0 {
			next := 0
			for i, c := range s.Choices {
				if c == m.input.Value() {
					next = (i + 1) % len(s.Choices)
				}
			}
			m.input.SetValue(s.Choices[next])
			m.input.CursorEnd()
		}
		return m, nil
	case "enter":
		v := strings.TrimSpace(m.input.Value())
		if err := s.validate(v); err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		// A blank secret keeps what is there; X on the review
		// screen is how one is cleared.
		if v != "" || s.Kind != kindSecret {
			if v == m.a.existingEnv[s.Key] {
				m.a.undoValue(s.Key)
			} else {
				m.a.setValue(s.Key, v)
			}
		}
		return m, m.enterReview()
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// sourceStyle colors the status column.
func sourceStyle(o origin) lipgloss.Style {
	switch o {
	case orgNew, orgGenerated:
		return okStyle
	case orgSkipped:
		return errStyle
	}
	return mutedStyle
}

// reviewView renders the whole review screen.
func (m model) reviewView() string {
	w, _ := m.size()
	bw := m.bodyWidth()
	items := m.reviewItems()
	cursor := max(0, min(m.cursor, len(items)-1))
	blockers, warnings := m.a.reviewProblems()

	var parts []string
	if l, ok := m.reviewHeader(); ok {
		parts = append(parts, center(w, renderHeader(l, m.frame)), "")
	}

	changes := len(m.a.finalEdits())
	title := titleStyle.Render("Review your settings")
	summary := mutedStyle.Render(fmt.Sprintf("%d to write", changes))
	if m.showAll {
		summary += mutedStyle.Render(", showing all")
	}
	gap := max(1, bw-lipgloss.Width(title)-lipgloss.Width(summary))
	parts = append(parts, center(w, title+strings.Repeat(" ", gap)+summary))

	switch {
	case m.filtering || m.filter.Value() != "":
		parts = append(parts, center(w, lipgloss.NewStyle().Width(bw).Render(m.filter.View())))
	default:
		parts = append(parts, center(w, lipgloss.NewStyle().Width(bw).Render(mutedStyle.Render("Detected: "+m.detectedSummary()))))
	}
	parts = append(parts, "")

	// The list.
	lines := reviewLines(items)
	rows := m.listRows()
	start := max(0, min(m.scroll, max(0, len(lines)-rows)))
	end := min(len(lines), start+rows)
	keyW := min(32, max(16, bw*2/5))
	statusW := 15
	if bw < 64 {
		statusW = 0
	}
	valueW := max(8, bw-keyW-statusW-4)
	var list []string
	for _, l := range lines[start:end] {
		switch {
		case l.item < 0 && l.heading != "":
			list = append(list, titleStyle.Render(strings.ToUpper(l.heading)))
		case l.item < 0:
			list = append(list, "")
		default:
			list = append(list, m.reviewRowLine(items[l.item], l.item == cursor, keyW, valueW, statusW))
		}
	}
	// A heading never sits alone at the bottom of the window.
	if end < len(lines) && lines[end-1].item < 0 && len(list) > 0 {
		list[len(list)-1] = ""
	}
	for len(list) < rows {
		list = append(list, "")
	}
	parts = append(parts, center(w, lipgloss.NewStyle().Width(bw).Render(strings.Join(list, "\n"))))

	// Detail of the selected row.
	cur := items[cursor]
	detail := cur.help
	if isInstallRow(cur) {
		detail = "Writes these settings, sets up the database and service, and starts EveSynapse."
		if m.dryRun {
			detail = "Dry run: nothing will be installed."
		}
	}
	scroll := ""
	if len(lines) > rows {
		scroll = mutedStyle.Render(fmt.Sprintf("  %d of %d", cursor+1, len(items)))
	}
	wrap := lipgloss.NewStyle().Width(bw)
	parts = append(parts, center(w, wrap.Render(mutedStyle.Render(strings.Repeat("─", max(0, bw-lipgloss.Width(scroll))))+scroll)))
	parts = append(parts, center(w, wrap.Render(clipLines(wrap.Render(detail), 2))))
	if lipgloss.Height(wrap.Render(detail)) < 2 {
		parts = append(parts, "")
	}

	// Problems, blockers first.
	shown := 0
	for _, p := range blockers {
		if shown < 3 {
			parts = append(parts, center(w, wrap.Render(errStyle.Render(truncateCells("! "+p, bw)))))
			shown++
		}
	}
	for _, p := range warnings {
		if shown < 3 {
			parts = append(parts, center(w, wrap.Render(warnStyle.Render(truncateCells("· "+p, bw)))))
			shown++
		}
	}
	if m.notice != "" {
		parts = append(parts, center(w, wrap.Render(okStyle.Render(truncateCells(m.notice, bw)))))
	} else {
		parts = append(parts, "")
	}
	parts = append(parts, center(w, wrap.Render(mutedStyle.Render(m.reviewHints(cur)))))
	return strings.Join(parts, "\n")
}

func (m model) detectedSummary() string {
	pg, env := "not installed", "new install"
	if m.a.hasLocalPG {
		pg = "installed"
	}
	if m.a.existingEnvAt != "" {
		env = "keeping " + m.a.existingEnvAt
	}
	return truncateCells(m.a.arch+", PostgreSQL "+pg+", "+env, m.bodyWidth()-10)
}

func (m model) reviewHints(cur reviewRow) string {
	if m.filtering {
		return "Type to filter, Enter to keep it, Esc to clear it."
	}
	h := "Enter edit  / filter  Tab group  A all  U undo  X clear  I install  Q quit"
	if s, ok := settingFor(cur.key); ok && s.Generate > 0 {
		h = "G generate  " + h
	}
	if m.bodyWidth() < lipgloss.Width(h) {
		h = "Enter edit  / filter  A all  I install  Q quit"
	}
	return h
}

// reviewRowLine renders one setting.
func (m model) reviewRowLine(r reviewRow, selected bool, keyW, valueW, statusW int) string {
	mark := "  "
	if selected {
		mark = titleStyle.Render("> ")
	}
	if isInstallRow(r) {
		label := r.setting
		if blockers, _ := m.a.reviewProblems(); len(blockers) > 0 {
			return mark + errStyle.Render(label+" (fix the problems below first)")
		}
		if selected {
			return mark + titleStyle.Render(label)
		}
		return mark + okStyle.Render(label)
	}
	key := fmt.Sprintf("%-*s", keyW, truncateCells(r.setting, keyW))
	if selected {
		key = titleStyle.Render(key)
	}
	val := truncateCells(r.value, valueW)
	pad := strings.Repeat(" ", max(0, valueW-lipgloss.Width(val)))
	if !r.isSet {
		val = mutedStyle.Render(val)
	}
	line := mark + key + " " + val + pad
	if statusW > 0 {
		line += " " + sourceStyle(r.source).Render(r.source.String())
	}
	return line
}
