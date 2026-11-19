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
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF4500"))
	boltStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4500"))
	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
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
	stageContact
	stageReview
	stageInstall
	stageDone
	stageFailed
)

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
	a       *answers
	dryRun  bool
	stage   stage
	frame   int
	input   textinput.Model
	choice  int // selected option on choice stages
	errMsg  string
	review  table.Model
	bar     progress.Model
	// download state
	dlAttempts []providerAttempt
	dlPct      float64
	dlErr      error
	dlCh       chan tea.Msg
	// install state
	step      int
	totalSteps int
	failedAt  string
	failOut   string
	installCh chan tea.Msg
	quitting  bool
	// returnToReview is set when a prompt was entered from the
	// review screen: finishing it goes back to the review.
	returnToReview bool
}

func newModel(a *answers, dryRun bool) model {
	bar := progress.New(progress.WithSolidFill("#FF4500"))
	bar.Width = 60
	return model{a: a, dryRun: dryRun, bar: bar, choice: 0}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) }),
		func() tea.Msg { return startMsg{} },
	)
}

// startMsg kicks the flow off: the first stage needing input.
type startMsg struct{}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case startMsg:
		return m, m.advanceFrom(stageSource - 1) // start at the first stage
	case tea.KeyMsg:
		// Keys the stage itself consumes go to handleKey; the
		// rest go to the input or the table on this screen.
		if m.stage == stageReview {
			switch msg.String() {
			case "enter", "q", "ctrl+c":
				return m.handleKey(msg)
			default:
				var cmd tea.Cmd
				m.review, cmd = m.review.Update(msg)
				return m, cmd
			}
		}
		if m.isPromptStage() && msg.String() != "enter" && msg.String() != "esc" && msg.String() != "ctrl+c" {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		return m.handleKey(msg)
	case tickMsg:
		m.frame = (m.frame + 1) % len(boltFrames)
		return m, tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
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
	// The review table gets the other messages on the review stage.
	if m.stage == stageReview {
		var cmd tea.Cmd
		m.review, cmd = m.review.Update(msg)
		return m, cmd
	}
	return m, nil
}

// isPromptStage reports whether the current stage takes text input.
func (m *model) isPromptStage() bool {
	switch m.stage {
	case stageSourceDir, stageDBURL, stageClientID, stageClientSecret, stageContact:
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
		return !a.given["databaseURL"]
	case stageDBURL:
		if a.dbLocal {
			return false
		}
		if a.databaseURL == "present" {
			return false // the existing URL stays unless changed
		}
		return !a.given["databaseURL"]
	case stageClientID:
		return !a.given["clientID"]
	case stageClientSecret:
		return !a.given["clientSecret"]
	case stageContact:
		return !a.given["contact"]
	}
	return true
}

// stageOrder is the prompt order. The review is not in it: it
// is where the prompts end, reached by advanceFrom's fallthrough.
var stageOrder = []stage{
	stageSource, stageSourceDir, stageDownload, stageDB, stageDBURL,
	stageClientID, stageClientSecret, stageContact,
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
	case stageContact:
		m.startPrompt(s, "Contact email", a.contact, false)
	}
	return nil
}

// enterReview builds the review table and shows it.
func (m *model) enterReview() tea.Cmd {
	m.stage = stageReview
	rows := m.a.reviewRows()
	cols := []table.Column{
		{Title: "Setting", Width: 22},
		{Title: "Value", Width: 52},
		{Title: "Source", Width: 14},
	}
	var trows []table.Row
	for _, r := range rows {
		trows = append(trows, table.Row{r.setting, r.value, r.source.String()})
	}
	trows = append(trows, table.Row{"Install EveSynapse", "", ""})
	t := table.New(
		table.WithColumns(cols),
		table.WithRows(trows),
		table.WithFocused(true),
		table.WithHeight(len(trows)+1),
	)
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Bold(true)
	styles.Selected = styles.Selected.Foreground(lipgloss.Color("#FF4500")).Bold(true)
	t.SetStyles(styles)
	m.review = t
	return nil
}

