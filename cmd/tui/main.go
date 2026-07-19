// Command tui is the device-holder's control surface for the overlay:
// it lists the device's org grants and toggles them — the six o'clock
// switch as a keypress. It talks to the registry's HTTP API and reads
// the device identity from the local tailscaled socket. It contains no
// protocol logic.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/juanfont/headscale/hscontrol/registryclient"
	"tailscale.com/client/local"
)

const refreshInterval = 3 * time.Second

var (
	styleTitle    = lipgloss.NewStyle().Bold(true)
	styleFaint    = lipgloss.NewStyle().Faint(true)
	styleActive   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"})
	styleInactive = lipgloss.NewStyle().Faint(true)
	stylePending  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})
	styleErr      = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "124", Dark: "203"})
	styleSelected = lipgloss.NewStyle().Bold(true)
)

type model struct {
	reg     *registryclient.Client
	nodeKey string

	device  string
	grants  []registryclient.Grant
	peers   map[string]int
	cursor  int
	pending map[string]bool
	err     error
	synced  time.Time
}

type refreshMsg struct {
	grants *registryclient.DeviceGrants
	world  *registryclient.World
	err    error
}

type toggleDoneMsg struct {
	org string
	err error
}

type tickMsg struct{}

func refreshCmd(reg *registryclient.Client, nodeKey string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()

		grants, err := reg.Grants(ctx, nodeKey)
		if err != nil {
			return refreshMsg{err: err}
		}

		world, err := reg.World(ctx, nodeKey)
		if err != nil {
			return refreshMsg{err: err}
		}

		return refreshMsg{grants: grants, world: world}
	}
}

func toggleCmd(reg *registryclient.Client, nodeKey, org string, active bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()

		return toggleDoneMsg{org: org, err: reg.SetActive(ctx, nodeKey, org, active)}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Init() tea.Cmd {
	return tea.Batch(refreshCmd(m.reg, m.nodeKey), tickCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.grants)-1 {
				m.cursor++
			}
		case " ", "enter":
			if m.cursor < len(m.grants) {
				g := m.grants[m.cursor]
				if !m.pending[g.Org] {
					m.pending[g.Org] = true
					return m, toggleCmd(m.reg, m.nodeKey, g.Org, !g.Active)
				}
			}
		case "r":
			return m, refreshCmd(m.reg, m.nodeKey)
		}

	case refreshMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}

		m.err = nil
		m.device = msg.grants.Device.Name
		m.grants = msg.grants.Grants
		m.synced = time.Now()

		m.peers = map[string]int{}
		for _, p := range msg.world.Peers {
			for _, org := range p.Orgs {
				m.peers[org]++
			}
		}

		if m.cursor >= len(m.grants) && len(m.grants) > 0 {
			m.cursor = len(m.grants) - 1
		}

	case toggleDoneMsg:
		delete(m.pending, msg.org)
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}

		return m, refreshCmd(m.reg, m.nodeKey)

	case tickMsg:
		return m, tea.Batch(refreshCmd(m.reg, m.nodeKey), tickCmd())
	}

	return m, nil
}

func (m model) View() string {
	var b []byte

	device := m.device
	if device == "" {
		device = "…"
	}

	b = fmt.Appendf(b, "\n  %s\n\n", styleTitle.Render("overlay · "+device))

	if len(m.grants) == 0 {
		b = fmt.Appendf(b, "  %s\n", styleFaint.Render("no grants — this device belongs to no orgs yet"))
	}

	for i, g := range m.grants {
		dot := styleInactive.Render("○")
		addr := styleInactive.Render(g.Address)
		peersCol := styleFaint.Render("—")

		if g.Active {
			dot = styleActive.Render("●")
			addr = g.Address

			n := m.peers[g.Org]
			peersCol = fmt.Sprintf("%d peer%s", n, plural(n))
		}

		if m.pending[g.Org] {
			dot = stylePending.Render("◌")
		}

		cursor := "  "
		org := fmt.Sprintf("%-10s", g.Org)

		if i == m.cursor {
			cursor = styleSelected.Render("▶ ")
			org = styleSelected.Render(org)
		}

		b = fmt.Appendf(b, "  %s%s %s %-24s %s\n", cursor, dot, org, addr, peersCol)
	}

	b = fmt.Appendf(b, "\n")

	if m.err != nil {
		b = fmt.Appendf(b, "  %s\n", styleErr.Render("registry unreachable: "+m.err.Error()))
	} else if !m.synced.IsZero() {
		b = fmt.Appendf(
			b,
			"  %s\n",
			styleFaint.Render(
				"synced "+m.synced.Format("15:04:05")+" · space toggle · j/k move · q quit",
			),
		)
	}

	return string(b)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}

	return "s"
}

func selfNodeKey(ctx context.Context, socket string) (string, error) {
	lc := &local.Client{Socket: socket, UseSocketOnly: true}

	st, err := lc.Status(ctx)
	if err != nil {
		return "", fmt.Errorf("reading tailscaled status from %s: %w", socket, err)
	}

	return st.Self.PublicKey.String(), nil
}

func main() {
	registryURL := flag.String("registry", "http://127.0.0.1:4000", "registry base URL")
	socket := flag.String("socket", "/var/run/tailscaled.socket", "tailscaled socket path")
	nodeKeyFlag := flag.String("node-key", "", "node key override (skips tailscaled lookup)")
	once := flag.Bool("once", false, "render one status frame and exit (no TTY needed)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nodeKey := *nodeKeyFlag
	if nodeKey == "" {
		var err error

		nodeKey, err = selfNodeKey(ctx, *socket)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	m := model{
		reg:     registryclient.New(*registryURL),
		nodeKey: nodeKey,
		pending: map[string]bool{},
	}

	if *once {
		msg := refreshCmd(m.reg, m.nodeKey)()

		updated, _ := m.Update(msg)
		fmt.Print(updated.View())

		if rm, ok := msg.(refreshMsg); ok && rm.err != nil {
			os.Exit(1)
		}

		return
	}

	if _, err := tea.NewProgram(m).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
