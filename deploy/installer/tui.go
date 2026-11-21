// Full-screen interface (Bubble Tea): the prompts, the review
// table, the download, and the install with its progress bar.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF4500"))
	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("247"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

type stage int

const (
	stageSource stage = iota
	stageSourceDir
	stageDownload
	stageDB
	stageDBURL
	stageClientID
	stageClientSecret
	stageCallback
	stageContact
	stageReview
	stageEdit
	stageInstall
	stageDone
	stageFailed
)

// frameEvery paces the bolt: one turn takes about 2.4 seconds.
const frameEvery = 66 * time.Millisecond

type tickMsg time.Time
type stepMsg int // install advanced to this many steps done
type installDoneMsg struct {
	err    error
	output string // install output, kept for the failure screen
}
type frameMsg struct{}
type downloadAttemptMsg struct{ att providerAttempt }
type downloadProgressMsg struct{ pct float64 }
type downloadDoneMsg struct {
	rel        *releaseInfo
	binaryPath string
	tmpDir     string
	err        error
}

// model is the installer interface.
type model struct {
	a             *answers
	dryRun        bool
	stage         stage
	frame         int
	input         textinput.Model
	choice        int // selected option on choice stages
	errMsg        string
	width, height int
	// review state
	cursor    int  // index into reviewItems
	scroll    int  // first visible list line
	showAll   bool // list every setting, not only the important ones
	filtering bool // the filter box has focus
	filter    textinput.Model
	editKey   string // the setting open on stageEdit
	notice    string // one-line result of the last review action
	bar       progress.Model
	// download state
	dlAttempts []providerAttempt
	dlPct      float64
	dlErr      error
	dlCh       chan tea.Msg
	// install state
	step       int
	totalSteps int
	failedAt   string
	failOut    string
	installCh  chan tea.Msg
	quitting   bool
	// returnToReview is set when a prompt was entered from the
	// review screen: finishing it goes back to the review.
	returnToReview bool
}

func newModel(a *answers, dryRun bool) model {
	bar := progress.New(progress.WithSolidFill("#FF4500"))
	bar.Width = 60
	filter := textinput.New()
	filter.Prompt = "/ "
	filter.Placeholder = "filter settings"
	return model{a: a, dryRun: dryRun, bar: bar, choice: 0, filter: filter}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		tea.Tick(frameEvery, func(t time.Time) tea.Msg { return tickMsg(t) }),
		func() tea.Msg { return startMsg{} },
	)
}