// editRow jumps from the review to the stage that edits a row.
func (m *model) editRow(row int) tea.Cmd {
	rows := m.a.reviewRows()
	if row < 0 || row >= len(rows) {
		return nil
	}
	edit := rows[row].edit
	if edit == "" {
		return nil
	}
	m.returnToReview = true
	switch edit {
	case "source":
		m.a.given["source"] = false
		return m.enterStage(stageSource)
	case "db":
		m.a.given["databaseURL"] = false
		if isLinux() {
			return m.enterStage(stageDB)
		}
		return m.enterStage(stageDBURL)
	case "clientid":
		m.a.given["clientID"] = false
		return m.enterStage(stageClientID)
	case "clientsecret":
		m.a.given["clientSecret"] = false
		return m.enterStage(stageClientSecret)
	case "contact":
		m.a.given["contact"] = false
		return m.enterStage(stageContact)
	}
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

	case stageDBURL, stageClientID, stageClientSecret, stageContact:
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

	case stageReview:
		switch msg.String() {
		case "enter":
			row := m.review.Cursor()
			rows := len(m.a.reviewRows())
			if row == rows {
				// Confirm: install, or stop here on a dry run.
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
			return m, m.editRow(row)
		case "q":
			m.quitting = true
			return m, tea.Quit
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
	ti.Width = 60
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

// boltHeader renders the spinning bolt, the title, the tagline,
// and the credits.
func (m model) boltHeader() string {
	var b strings.Builder
	for _, ln := range boltFrames[m.frame] {
		b.WriteString(boltStyle.Render(ln) + "\n")
	}
	b.WriteString("\n" + titleStyle.Render("  EVESYNAPSE") + "\n")
	b.WriteString("  " + mutedStyle.Render(tagline) + "\n")
	b.WriteString("  " + mutedStyle.Render(creditsLine) + "\n")
	return b.String()
}

// errLine renders the inline validation error, when there is one.
func (m model) errLine() string {
	if m.errMsg == "" {
		return ""
	}
	return "\n" + errStyle.Render(m.errMsg) + "\n"
}

// View renders the current screen.
func (m model) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder
	b.WriteString(m.boltHeader())
	b.WriteString("\n")

	switch m.stage {
	case stageSource:
		b.WriteString(titleStyle.Render("How should EveSynapse be installed?") + "\n\n")
		opts := []string{
			"Build from source, from a checkout on this machine",
			"Download the latest release (GitHub, then GitLab, then natems.dev)",
		}
		for i, o := range opts {
			mark := "  "
			if i == m.choice {
				mark = titleStyle.Render("> ")
			}
			b.WriteString(mark + o + "\n")
		}
		b.WriteString("\n" + mutedStyle.Render("Up and down to choose, Enter to continue, Ctrl+C to quit.") + "\n")

	case stageSourceDir:
		b.WriteString(titleStyle.Render("Where is the EveSynapse checkout?") + "\n")
		if detectSourceDir() != "" {
			b.WriteString("Found one at " + detectSourceDir() + "; change it below if that is not the right one.\n\n")
		} else {
			b.WriteString("The checkout holds the source the install builds from.\n\n")
		}
		b.WriteString(m.input.View() + "\n")
		b.WriteString(m.errLine())
		b.WriteString("\n" + mutedStyle.Render("Enter to continue, Esc to go back.") + "\n")

	case stageDownload:
		if m.dlErr != nil {
			b.WriteString(errStyle.Render("The latest release could not be fetched.") + "\n\n")
			b.WriteString(m.dlErr.Error() + "\n\n")
			b.WriteString("Download it by hand from one of these pages:\n")
			for _, p := range providerDownloadPages {
				b.WriteString("  " + p + "\n")
			}
			b.WriteString("\n" + mutedStyle.Render("Enter to choose a different source, Q to quit.") + "\n")
			break
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
				b.WriteString("  " + m.bar.View() + "\n")
			}
		} else {
			b.WriteString("\n" + okStyle.Render("Release v"+m.a.release.Version+" from "+providerDisplayName(m.a.release.Provider)+".") + "\n")
		}
		b.WriteString("\n" + mutedStyle.Render("One provider failing just tries the next.") + "\n")

	case stageDB:
		b.WriteString(titleStyle.Render("Where should EveSynapse keep its data?") + "\n\n")
		opts := []string{
			"Set up PostgreSQL on this machine (recommended)",
			"Use a PostgreSQL database I already have",
		}
		if m.a.hasLocalPG {
			opts[0] += " (already installed)"
		}
		for i, o := range opts {
			mark := "  "
			if i == m.choice {
				mark = titleStyle.Render("> ")
			}
			b.WriteString(mark + o + "\n")
		}
		b.WriteString("\n" + mutedStyle.Render("Up and down to choose, Enter to continue, Ctrl+C to quit.") + "\n")

	case stageDBURL:
		if !isLinux() {
			b.WriteString(titleStyle.Render("What is the connection string for your database?") + "\n")
			b.WriteString("Local provisioning needs a Linux machine, so give the\n")
			b.WriteString("DATABASE_URL of a PostgreSQL you already have, or leave\n")
			b.WriteString("it blank to add it later (the app will not start without one).\n\n")
		} else {
			b.WriteString(titleStyle.Render("What is the connection string for your database?") + "\n")
			b.WriteString("It looks like postgres://user:password@host:5432/evesynapse\n\n")
		}
		b.WriteString(m.input.View() + "\n")
		b.WriteString(m.errLine())
		b.WriteString("\n" + mutedStyle.Render("Enter to continue, Esc to go back.") + "\n")

	case stageClientID:
		b.WriteString(titleStyle.Render("What is your EVE Online application client ID?") + "\n")
		b.WriteString("Find it at developers.eveonline.com, on your application page.\n")
		b.WriteString("Leave it blank to add it later; sign-in stays off until then.\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString(m.errLine())
		b.WriteString("\n" + mutedStyle.Render("Enter to continue, Esc to go back.") + "\n")

	case stageClientSecret:
		b.WriteString(titleStyle.Render("What is your EVE Online application client secret?") + "\n")
		b.WriteString("It is on the same application page as the client ID.\n")
		b.WriteString("Leave it blank to add it later; sign-in stays off until then.\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString(m.errLine())
		b.WriteString("\n" + mutedStyle.Render("Enter to continue, Esc to go back.") + "\n")

	case stageContact:
		b.WriteString(titleStyle.Render("What email should CCP see as the contact for your EVE application?") + "\n")
		b.WriteString("CCP uses it to reach you about your application. Leave it blank\n")
		b.WriteString("to skip it.\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString(m.errLine())
		b.WriteString("\n" + mutedStyle.Render("Enter to continue, Esc to go back.") + "\n")

	case stageReview:
		b.WriteString(titleStyle.Render("Here is what the install will use.") + "\n")
		b.WriteString("Move to a line and press Enter to change it, or choose Install to go ahead.\n\n")
		b.WriteString(mutedStyle.Render("Detected on this machine:") + "\n")
		for _, d := range m.a.detectedLines() {
			b.WriteString("  " + d[0] + ": " + d[1] + "\n")
		}
		b.WriteString("\n")
		b.WriteString(m.review.View() + "\n")
		b.WriteString("\n" + mutedStyle.Render("Up and down to move, Enter to change or install, Q to quit.") + "\n")
		if m.dryRun {
			b.WriteString(mutedStyle.Render("(Dry run: nothing will be installed.)") + "\n")
		}

	case stageInstall:
		b.WriteString(titleStyle.Render("Installing EveSynapse...") + "\n\n")
		steps := installStepsFor(m.a)
		for i, s := range steps {
			switch {
			case i < m.step:
				b.WriteString("  " + okStyle.Render("[x]") + " " + s.name + "\n")
			case i == m.step:
				b.WriteString("  " + titleStyle.Render("[*]") + " " + s.name + "...\n")
			default:
				b.WriteString("  [ ] " + s.name + "\n")
			}
		}
		b.WriteString("\n" + m.bar.View() + "\n")
		b.WriteString("\n" + mutedStyle.Render("Please wait, this takes a few minutes.") + "\n")

	case stageDone:
		b.WriteString(titleStyle.Render("Done. EveSynapse is listening on " + m.a.siteURL() + ".") + "\n\n")
		b.WriteString(m.doneNotes())
		b.WriteString("\n" + mutedStyle.Render(creditsLine) + "\n")
		b.WriteString("\n" + mutedStyle.Render("Press Enter to finish.") + "\n")

	case stageFailed:
		b.WriteString(errStyle.Render("The install stopped at: "+m.failedAt) + "\n\n")
		b.WriteString("What the installer said:\n---\n")
		b.WriteString(lastLines(m.failOut, 30))
		b.WriteString("---\n")
		b.WriteString("\n" + mutedStyle.Render("Press Enter to finish.") + "\n")
	}
	return b.String()
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
	b.WriteString("  nginx, or a Cloudflare Tunnel at 127.0.0.1:8080 to reach it\n")
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
	p := tea.NewProgram(newModel(a, dryRun))
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "installer:", err)
		os.Exit(1)
	}
}
