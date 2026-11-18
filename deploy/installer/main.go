// Command installer is the interactive front end for EveSynapse setup.
//
// It asks the operator for the settings the app needs, shows a review
// screen where every value can be changed, and then runs deploy/setup.sh
// to do the install, showing plain-language progress while it works.
// The bolt spins through its 3D rotation on every screen.
//
// Usage:
//   sudo ./installer            run the prompts and install
//   sudo ./installer --dry-run  run the prompts and review, then stop
//                               before running setup.sh
// When stdout is not a terminal, the prompts are plain questions on
// stdin/stdout instead of the full-screen interface.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

const tagline = "Lightweight, fast, and comprehensive EVE Online companion."

// installSteps is the plain-language progress shown while setup.sh runs.
// The step advances when a marker line appears in the script's output.
var installSteps = []struct {
	name   string
	marker string
}{
	{"Downloading EveSynapse", "Verified EveSynapse"},
	{"Installing the app", "Installed the program to"},
	{"Setting up the database", "PostgreSQL ready"},
	{"Writing your settings", "Wrote "},
	{"Starting the service", "Service installed"},
}

// answers holds everything the install needs.
type answers struct {
	dbLocal    bool   // true: provision local PostgreSQL; false: use DATABASE_URL
	databaseURL string // only when dbLocal is false
	clientID    string
	clientSecret string
	contact     string
}

// envVars returns the environment variables to pass to setup.sh.
// Only non-empty values are set, so a blank stays blank.
func (a *answers) envVars() []string {
	vars := []string{}
	if !a.dbLocal && a.databaseURL != "" {
		vars = append(vars, "EVESYNAPSE_DATABASE_URL="+a.databaseURL)
	}
	if a.clientID != "" {
		vars = append(vars, "EVESYNAPSE_EVE_CLIENT_ID="+a.clientID)
	}
	if a.clientSecret != "" {
		vars = append(vars, "EVESYNAPSE_EVE_CLIENT_SECRET="+a.clientSecret)
	}
	if a.contact != "" {
		vars = append(vars, "EVESYNAPSE_ESI_CONTACT="+a.contact)
	}
	return vars
}

// maskSecret hides all but the last 4 characters of a secret.
func maskSecret(s string) string {
	if s == "" {
		return "(blank)"
	}
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

// maskDBURL hides the password inside a postgres connection string.
func maskDBURL(u string) string {
	if u == "" {
		return "(blank)"
	}
	// postgres://user:password@host:port/db
	rest := strings.TrimPrefix(u, "postgres://")
	rest = strings.TrimPrefix(rest, "postgresql://")
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return u
	}
	cred, host := rest[:at], rest[at+1:]
	colon := strings.Index(cred, ":")
	if colon < 0 {
		return u
	}
	user := cred[:colon]
	scheme := "postgres://"
	if strings.HasPrefix(u, "postgresql://") {
		scheme = "postgresql://"
	}
	return scheme + user + ":****@" + host
}

// reviewRows lists the values that will be used, for the review screen.
// Each row is a label and a display value.
func (a *answers) reviewRows() [][2]string {
	db := "PostgreSQL on this machine (set up by the installer)"
	if !a.dbLocal {
		db = maskDBURL(a.databaseURL)
	}
	id := a.clientID
	if id == "" {
		id = "(blank, add it later)"
	}
	contact := a.contact
	if contact == "" {
		contact = "(blank)"
	}
	return [][2]string{
		{"Database", db},
		{"EVE client ID", id},
		{"EVE client secret", maskSecret(a.clientSecret)},
		{"Contact email", contact},
	}
}

// findSetupScript locates deploy/setup.sh next to this program.
func findSetupScript() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		p := filepath.Join(filepath.Dir(exe), "..", "setup.sh")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, p := range []string{"deploy/setup.sh", "../setup.sh", "setup.sh"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("could not find deploy/setup.sh next to this program")
}

