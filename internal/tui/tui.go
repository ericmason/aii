// Package tui is a bubbletea two-pane search UI for the aii database.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ericmason/aii/internal/store"
)

func Run(db *store.DB) error {
	m := newModel(db)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// --- model -------------------------------------------------------------

type model struct {
	db       *store.DB
	input    textinput.Model
	results  []store.Result
	cursor   int
	agent    string // "", "claude_code", "codex", "cursor"
	err      error
	status   string
	width    int
	height   int
	focused  focus
	preview  []store.Row
	previewS *store.Session
	lastQuery string
}

type focus int

const (
	focusInput focus = iota
	focusList
)

func newModel(db *store.DB) model {
	ti := textinput.New()
	ti.Placeholder = "search…"
	ti.Focus()
	ti.CharLimit = 200
	return model{db: db, input: ti, focused: focusInput}
}

// --- messages ----------------------------------------------------------

type searchDoneMsg struct {
	query   string
	results []store.Result
	err     error
}

type previewMsg struct {
	session *store.Session
	rows    []store.Row
}

func (m model) doSearch(q string) tea.Cmd {
	return func() tea.Msg {
		if strings.TrimSpace(q) == "" {
			return searchDoneMsg{query: q}
		}
		res, err := m.db.Search(store.Query{Text: q, Agent: m.agent, Limit: 100})
		return searchDoneMsg{query: q, results: res, err: err}
	}
}

func (m model) loadPreview(r store.Result) tea.Cmd {
	return func() tea.Msg {
		s, err := m.db.SessionByUIDAny(r.SessionUID)
		if err != nil || s == nil {
			return previewMsg{}
		}
		ord := 0
		if len(r.Excerpts) > 0 {
			ord = r.Excerpts[0].Ordinal
		}
		rows, err := m.db.ContextRows(s.ID, ord, 3)
		if err != nil {
			return previewMsg{}
		}
		return previewMsg{session: s, rows: rows}
	}
}

// debounce helpers -----------------------------------------------------

type debouncedMsg struct{ query string }

func debounce(q string) tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return debouncedMsg{q} })
}

// Init/Update/View -----------------------------------------------------

func (m model) Init() tea.Cmd { return textinput.Blink }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		}
		switch msg.String() {
		case "esc":
			if m.focused == focusList {
				m.focused = focusInput
				m.input.Focus()
			} else {
				return m, tea.Quit
			}
			return m, nil
		case "tab":
			if m.focused == focusInput {
				m.focused = focusList
				m.input.Blur()
			} else {
				m.focused = focusInput
				m.input.Focus()
			}
			return m, nil
		case "f":
			if m.focused == focusList {
				m.agent = nextAgent(m.agent)
				m.status = fmt.Sprintf("filter: %s", agentLabel(m.agent))
				return m, m.doSearch(m.lastQuery)
			}
		case "j", "down":
			if m.focused == focusList && m.cursor < len(m.results)-1 {
				m.cursor++
				return m, m.loadPreview(m.results[m.cursor])
			}
		case "k", "up":
			if m.focused == focusList && m.cursor > 0 {
				m.cursor--
				return m, m.loadPreview(m.results[m.cursor])
			}
		case "enter":
			if m.focused == focusInput {
				q := m.input.Value()
				m.lastQuery = q
				m.status = "searching…"
				return m, m.doSearch(q)
			}
			if len(m.results) > m.cursor {
				return m, m.loadPreview(m.results[m.cursor])
			}
		}

		if m.focused == focusInput {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			q := m.input.Value()
			m.lastQuery = q
			return m, tea.Batch(cmd, debounce(q))
		}

	case debouncedMsg:
		if msg.query == m.input.Value() {
			return m, m.doSearch(msg.query)
		}

	case searchDoneMsg:
		if msg.query != m.input.Value() && msg.query != m.lastQuery {
			return m, nil // stale
		}
		m.results = msg.results
		m.err = msg.err
		if m.cursor >= len(m.results) {
			m.cursor = 0
		}
		if len(m.results) > 0 {
			m.status = fmt.Sprintf("%d results", len(m.results))
			return m, m.loadPreview(m.results[m.cursor])
		}
		m.status = "0 results"
		m.previewS = nil
		m.preview = nil

	case previewMsg:
		m.previewS = msg.session
		m.preview = msg.rows
	}

	if m.focused == focusInput {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// --- styling -----------------------------------------------------------

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	footerStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	selectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("255"))
	agentStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
	snippetStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	hitStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	roleStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("117")).Bold(true)
)

func (m model) View() string {
	if m.width == 0 {
		return "loading…"
	}

	header := headerStyle.Render(" aii — ") + m.input.View()
	if m.status != "" {
		header += "  " + footerStyle.Render("["+m.status+"]")
	}

	leftW := m.width / 2
	if leftW < 30 {
		leftW = 30
	}
	rightW := m.width - leftW - 1
	bodyH := m.height - 3 // header + footer

	left := m.renderResults(leftW, bodyH)
	right := m.renderPreview(rightW, bodyH)

	body := lipgloss.JoinHorizontal(lipgloss.Top, left, lipgloss.NewStyle().Width(1).Render(" "), right)

	foot := footerStyle.Render(" tab=switch pane  j/k=nav  f=filter agent  enter=run  esc/ctrl-c=quit")
	return lipgloss.JoinVertical(lipgloss.Left, header, body, foot)
}