// startMsg kicks the flow off: the first stage needing input.
type startMsg struct{}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case startMsg:
		return m, m.advanceFrom(stageSource - 1) // start at the first stage
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.keepCursorVisible()
		return m, nil
	case tea.KeyMsg:
		if m.stage == stageReview {
			return m.handleReviewKey(msg)
		}
		if m.stage == stageEdit {
			return m.handleEditKey(msg)
		}
		// Keys the stage itself consumes go to handleKey; the
		// rest go to the input on this screen.
		if m.isPromptStage() && msg.String() != "enter" && msg.String() != "esc" && msg.String() != "ctrl+c" {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		return m.handleKey(msg)
	case tickMsg:
		m.frame = (m.frame + 1) % boltFrameCount
		return m, tea.Tick(frameEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
	case downloadAttemptMsg:
		m.dlAttempts = append(m.dlAttempts, msg.att)
		return m, m.waitDownload
	case downloadProgressMsg:
		m.dlPct = msg.pct
		return m, tea.Batch(m.waitDownload, m.bar.SetPercent(msg.pct))
	case downloadDoneMsg:
		if msg.err != nil {
			m.dlErr = msg.err
			return m, nil
		}
		m.a.release = msg.rel
		m.a.downloadedBinary = msg.binaryPath
		m.a.downloadDir = msg.tmpDir
		return m, m.advanceFrom(stageDownload)
	case stepMsg:
		if int(msg) > m.step {
			m.step = int(msg)
		}
		pct := 0.0
		if m.totalSteps > 0 {
			pct = float64(m.step) / float64(m.totalSteps)
		}
		return m, tea.Batch(m.waitInstall, m.bar.SetPercent(pct))
	case installDoneMsg:
		if msg.err != nil {
			m.stage = stageFailed
			m.failOut = msg.output
			if m.step < m.totalSteps {
				m.failedAt = installStepsFor(m.a)[m.step].name
			} else {
				m.failedAt = "Finishing up"
			}
		} else {
			m.stage = stageDone
			m.step = m.totalSteps
		}
		return m, m.bar.SetPercent(1)
	case progress.FrameMsg:
		updated, cmd := m.bar.Update(msg)
		m.bar = updated.(progress.Model)
		return m, cmd
	}

	// Text input gets the other messages on prompt stages.
	if m.isPromptStage() {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	if m.stage == stageEdit {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	if m.stage == stageReview && m.filtering {
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		return m, cmd
	}
	return m, nil
}

// isPromptStage reports whether the current stage takes text input.
func (m *model) isPromptStage() bool {
	switch m.stage {
	case stageSourceDir, stageDBURL, stageClientID, stageClientSecret, stageCallback, stageContact:
		return true
	}
	return false
}

// needsStage reports whether a prompt stage has anything to ask:
// a value given by flag or environment is already answered.
func (m *model) needsStage(s stage) bool {
	a := m.a
	switch s {
	case stageSource:
		return !a.given["source"]
	case stageSourceDir:
		if a.source != sourceBuild {
			return false
		}
		return !(a.given["sourceDir"] && validateSourceDir(a.sourceDir) == nil)
	case stageDownload:
		return a.source == sourceDownload && a.release == nil
	case stageDB:
		if !isLinux() {
			return false // elsewhere the URL is asked directly
		}
		return !a.given["databaseURL"] && a.databaseURL != "present"
	case stageDBURL:
		if a.dbLocal {
			return false
		}
		if a.databaseURL == "present" {
			return false // the existing URL stays unless changed
		}
		return !a.given["databaseURL"]
	case stageClientID:
		return !a.given["clientID"] && a.existingEnv["EVE_CLIENT_ID"] == ""
	case stageClientSecret:
		return !a.given["clientSecret"] && a.existingEnv["EVE_CLIENT_SECRET"] == ""
	case stageCallback:
		return !a.given["callback"] && a.existingEnv["EVE_CALLBACK_URL"] == ""
	case stageContact:
		return !a.given["contact"] && a.existingEnv["ESI_CONTACT"] == ""
	}
	return true
}

// stageOrder is the prompt order. The review is not in it: it
// is where the prompts end, reached by advanceFrom's fallthrough.
var stageOrder = []stage{
	stageSource, stageSourceDir, stageDownload, stageDB, stageDBURL,
	stageClientID, stageClientSecret, stageCallback, stageContact,
}

// advanceFrom moves to the next stage after s that has anything
// to ask, or to the review when the prompts are done. It returns
// the command the new stage needs, if any.
func (m *model) advanceFrom(s stage) tea.Cmd {
	if m.returnToReview {
		m.returnToReview = false
		return m.enterReview()
	}
	idx := -1
	for i, st := range stageOrder {
		if st == s {
			idx = i
			break
		}
	}
	for i := idx + 1; i < len(stageOrder); i++ {
		if m.needsStage(stageOrder[i]) {
			return m.enterStage(stageOrder[i])
		}
	}
	return m.enterReview()
}

// enterStage sets up a stage for display. It returns the command
// the stage needs, if any.
func (m *model) enterStage(s stage) tea.Cmd {
	m.stage = s
	m.errMsg = ""
	a := m.a
	switch s {
	case stageSource:
		// Default to building when a checkout is here,
		// downloading when it is not.
		if detectSourceDir() != "" {
			m.choice = 0
		} else {
			m.choice = 1
		}
	case stageSourceDir:
		detected := detectSourceDir()
		prefill := a.sourceDir
		if prefill == "" {
			prefill = detected
		}
		if prefill == "" {
			m.errMsg = "No checkout found in the usual places (this directory, ~/evesynapse)."
		} else {
			a.orgSourceDir = orgDetected
		}
		m.startPrompt(s, "Folder holding the EveSynapse checkout", prefill, false)
	case stageDownload:
		m.dlAttempts = nil
		m.dlPct = 0
		m.dlErr = nil
		return m.startDownloadCmd()
	case stageDB:
		if a.dbLocal {
			m.choice = 0
		} else {
			m.choice = 1
		}
	case stageDBURL:
		if !isLinux() {
			m.startPrompt(s, "postgres://user:password@host:5432/evesynapse", "", false)
			return nil
		}
		m.startPrompt(s, "postgres://user:password@host:5432/evesynapse", a.databaseURL, false)
	case stageClientID:
		m.startPrompt(s, "EVE client ID", a.clientID, false)
	case stageClientSecret:
		m.startPrompt(s, "EVE client secret", a.clientSecret, true)
	case stageCallback:
		v := a.callback
		if v == "" {
			cb, _ := settingFor("EVE_CALLBACK_URL")
			v = cb.Default
		}
		m.startPrompt(s, "https://your-host/auth/callback", v, false)
	case stageContact:
		m.startPrompt(s, "Contact email", a.contact, false)
	}
	return nil
}

// enterReview shows the settings review.
func (m *model) enterReview() tea.Cmd {
	m.stage = stageReview
	m.errMsg = ""
	m.filtering = false
	m.filter.Blur()
	m.clampCursor()
	m.keepCursorVisible()
	return nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		m.quitting = true
		return m, tea.Quit
	}

	switch m.stage {
	case stageSource:
		switch msg.String() {
		case "up", "k":
			if m.choice > 0 {
				m.choice--
			}
		case "down", "j":
			if m.choice < 1 {
				m.choice++
			}
		case "enter":
			if m.choice == 0 {
				m.a.source = sourceBuild
			} else {
				m.a.source = sourceDownload
			}
			return m, m.advanceFrom(stageSource)
		}
		return m, nil

	case stageSourceDir:
		if msg.String() == "enter" {
			dir := expandPath(strings.TrimSpace(m.input.Value()))
			if err := validateSourceDir(dir); err != nil {
				m.errMsg = err.Error()
				return m, nil
			}
			m.a.sourceDir = dir
			m.a.orgSourceDir = orgNew
			return m, m.advanceFrom(stageSourceDir)
		}
		if msg.String() == "esc" {
			return m, m.advanceFrom(stageSource - 1)
		}
		return m, nil

	case stageDownload:
		if m.dlErr != nil {
			switch msg.String() {
			case "enter":
				m.a.source = ""
				m.a.given["source"] = false
				return m, m.enterStage(stageSource)
			case "q":
				m.quitting = true
				return m, tea.Quit
			}
		}
		return m, nil

	case stageDB:
		switch msg.String() {
		case "up", "k":
			if m.choice > 0 {
				m.choice--
			}
		case "down", "j":
			if m.choice < 1 {
				m.choice++
			}
		case "enter":
			m.a.dbLocal = m.choice == 0
			if m.a.dbLocal {
				m.a.databaseURL = ""
			}
			return m, m.advanceFrom(stageDB)
		}
		return m, nil

	case stageDBURL, stageClientID, stageClientSecret, stageCallback, stageContact:
		if msg.String() == "enter" {
			v := strings.TrimSpace(m.input.Value())
			switch m.stage {
			case stageDBURL:
				if v != "" && !strings.HasPrefix(v, "postgres://") && !strings.HasPrefix(v, "postgresql://") {
					m.errMsg = "That does not look like a connection string. It should start with postgres://"
					return m, nil
				}
				if v == "" && isLinux() {
					m.errMsg = "A connection string is needed for a database you already have."
					return m, nil
				}
				if v != "" {
					m.a.dbLocal = false
					m.a.databaseURL = v
					m.a.orgDB = orgNew
				}
				// Blank elsewhere means "add it later": the
				// app will not start without one, and the done
				// screen says so.
				return m, m.advanceFrom(stageDBURL)
			case stageClientID:
				m.a.clientID = v
				if v == "" {
					m.a.orgClientID = orgSkipped
				} else {
					m.a.orgClientID = orgNew
				}
				return m, m.advanceFrom(stageClientID)
			case stageClientSecret:
				m.a.clientSecret = v
				if v == "" {
					m.a.orgClientSecret = orgSkipped
				} else {
					m.a.orgClientSecret = orgNew
				}
				return m, m.advanceFrom(stageClientSecret)
			case stageCallback:
				cb, _ := settingFor("EVE_CALLBACK_URL")
				if err := cb.validate(v); err != nil {
					m.errMsg = err.Error()
					return m, nil
				}
				m.a.callback = v
				m.a.orgCallback = orgNew
				return m, m.advanceFrom(stageCallback)
			case stageContact:
				m.a.contact = v
				if v == "" {
					m.a.orgContact = orgSkipped
				} else {
					m.a.orgContact = orgNew
				}
				return m, m.advanceFrom(stageContact)
			}
			return m, nil
		}
		if msg.String() == "esc" {
			return m, m.advanceFrom(stageSource - 1)
		}
		return m, nil

	case stageDone, stageFailed:
		if msg.String() == "enter" || msg.String() == "q" {
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil
	}
	return m, nil
}

// startPrompt opens a text prompt for one value.
func (m *model) startPrompt(s stage, placeholder, value string, masked bool) {
	m.stage = s
	ti := textinput.New()
	ti.Focus()
	ti.Width = min(60, max(20, m.bodyWidth()-6))
	ti.Placeholder = placeholder
	ti.SetValue(value)
	if masked {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '*'
	}
	m.input = ti
}

// startDownloadCmd fetches the latest release (and, unless this
// is a dry run, its binary) in the background, reporting each
// provider it tries and the download's progress. It returns the
// command that waits for the first message.
func (m *model) startDownloadCmd() tea.Cmd {
	ch := make(chan tea.Msg, 16)
	go func() {
		defer close(ch)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		rel, err := fetchLatestRelease(ctx, targetArch(), func(att providerAttempt) {
			ch <- downloadAttemptMsg{att}
		})
		if err != nil {
			ch <- downloadDoneMsg{err: err}
			return
		}
		if m.dryRun {
			ch <- downloadDoneMsg{rel: rel}
			return
		}
		tmpDir, err := os.MkdirTemp("", "evesynapse-release-*")
		if err != nil {
			ch <- downloadDoneMsg{err: err}
			return
		}
		dest := filepath.Join(tmpDir, "evesynapse-"+targetArch())
		err = downloadBinary(ctx, rel, dest, func(p downloadProgress) {
			pct := 0.0
			if p.Total > 0 {
				pct = float64(p.Done) / float64(p.Total)
			}
			ch <- downloadProgressMsg{pct: pct}
		})
		if err != nil {
			os.RemoveAll(tmpDir)
			ch <- downloadDoneMsg{err: err}
			return
		}
		ch <- downloadDoneMsg{rel: rel, binaryPath: dest, tmpDir: tmpDir}
	}()
	m.dlCh = ch
	return m.waitDownload
}

// waitDownload blocks until the download goroutine sends the
// next message.
func (m model) waitDownload() tea.Msg {
	msg, ok := <-m.dlCh
	if !ok {
		return downloadDoneMsg{err: fmt.Errorf("the download stopped unexpectedly")}
	}
	return msg
}

// runInstall starts the install in the background, sending a
// stepMsg for every step that completes and an installDoneMsg
// when it finishes.
func (m model) runInstall() (tea.Model, tea.Cmd) {
	ch := make(chan tea.Msg, 16)
	a := m.a
	go func() {
		defer close(ch)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		cb := installCallbacks{
			Step: func(done int) { ch <- stepMsg(done) },
		}
		out, err := runInstall(ctx, a, cb)
		ch <- installDoneMsg{err: err, output: out}
	}()
	m.installCh = ch
	return m, m.waitInstall
}

// waitInstall blocks until the installer goroutine sends the
// next message.
func (m model) waitInstall() tea.Msg {
	msg, ok := <-m.installCh
	if !ok {
		return installDoneMsg{}
	}
	return msg
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// size is the terminal size, with a default until it is known.
func (m model) size() (int, int) {
	if m.width <= 0 || m.height <= 0 {
		return defaultWidth, defaultHeight
	}
	return m.width, m.height
}

// bodyWidth is the width of the content column.
func (m model) bodyWidth() int {
	w, _ := m.size()
	return max(20, min(maxBodyWidth, w-4))
}

// promptBody lays out a question: centered title and help, the
// control as a centered block, and the key hints.
func (m model) promptBody(title string, help []string, control, hint string) string {
	w := m.bodyWidth()
	wrap := lipgloss.NewStyle().Width(w).Align(lipgloss.Center)
	parts := []string{wrap.Render(titleStyle.Render(title))}
	for _, h := range help {
		parts = append(parts, wrap.Render(h))
	}
	parts = append(parts, "", center(w, control))
	if m.errMsg != "" {
		parts = append(parts, "", wrap.Render(errStyle.Render(m.errMsg)))
	}
	parts = append(parts, "", wrap.Render(mutedStyle.Render(hint)))
	return strings.Join(parts, "\n")
}

// choiceBlock renders options with the selection marked.
func (m model) choiceBlock(opts []string) string {
	var lines []string
	for i, o := range opts {
		mark := "  "
		if i == m.choice {
			mark = titleStyle.Render("> ")
		}
		lines = append(lines, mark+truncateCells(o, m.bodyWidth()-2))
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// View renders the current screen.
func (m model) View() string {
	if m.quitting {
		return ""
	}
	w, h := m.size()
	if m.stage == stageReview {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, m.reviewView())
	}
	body := center(w, lipgloss.NewStyle().Width(m.bodyWidth()).Render(m.stageBody(h)))
	return compose(w, h, m.frame, body)
}

const (
	promptHint = "Enter to continue, Esc to go back."
	choiceHint = "Up and down to choose, Enter to continue, Ctrl+C to quit."
)

// stageBody renders everything below the header.
func (m model) stageBody(h int) string {
	var b strings.Builder
	switch m.stage {
	case stageSource:
		return m.promptBody("How should EveSynapse be installed?", nil, m.choiceBlock([]string{
			"Build from a checkout on this machine",
			"Download the latest release",
		}), choiceHint)

	case stageSourceDir:
		help := "The checkout holds the source the install builds from."
		if d := detectSourceDir(); d != "" {
			help = "Found one at " + d + ". Change it below if that is not the right one."
		}
		return m.promptBody("Where is the EveSynapse checkout?", []string{help}, m.input.View(), promptHint)

	case stageDownload:
		if m.dlErr != nil {
			b.WriteString(errStyle.Render("The latest release could not be fetched.") + "\n\n")
			b.WriteString(m.dlErr.Error() + "\n\n")
			b.WriteString("Download it by hand from one of these pages:\n")
			for _, p := range providerDownloadPages {
				b.WriteString("  " + p + "\n")
			}
			b.WriteString("\n" + mutedStyle.Render("Enter to choose a different source, Q to quit."))
			return b.String()
		}
		b.WriteString(titleStyle.Render("Downloading the latest release...") + "\n\n")
		for _, att := range m.dlAttempts {
			if att.Err != nil {
				b.WriteString("  " + providerDisplayName(att.Provider) + ": " + mutedStyle.Render("could not be reached ("+att.Err.Error()+")") + "\n")
			} else {
				b.WriteString("  " + providerDisplayName(att.Provider) + ": " + okStyle.Render("release v"+att.Version) + "\n")
			}
		}
		if m.a.release == nil {
			if len(m.dlAttempts) == 0 {
				b.WriteString("  Trying GitHub...\n")
			} else {
				b.WriteString("  " + m.barView() + "\n")
			}
		} else {
			b.WriteString("\n" + okStyle.Render("Release v"+m.a.release.Version+" from "+providerDisplayName(m.a.release.Provider)+".") + "\n")
		}
		b.WriteString("\n" + mutedStyle.Render("One provider failing just tries the next."))
		return b.String()

	case stageDB:
		opts := []string{
			"Set up PostgreSQL on this machine (recommended)",
			"Use a PostgreSQL database I already have",
		}
		if m.a.hasLocalPG {
			opts[0] += " (already installed)"
		}
		return m.promptBody("Where should EveSynapse keep its data?", nil, m.choiceBlock(opts), choiceHint)

	case stageDBURL:
		help := []string{"It looks like postgres://user:password@host:5432/evesynapse"}
		if !isLinux() {
			help = []string{"Local setup needs a Linux machine. Give the DATABASE_URL of a PostgreSQL you already have, or leave it blank to add it later. The app will not start without one."}
		}
		return m.promptBody("What is the connection string for your database?", help, m.input.View(), promptHint)

	case stageClientID:
		return m.promptBody("What is your EVE Online application client ID?", []string{
			"Find it at developers.eveonline.com, on your application page.",
			"Leave it blank to add it later. Sign-in stays off until then.",
		}, m.input.View(), promptHint)

	case stageClientSecret:
		return m.promptBody("What is your EVE Online application client secret?", []string{
			"It is on the same application page as the client ID.",
			"Leave it blank to add it later. Sign-in stays off until then.",
		}, m.input.View(), promptHint)

	case stageCallback:
		return m.promptBody("What is your EVE callback URL?", []string{
			"CCP sends the browser here after login.",
			"It must match your EVE application exactly.",
		}, m.input.View(), promptHint)

	case stageContact:
		return m.promptBody("What email should CCP see as your contact?", []string{
			"CCP uses it to reach you about your application. Leave it blank to skip it.",
		}, m.input.View(), promptHint)

	case stageEdit:
		s, _ := settingFor(m.editKey)
		help := []string{s.Help}
		hint := "Enter to save, Esc to cancel."
		if s.Kind == kindSecret {
			hint = "Enter to save, Esc to cancel. Leave blank to keep the current value."
		}
		if len(s.Choices) > 0 {
			help = append(help, "Choices: "+strings.Join(s.Choices, ", "))
			hint = "Tab to cycle the choices, Enter to save, Esc to cancel."
		}
		if s.Default != "" {
			help = append(help, "Default: "+s.Default)
		}
		return m.promptBody(s.Key, help, m.input.View(), hint)

	case stageInstall:
		b.WriteString(titleStyle.Render("Installing EveSynapse...") + "\n\n")
		for i, st := range installStepsFor(m.a) {
			switch {
			case i < m.step:
				b.WriteString("  " + okStyle.Render("[x]") + " " + st.name + "\n")
			case i == m.step:
				b.WriteString("  " + titleStyle.Render("[*]") + " " + st.name + "...\n")
			default:
				b.WriteString("  [ ] " + st.name + "\n")
			}
		}
		b.WriteString("\n" + m.barView() + "\n")
		b.WriteString("\n" + mutedStyle.Render("Please wait, this takes a few minutes."))
		return b.String()

	case stageDone:
		b.WriteString(titleStyle.Render("Done. EveSynapse is at "+m.a.siteURL()+".") + "\n\n")
		b.WriteString(m.doneNotes())
		b.WriteString("\n" + mutedStyle.Render("Press Enter to finish."))
		return b.String()

	case stageFailed:
		b.WriteString(errStyle.Render("The install stopped at: "+m.failedAt) + "\n\n")
		b.WriteString("What the installer said:\n")
		b.WriteString(lastLines(m.failOut, max(4, min(30, h-10))))
		b.WriteString("\n" + mutedStyle.Render("Press Enter to finish."))
		return b.String()
	}
	return b.String()
}

// barView renders the progress bar at the column's width.
func (m model) barView() string {
	bar := m.bar
	bar.Width = min(60, m.bodyWidth()-2)
	return bar.View()
}

// doneNotes lists what to do next, and what was skipped.
func (m *model) doneNotes() string {
	var b strings.Builder
	a := m.a
	if isLinux() {
		b.WriteString("  Settings: /opt/evesynapse/.env\n")
		b.WriteString("  Check the service: sudo systemctl status evesynapse\n")
	} else {
		b.WriteString("  The binary is at " + filepath.Join(a.installDir, exeName()) + "\n")
		b.WriteString("  Settings: " + filepath.Join(a.installDir, ".env") + "\n")
		b.WriteString("  Run it with: " + filepath.Join(a.installDir, exeName()) + "\n")
		b.WriteString("  " + linuxNote("The service and local database setup") + "\n")
	}
	b.WriteString("  Your reverse proxy is yours to arrange: point Ferron, Caddy,\n")
	b.WriteString("  nginx, or a Cloudflare Tunnel at " + strings.TrimPrefix(a.siteURL(), "http://") + " to reach it\n")
	b.WriteString("  from the internet.\n")
	if a.clientID == "" || a.clientSecret == "" {
		b.WriteString("  Sign-in stays off until you add your EVE client ID and secret\n")
		if isLinux() {
			b.WriteString("  to /opt/evesynapse/.env and restart the service.\n")
		} else {
			b.WriteString("  to the .env beside the binary and restart it.\n")
		}
	}
	if a.startWarning != "" {
		b.WriteString("  " + errStyle.Render("Warning: "+a.startWarning) + "\n")
	}
	return b.String()
}

// lastLines returns the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

func runTUI(a *answers, dryRun bool) {
	p := tea.NewProgram(newModel(a, dryRun), tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "installer:", err)
		os.Exit(1)
	}
	// The full-screen view is gone once the program exits, so the
	// outcome is printed where it stays in the scrollback.
	if m, ok := final.(model); ok {
		switch m.stage {
		case stageDone:
			fmt.Println("Done. EveSynapse is at " + a.siteURL() + ".")
			plainDoneNotes(a)
		case stageFailed:
			fmt.Fprintln(os.Stderr, "The install stopped at: "+m.failedAt)
			fmt.Fprint(os.Stderr, lastLines(m.failOut, 30))
			os.Exit(1)
		}
	}
}