func main() {
	dryRun := flag.Bool("dry-run", false, "run the prompts and review, then stop before installing")
	flag.Parse()

	if isTerminal() {
		runTUI(*dryRun)
		return
	}
	runPlain(*dryRun)
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// ---------------------------------------------------------------------------
// Full-screen interface (Bubble Tea)
// ---------------------------------------------------------------------------

type stage int

const (
	stageDB stage = iota
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
type stepMsg int    // install advanced to this step index
type installDoneMsg struct {
	err    error
	output string // setup.sh output, kept for the failure screen
}
type frameMsg struct{}

// model is the installer interface.
type model struct {
	dryRun    bool
	stage     stage
	frame     int
	input     textinput.Model
	dbChoice  int // 0 local, 1 existing
	reviewSel int // selected row on the review screen, last row is confirm
	answers   answers
	// install state
	step      int
	failedAt  string
	failOut   string
	installCh chan tea.Msg
	quitting  bool
}

func newModel(dryRun bool) model {
	ti := textinput.New()
	ti.Focus()
	ti.Width = 60
	return model{dryRun: dryRun, stage: stageDB, dbChoice: 0}
}

func (m model) Init() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tickMsg:
		m.frame = (m.frame + 1) % len(boltFrames)
		return m, tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
	case stepMsg:
		if int(msg) > m.step {
			m.step = int(msg)
		}
		return m, m.waitInstall
	case installDoneMsg:
		if msg.err != nil {
			m.stage = stageFailed
			m.failOut = msg.output
			if m.step < len(installSteps) {
				m.failedAt = installSteps[m.step].name
			} else {
				m.failedAt = "Finishing up"
			}
		} else {
			m.stage = stageDone
			m.step = len(installSteps)
		}
		return m, nil
	}

	// Text input gets the keystrokes on prompt stages.
	if m.stage == stageDBURL || m.stage == stageClientID || m.stage == stageClientSecret || m.stage == stageContact {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		m.quitting = true
		return m, tea.Quit
	}

	switch m.stage {
	case stageDB:
		switch msg.String() {
		case "up", "k":
			if m.dbChoice > 0 {
				m.dbChoice--
			}
		case "down", "j":
			if m.dbChoice < 1 {
				m.dbChoice++
			}
		case "enter":
			m.answers.dbLocal = m.dbChoice == 0
			if m.answers.dbLocal {
				m.startPrompt(stageClientID, "EVE client ID", "")
			} else {
				m.startPrompt(stageDBURL, "Database connection string", m.answers.databaseURL)
			}
		}
		return m, nil

	case stageDBURL, stageClientID, stageClientSecret, stageContact:
		if msg.String() == "enter" {
			v := strings.TrimSpace(m.input.Value())
			switch m.stage {
			case stageDBURL:
				m.answers.databaseURL = v
				m.startPrompt(stageClientID, "EVE client ID", m.answers.clientID)
			case stageClientID:
				m.answers.clientID = v
				m.startPrompt(stageClientSecret, "EVE client secret", m.answers.clientSecret)
			case stageClientSecret:
				m.answers.clientSecret = v
				m.startPrompt(stageContact, "Contact email", m.answers.contact)
			case stageContact:
				m.answers.contact = v
				m.stage = stageReview
				m.reviewSel = 0
			}
			return m, nil
		}
		if msg.String() == "esc" {
			m.stage = stageReview
			m.reviewSel = 0
			return m, nil
		}
		return m, nil

	case stageReview:
		rows := len(m.answers.reviewRows()) + 1 // values plus the confirm row
		switch msg.String() {
		case "up", "k":
			if m.reviewSel > 0 {
				m.reviewSel--
			}
		case "down", "j":
			if m.reviewSel < rows-1 {
				m.reviewSel++
			}
		case "enter":
			if m.reviewSel == rows-1 {
				// Confirm: install, or stop here on a dry run.
				if m.dryRun {
					m.quitting = true
					return m, tea.Quit
				}
				m.stage = stageInstall
				m.step = 0
				return m.runInstall()
			}
			// Edit the selected value.
			switch m.reviewSel {
			case 0:
				m.stage = stageDB
			case 1:
				m.startPrompt(stageClientID, "EVE client ID", m.answers.clientID)
			case 2:
				m.startPrompt(stageClientSecret, "EVE client secret", m.answers.clientSecret)
			case 3:
				m.startPrompt(stageContact, "Contact email", m.answers.contact)
			}
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
func (m *model) startPrompt(s stage, placeholder, value string) {
	m.stage = s
	ti := textinput.New()
	ti.Focus()
	ti.Width = 60
	ti.Placeholder = placeholder
	ti.SetValue(value)
	if s == stageClientSecret {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '*'
	}
	m.input = ti
}

// runInstall starts setup.sh with the confirmed answers and watches its
// output for the step markers. It runs in a background goroutine that
// sends a stepMsg for every step the script reaches and an
// installDoneMsg when the script exits. It returns the model with the
// channel set, plus a command that waits on it, so the steps arrive live.
func (m model) runInstall() (tea.Model, tea.Cmd) {
	ch := make(chan tea.Msg, 16)
	go func() {
		defer close(ch)
		script, err := findSetupScript()
		if err != nil {
			ch <- installDoneMsg{err: err}
			return
		}
		cmd := exec.Command("bash", script)
		cmd.Env = append(os.Environ(), m.answers.envVars()...)
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			ch <- installDoneMsg{err: err}
			return
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			ch <- installDoneMsg{err: err}
			return
		}
		var buf strings.Builder
		scanner := bufio.NewScanner(pipe)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		step := 0
		for scanner.Scan() {
			line := scanner.Text()
			buf.WriteString(line + "\n")
			for step < len(installSteps) && strings.Contains(line, installSteps[step].marker) {
				step++
				ch <- stepMsg(step)
			}
		}
		if err := cmd.Wait(); err != nil {
			ch <- installDoneMsg{err: err, output: buf.String()}
			return
		}
		ch <- installDoneMsg{}
	}()
	m.installCh = ch
	return m, m.waitInstall
}

// waitInstall blocks until the installer goroutine sends the next
// message, then hands it to the model.
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

// boltHeader renders the spinning bolt, the title, and the tagline.
// The frame advances on every tick, so the bolt turns on all screens.
func (m model) boltHeader() string {
	var b strings.Builder
	for _, ln := range boltFrames[m.frame] {
		b.WriteString(ln + "\n")
	}
	b.WriteString("\n  EVESYNAPSE\n  " + tagline + "\n")
	return b.String()
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
	case stageDB:
		b.WriteString("Where should EveSynapse keep its data?\n\n")
		opts := []string{
			"Set up PostgreSQL on this machine (recommended)",
			"Use a PostgreSQL database I already have",
		}
		for i, o := range opts {
			mark := "  "
			if i == m.dbChoice {
				mark = "> "
			}
			b.WriteString(mark + o + "\n")
		}
		b.WriteString("\nUp and down to choose, Enter to continue, Ctrl+C to quit.\n")

	case stageDBURL:
		b.WriteString("What is the connection string for your database?\n")
		b.WriteString("It looks like postgres://user:password@host:5432/evesynapse\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString("\nEnter to continue, Esc to go back.\n")

	case stageClientID:
		b.WriteString("What is your EVE Online application client ID?\n")
		b.WriteString("Find it at developers.eveonline.com, on your application page.\n")
		b.WriteString("Leave it blank to add it later.\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString("\nEnter to continue, Esc to go back.\n")

	case stageClientSecret:
		b.WriteString("What is your EVE Online application client secret?\n")
		b.WriteString("It is on the same application page as the client ID.\n")
		b.WriteString("Leave it blank to add it later.\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString("\nEnter to continue, Esc to go back.\n")

	case stageContact:
		b.WriteString("What email should CCP see as the contact for your EVE application?\n")
		b.WriteString("CCP uses it to reach you about your application. Leave it blank\n")
		b.WriteString("to skip it.\n\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString("\nEnter to continue, Esc to go back.\n")

	case stageReview:
		b.WriteString("Here is what the install will use. Move to a line and press\n")
		b.WriteString("Enter to change it, or choose Install to go ahead.\n\n")
		rows := m.answers.reviewRows()
		for i, r := range rows {
			mark := "  "
			if i == m.reviewSel {
				mark = "> "
			}
			b.WriteString(mark + r[0] + ": " + r[1] + "\n")
		}
		mark := "  "
		if m.reviewSel == len(rows) {
			mark = "> "
		}
		b.WriteString("\n" + mark + "Install EveSynapse\n")
		b.WriteString("\nUp and down to move, Enter to change or install, Q to quit.\n")
		if m.dryRun {
			b.WriteString("(Dry run: nothing will be installed.)\n")
		}

	case stageInstall:
		b.WriteString("Installing EveSynapse...\n\n")
		for i, s := range installSteps {
			switch {
			case i < m.step:
				b.WriteString("  [x] " + s.name + "\n")
			case i == m.step:
				b.WriteString("  [*] " + s.name + "...\n")
			default:
				b.WriteString("  [ ] " + s.name + "\n")
			}
		}
		b.WriteString("\nPlease wait, this takes a few minutes.\n")

	case stageDone:
		b.WriteString("Done. Your site is ready.\n\n")
		for _, s := range installSteps {
			b.WriteString("  [x] " + s.name + "\n")
		}
		b.WriteString("\nPress Enter to finish.\n")

	case stageFailed:
		b.WriteString("The install stopped at: " + m.failedAt + "\n\n")
		b.WriteString("What the installer said:\n")
		b.WriteString("---\n")
		b.WriteString(m.failOut)
		b.WriteString("---\n")
		b.WriteString("\nPress Enter to finish.\n")
	}
	return b.String()
}

func runTUI(dryRun bool) {
	p := tea.NewProgram(newModel(dryRun))
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "installer:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Plain interface (no terminal): prompts on stdin/stdout
// ---------------------------------------------------------------------------

func runPlain(dryRun bool) {
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	// Static bolt, then the prompts.
	for _, ln := range boltFrames[0] {
		fmt.Fprintln(out, ln)
	}
	fmt.Fprintln(out, "\n  EVESYNAPSE")
	fmt.Fprintln(out, "  "+tagline)
	fmt.Fprintln(out)

	a := answers{dbLocal: true}

	ask := func(q string) string {
		fmt.Fprintln(out, q)
		out.Flush()
		s, _ := in.ReadString('\n')
		return strings.TrimSpace(s)
	}

	db := ask("Where should EveSynapse keep its data?\n  1. Set up PostgreSQL on this machine (recommended)\n  2. Use a PostgreSQL database I already have\nEnter 1 or 2 (default 1):")
	if db == "2" {
		a.dbLocal = false
		for {
			u := ask("What is the connection string for your database?\nIt looks like postgres://user:password@host:5432/evesynapse")
			if strings.HasPrefix(u, "postgres://") || strings.HasPrefix(u, "postgresql://") {
				a.databaseURL = u
				break
			}
			fmt.Fprintln(out, "That does not look like a connection string. It should start with postgres://")
		}
	}
	a.clientID = ask("What is your EVE Online application client ID?\nFind it at developers.eveonline.com, on your application page.\nLeave it blank to add it later:")
	a.clientSecret = ask("What is your EVE Online application client secret?\nIt is on the same application page as the client ID.\nLeave it blank to add it later:")
	a.contact = ask("What email should CCP see as the contact for your EVE application?\nCCP uses it to reach you about your application. Leave it blank to skip it:")

	// Review, with a chance to change each value.
	for {
		fmt.Fprintln(out, "\nHere is what the install will use:")
		rows := a.reviewRows()
		for i, r := range rows {
			fmt.Fprintf(out, "  %d. %s: %s\n", i+1, r[0], r[1])
		}
		choice := ask("Enter a number to change that line, or press Enter to install:")
		if choice == "" {
			break
		}
		switch choice {
		case "1":
			db = ask("Where should EveSynapse keep its data?\n  1. Set up PostgreSQL on this machine (recommended)\n  2. Use a PostgreSQL database I already have\nEnter 1 or 2 (default 1):")
			if db == "2" {
				a.dbLocal = false
				a.databaseURL = ask("What is the connection string for your database?")
			} else {
				a.dbLocal = true
				a.databaseURL = ""
			}
		case "2":
			a.clientID = ask("What is your EVE Online application client ID?")
		case "3":
			a.clientSecret = ask("What is your EVE Online application client secret?")
		case "4":
			a.contact = ask("What email should CCP see as the contact for your EVE application?")
		default:
			fmt.Fprintln(out, "No line with that number.")
		}
	}

	if dryRun {
		fmt.Fprintln(out, "\nDry run: nothing will be installed.")
		return
	}

	// Install, printing the plain-language steps.
	script, err := findSetupScript()
	if err != nil {
		fmt.Fprintln(out, "installer:", err)
		out.Flush()
		os.Exit(1)
	}
	fmt.Fprintln(out, "\nInstalling EveSynapse...")
	for _, s := range installSteps {
		fmt.Fprintln(out, "  [ ] "+s.name+"...")
	}
	out.Flush()
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), a.envVars()...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(out, "installer:", err)
		out.Flush()
		os.Exit(1)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(out, "installer:", err)
		out.Flush()
		os.Exit(1)
	}
	var buf strings.Builder
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	step := 0
	for scanner.Scan() {
		line := scanner.Text()
		buf.WriteString(line + "\n")
		for step < len(installSteps) && strings.Contains(line, installSteps[step].marker) {
			fmt.Fprintf(out, "  [x] %s\n", installSteps[step].name)
			out.Flush()
			step++
		}
	}
	if err := cmd.Wait(); err != nil {
		name := "Finishing up"
		if step < len(installSteps) {
			name = installSteps[step].name
		}
		fmt.Fprintln(out, "\nThe install stopped at: "+name)
		fmt.Fprintln(out, "\nWhat the installer said:")
		fmt.Fprintln(out, "---")
		fmt.Fprintln(out, buf.String())
		fmt.Fprintln(out, "---")
		out.Flush()
		os.Exit(1)
	}
	for ; step < len(installSteps); step++ {
		fmt.Fprintf(out, "  [x] %s\n", installSteps[step].name)
	}
	fmt.Fprintln(out, "\nDone. Your site is ready.")
}