func (m model) renderResults(w, h int) string {
	if m.err != nil {
		return lipgloss.NewStyle().Width(w).Render("error: " + m.err.Error())
	}
	var lines []string
	for i, r := range m.results {
		title := r.Title
		if title == "" {
			title = r.SessionUID[:minInt(8, len(r.SessionUID))]
		}
		date := store.FormatTime(r.StartedAt)
		selected := i == m.cursor && m.focused == focusList
		prefix := " "
		if selected {
			prefix = "▌"
		}
		matchSuffix := ""
		if r.MatchCount > len(r.Excerpts) {
			matchSuffix = fmt.Sprintf(" (%d)", r.MatchCount)
		}
		head := fmt.Sprintf("%s%s %s  %s%s",
			prefix, agentStyle.Render(agentShort(r.Agent)), date,
			truncate(title, w-20), matchSuffix)
		if selected {
			head = selectedStyle.Render(padRight(head, w))
		}
		lines = append(lines, head)

		// Up to two excerpts per session (saves vertical space).
		maxEx := 2
		if selected {
			maxEx = 3
		}
		for j, e := range r.Excerpts {
			if j >= maxEx {
				break
			}
			body := fmt.Sprintf("  %s %s",
				roleTag(e.Role),
				highlight(truncateLine(e.Snippet, w-6), w-6))
			if selected {
				body = selectedStyle.Render(padRight(body, w))
			} else {
				body = snippetStyle.Render(body)
			}
			lines = append(lines, body)
		}
		lines = append(lines, "")
		if len(lines) >= h-1 {
			break
		}
	}
	return lipgloss.NewStyle().Width(w).Height(h).Render(strings.Join(lines, "\n"))
}

func roleTag(role string) string {
	switch role {
	case "user":
		return "user"
	case "assistant":
		return "asst"
	case "thinking":
		return "thk "
	case "tool":
		return "tool"
	}
	if len(role) > 4 {
		return role[:4]
	}
	return role
}

func (m model) renderPreview(w, h int) string {
	if m.previewS == nil {
		return lipgloss.NewStyle().Width(w).Height(h).Foreground(lipgloss.Color("241")).Render("(no selection)")
	}
	var b strings.Builder
	b.WriteString(headerStyle.Render(m.previewS.Agent+"  "+m.previewS.UID) + "\n")
	if m.previewS.Workspace != "" {
		b.WriteString(footerStyle.Render(m.previewS.Workspace) + "\n")
	}
	if s := strings.TrimSpace(m.previewS.Summary); s != "" {
		b.WriteString("\n" + footerStyle.Render("summary:") + "\n")
		b.WriteString(wrap(truncate(s, 800), w) + "\n")
	}
	b.WriteString("\n")
	for _, r := range m.preview {
		b.WriteString(roleStyle.Render("▸ "+r.Role+" "+store.FormatTime(r.TS)) + "\n")
		b.WriteString(wrap(r.Content, w) + "\n\n")
	}
	return lipgloss.NewStyle().Width(w).Height(h).Render(b.String())
}

// --- helpers -----------------------------------------------------------

func agentShort(a string) string {
	switch a {
	case "claude_code":
		return "cc"
	case "codex":
		return "cdx"
	case "cursor":
		return "cur"
	}
	return a
}

func nextAgent(cur string) string {
	order := []string{"", "claude_code", "codex", "cursor"}
	for i, a := range order {
		if a == cur {
			return order[(i+1)%len(order)]
		}
	}
	return ""
}

func agentLabel(a string) string {
	if a == "" {
		return "all"
	}
	return agentShort(a)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 2 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func truncateLine(s string, n int) string {
	return truncate(strings.ReplaceAll(s, "\n", " "), n)
}

func padRight(s string, w int) string {
	visible := lipgloss.Width(s)
	if visible >= w {
		return s
	}
	return s + strings.Repeat(" ", w-visible)
}

func highlight(s string, w int) string {
	// replace «..» from FTS snippet() with colored segments
	var b strings.Builder
	for {
		i := strings.Index(s, "«")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		rest := s[i+len("«"):]
		j := strings.Index(rest, "»")
		if j < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(hitStyle.Render(rest[:j]))
		s = rest[j+len("»"):]
	}
}

func wrap(s string, w int) string {
	if w < 10 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		for len([]rune(line)) > w {
			r := []rune(line)
			// soft wrap at last space before w
			cut := w
			for i := w - 1; i >= w/2; i-- {
				if r[i] == ' ' {
					cut = i
					break
				}
			}
			out = append(out, string(r[:cut]))
			line = string(r[cut:])
		}
		out = append(out, line)
	}
	// cap total lines to keep the pane from blowing up
	const maxLines = 60
	if len(out) > maxLines {
		out = append(out[:maxLines], "…")
	}
	return strings.Join(out, "\n")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
