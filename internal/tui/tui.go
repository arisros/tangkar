package tui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arisros/tangkar/internal/order"
	"github.com/arisros/tangkar/internal/store"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func Run(base string) error {
	base = strings.TrimRight(base, "/")
	c := &client{base: base, http: &http.Client{Timeout: 20 * time.Second}}
	if p, err := tokenPath(base); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			c.token = strings.TrimSpace(string(b))
		}
	}
	var who me
	err := errLogin
	if c.token != "" {
		err = c.call(http.MethodGet, "/api/me", nil, &who)
	}
	if errors.Is(err, errLogin) {
		if err := login(c); err != nil {
			return err
		}
		err = c.call(http.MethodGet, "/api/me", nil, &who)
	}
	if err != nil {
		return err
	}

	m := newModel(c, who)
	p := tea.NewProgram(m, tea.WithAltScreen())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.send = p.Send
	go c.stream(ctx, "/api/events", func(ev string, data []byte) {
		var l list
		if ev == "list" && json.Unmarshal(data, &l) == nil {
			p.Send(listMsg(l))
		}
	}, func(ok bool) { p.Send(connMsg{list: true, ok: ok}) })
	_, err = p.Run()
	return err
}

type (
	listMsg list
	snapMsg snapshot
	connMsg struct{ list, ok bool }
	tickMsg time.Time
	doneMsg struct {
		text string
		err  error
	}
)

type view int

const (
	viewList view = iota
	viewOrder
	viewCreate
)

type prompt int

const (
	promptNone prompt = iota
	promptLockAt
	promptLate
	confirmLock
	confirmCancel
	confirmLateOff
)

type model struct {
	c    *client
	me   me
	send func(tea.Msg)

	view   view
	list   list
	cursor int
	offset time.Duration
	liveL  bool
	liveO  bool

	orderID string
	snap    *snapshot
	stopO   context.CancelFunc
	menu    string
	sate    int
	note    textinput.Model
	noting  bool
	pick    int

	prompt prompt
	input  textinput.Model

	form   []textinput.Model
	formAt int

	status string
	bad    bool
	width  int
}

func newModel(c *client, who me) *model {
	note := textinput.New()
	note.Placeholder = "no tauge"
	note.CharLimit = order.MaxNote
	note.Prompt = ""
	in := textinput.New()
	in.Prompt = "> "
	return &model{c: c, me: who, menu: order.Daging, note: note, input: in, width: 80}
}

func (m *model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) now() time.Time { return time.Now().Add(m.offset) }

func phaseOf(o store.Order, now time.Time) string {
	switch {
	case o.Status == order.StatusCancelled:
		return "cancelled"
	case o.Status == order.StatusClosed:
		return "closed"
	case now.Before(o.LockAt):
		return "open"
	case now.Before(o.LateUntil):
		return "late"
	}
	return "closed"
}

func (m *model) flash(text string, err error) {
	m.status, m.bad = text, false
	if err != nil {
		m.status, m.bad = err.Error(), true
	}
}

// act runs an API call off the UI loop and reports back as a doneMsg.
func (m *model) act(okText, method, path string, body any) tea.Cmd {
	return func() tea.Msg {
		if body == nil && method != http.MethodGet && method != http.MethodDelete {
			body = map[string]string{}
		}
		err := m.c.call(method, path, body, nil)
		return doneMsg{text: okText, err: err}
	}
}

func (m *model) openOrder(id string) {
	m.closeOrder()
	m.orderID, m.snap, m.view, m.pick, m.sate, m.menu = id, nil, viewOrder, 0, 0, order.Daging
	m.note.SetValue("")
	ctx, cancel := context.WithCancel(context.Background())
	m.stopO = cancel
	go m.c.stream(ctx, "/api/orders/"+id+"/events", func(ev string, data []byte) {
		var s snapshot
		if ev == "snapshot" && json.Unmarshal(data, &s) == nil && s.Order.ID == id {
			m.send(snapMsg(s))
		}
	}, func(ok bool) { m.send(connMsg{ok: ok}) })
}

func (m *model) closeOrder() {
	if m.stopO != nil {
		m.stopO()
		m.stopO = nil
	}
	m.liveO = false
}

func copyCmd(text string) tea.Cmd {
	return func() tea.Msg {
		fmt.Fprintf(os.Stdout, "\x1b]52;c;%s\a", base64.StdEncoding.EncodeToString([]byte(text)))
		return doneMsg{text: "link tersalin: " + text}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tickMsg:
		return m, tick()
	case listMsg:
		m.list = list(msg)
		m.offset = time.Until(time.UnixMilli(msg.Now))
		if m.cursor >= len(m.list.Orders) {
			m.cursor = max(0, len(m.list.Orders)-1)
		}
	case snapMsg:
		if msg.Order.ID == m.orderID {
			s := snapshot(msg)
			m.snap = &s
			m.offset = time.Until(time.UnixMilli(s.Now))
			if n := len(m.mine()); m.pick >= n {
				m.pick = max(0, n-1)
			}
		}
	case connMsg:
		if msg.list {
			m.liveL = msg.ok
		} else {
			m.liveO = msg.ok
		}
	case createdMsg:
		m.openOrder(msg.id)
		m.flash("dibuka: "+msg.url+"  (c untuk salin link)", nil)
	case doneMsg:
		m.flash(msg.text, msg.err)
		if errors.Is(msg.err, errLogin) {
			m.flash("login kedaluwarsa: keluar lalu jalankan tangkar lagi", msg.err)
		}
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.prompt != promptNone {
		return m.promptKey(k)
	}
	if k.Type == tea.KeyRunes && len(k.Runes) > 1 && !m.typing() {
		var cmds []tea.Cmd
		for _, r := range k.Runes {
			_, cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	}
	switch m.view {
	case viewCreate:
		return m.createKey(k)
	case viewOrder:
		return m.orderKey(k)
	}
	return m.listKey(k)
}

// typing reports whether keys go into a text field, where a burst of runes is
// a paste rather than several commands.
func (m *model) typing() bool {
	return m.view == viewCreate || (m.view == viewOrder && m.noting)
}

func (m *model) listKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "q":
		return m, tea.Quit
	case "up", "k":
		m.cursor = max(0, m.cursor-1)
	case "down", "j":
		m.cursor = min(len(m.list.Orders)-1, m.cursor+1)
	case "enter":
		if len(m.list.Orders) > 0 {
			m.openOrder(m.list.Orders[m.cursor].Order.ID)
			m.status = ""
		}
	case "c":
		if len(m.list.Orders) > 0 {
			return m, copyCmd(m.c.base + "/o/" + m.list.Orders[m.cursor].Order.ID)
		}
	case "n":
		if m.me.Admin {
			m.startCreate()
		}
	}
	return m, nil
}

func (m *model) startCreate() {
	mk := func(label, val string, limit int) textinput.Model {
		t := textinput.New()
		t.Prompt = fmt.Sprintf("%-18s", label)
		t.SetValue(val)
		t.CharLimit = limit
		return t
	}
	m.form = []textinput.Model{
		mk("Judul", "Soto Tangkar", 40),
		mk("Jam tutup", order.DefaultLock, 5),
		mk("Late join (menit)", "60", 3),
	}
	m.formAt = 0
	m.form[0].Focus()
	m.view = viewCreate
	m.status = ""
}

func (m *model) createKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.view = viewList
		return m, nil
	case "tab", "down", "shift+tab", "up":
		m.form[m.formAt].Blur()
		if k.String() == "tab" || k.String() == "down" {
			m.formAt = (m.formAt + 1) % len(m.form)
		} else {
			m.formAt = (m.formAt + len(m.form) - 1) % len(m.form)
		}
		m.form[m.formAt].Focus()
		return m, nil
	case "enter":
		late, err := strconv.Atoi(strings.TrimSpace(m.form[2].Value()))
		if err != nil {
			m.flash("", errors.New("late join harus angka menit"))
			return m, nil
		}
		body := map[string]any{"title": m.form[0].Value(), "lock": strings.TrimSpace(m.form[1].Value()), "late_minutes": late}
		return m, func() tea.Msg {
			var out struct {
				URL   string      `json:"url"`
				Order store.Order `json:"order"`
			}
			if err := m.c.call(http.MethodPost, "/api/orders", body, &out); err != nil {
				return doneMsg{err: err}
			}
			return createdMsg{url: out.URL, id: out.Order.ID}
		}
	}
	var cmd tea.Cmd
	m.form[m.formAt], cmd = m.form[m.formAt].Update(k)
	return m, cmd
}

type createdMsg struct{ url, id string }

func (m *model) mine() []store.Line {
	if m.snap == nil {
		return nil
	}
	var out []store.Line
	for _, l := range m.snap.Lines {
		if l.Email == m.me.Email {
			out = append(out, l)
		}
	}
	return out
}

func (m *model) orderKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.noting {
		switch k.String() {
		case "esc", "tab":
			m.noting = false
			m.note.Blur()
			return m, nil
		case "enter":
			m.noting = false
			m.note.Blur()
			return m, m.submit()
		}
		var cmd tea.Cmd
		m.note, cmd = m.note.Update(k)
		return m, cmd
	}
	if m.snap == nil {
		if k.String() == "esc" || k.String() == "q" {
			m.closeOrder()
			m.view = viewList
		}
		return m, nil
	}
	o := m.snap.Order
	p := phaseOf(o, m.now())
	base := "/api/orders/" + o.ID
	switch k.String() {
	case "esc", "backspace":
		m.closeOrder()
		m.view, m.status = viewList, ""
	case "q":
		return m, tea.Quit
	case "d", "1":
		m.menu = order.Daging
	case "a", "2":
		m.menu = order.Campur
	case "tab", "left", "right", "m":
		if m.menu == order.Daging {
			m.menu = order.Campur
		} else {
			m.menu = order.Daging
		}
	case "+", "=":
		m.sate = min(order.MaxSate, m.sate+1)
	case "-", "_":
		m.sate = max(0, m.sate-1)
	case "n", "/":
		m.noting = true
		return m, m.note.Focus()
	case "enter", "p":
		return m, m.submit()
	case "up", "k":
		m.pick = max(0, m.pick-1)
	case "down", "j":
		m.pick = min(len(m.mine())-1, m.pick+1)
	case "x", "delete":
		if mine := m.mine(); m.pick < len(mine) {
			return m, m.act("dihapus", http.MethodDelete, fmt.Sprintf("%s/lines/%d", base, mine[m.pick].ID), nil)
		}
	case "c":
		return m, copyCmd(m.c.base + "/o/" + o.ID)
	}
	if !m.me.Admin {
		return m, nil
	}
	switch k.String() {
	case "L":
		if p == "open" {
			m.prompt = confirmLock
		}
	case "T":
		if p == "open" {
			m.ask(promptLockAt, o.LockAt.In(order.Jakarta).Format("15:04"))
			return m, m.input.Focus()
		}
	case "J":
		if p == "open" || p == "late" {
			m.ask(promptLate, strconv.Itoa(int(o.LateUntil.Sub(o.LockAt).Minutes())))
			return m, m.input.Focus()
		}
	case "X":
		if p == "late" {
			m.prompt = confirmLateOff
		}
	case "C":
		if p == "open" {
			m.prompt = confirmCancel
		}
	case "R":
		for _, s := range m.snap.Sends {
			if s.Status == order.SendFailed {
				return m, m.act("kirim ulang dijadwalkan", http.MethodPost, fmt.Sprintf("%s/sends/%d/retry", base, s.Batch), nil)
			}
		}
		m.flash("tidak ada kiriman gagal", nil)
	}
	return m, nil
}

func (m *model) ask(p prompt, val string) {
	m.prompt = p
	m.input.SetValue(val)
	m.input.CursorEnd()
}

func (m *model) submit() tea.Cmd {
	if m.snap == nil {
		return nil
	}
	body := map[string]any{"menu": m.menu, "sate": m.sate, "note": m.note.Value()}
	path := "/api/orders/" + m.snap.Order.ID + "/lines"
	m.note.SetValue("")
	m.sate = 0
	return m.act("dipesan: "+order.Line{Menu: body["menu"].(string), Sate: body["sate"].(int), Note: order.CleanText(body["note"].(string))}.String(), http.MethodPost, path, body)
}

func (m *model) promptKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	base := "/api/orders/" + m.orderID
	switch m.prompt {
	case confirmLock, confirmCancel, confirmLateOff:
		p := m.prompt
		m.prompt = promptNone
		if k.String() != "y" && k.String() != "Y" {
			m.flash("batal", nil)
			return m, nil
		}
		switch p {
		case confirmLock:
			return m, m.act("dikunci, dikirim ke grup", http.MethodPost, base+"/lock", nil)
		case confirmCancel:
			return m, m.act("pesanan dibatalkan", http.MethodPost, base+"/cancel", nil)
		default:
			return m, m.act("late join ditutup", http.MethodPost, base+"/late", map[string]int{"minutes": 0})
		}
	}
	switch k.String() {
	case "esc":
		m.prompt = promptNone
		m.input.Blur()
		return m, nil
	case "enter":
		p, v := m.prompt, strings.TrimSpace(m.input.Value())
		m.prompt = promptNone
		m.input.Blur()
		if p == promptLockAt {
			return m, m.act("jam tutup jadi "+v, http.MethodPost, base+"/lock", map[string]string{"at": v})
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			m.flash("", errors.New("harus angka menit"))
			return m, nil
		}
		return m, m.act(fmt.Sprintf("late join %d menit setelah tutup", n), http.MethodPost, base+"/late", map[string]int{"minutes": n})
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

var (
	sInk    = lipgloss.NewStyle().Bold(true)
	sMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	sRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("160")).Bold(true)
	sGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("34"))
	sAmber  = lipgloss.NewStyle().Foreground(lipgloss.Color("172"))
	sKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("236")).Padding(0, 1)
	sOn     = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("160")).Bold(true)
	sOff    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	sBoard  = lipgloss.NewStyle().Foreground(lipgloss.Color("229")).Background(lipgloss.Color("235")).Bold(true).Padding(0, 2)
	sSelect = lipgloss.NewStyle().Reverse(true)
)

func dot(ok bool) string {
	if ok {
		return sGreen.Render("● live")
	}
	return sAmber.Render("○ menyambung")
}

func left(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}

func clock(t time.Time) string { return t.In(order.Jakarta).Format("15:04") }

func keys(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		b.WriteString(sKey.Render(pairs[i]) + " " + sMuted.Render(pairs[i+1]) + "  ")
	}
	return b.String()
}

func (m *model) View() string {
	var b strings.Builder
	role := ""
	if m.me.Admin {
		role = sAmber.Render(" admin")
	}
	host := strings.TrimPrefix(strings.TrimPrefix(m.c.base, "https://"), "http://")
	fmt.Fprintf(&b, "%s  %s  %s%s\n\n", sInk.Render("TANGKAR"), sMuted.Render(host+" · "+m.me.Name), dot(m.liveL), role)
	switch m.view {
	case viewCreate:
		m.viewCreate(&b)
	case viewOrder:
		m.viewOrder(&b)
	default:
		m.viewList(&b)
	}
	if m.status != "" {
		st := sGreen
		if m.bad {
			st = sRed
		}
		b.WriteString("\n" + st.Render(m.status) + "\n")
	}
	return b.String()
}

func (m *model) viewList(b *strings.Builder) {
	now := m.now()
	b.WriteString(sMuted.Render("PESANAN HARI INI") + "\n\n")
	if len(m.list.Orders) == 0 {
		b.WriteString("  belum ada pesanan\n")
	}
	fmt.Fprintf(b, "  %-8s %-18s %-6s %-10s %-10s %6s %6s %5s %5s\n", "ID", "JUDUL", "TUTUP", "SISA", "STATUS", "DAGING", "CAMPUR", "SATE", "BUKA")
	for i, s := range m.list.Orders {
		o := s.Order
		p := phaseOf(o, now)
		rem := ""
		switch p {
		case "open":
			rem = left(o.LockAt.Sub(now))
		case "late":
			rem = left(o.LateUntil.Sub(now))
		}
		wa := ""
		for _, x := range s.Sends {
			if x.Status == order.SendFailed {
				wa = sRed.Render(" WA gagal")
			}
		}
		title := []rune(o.Title)
		if len(title) > 18 {
			title = title[:18]
		}
		row := fmt.Sprintf("  %-8s %-18s %-6s %-10s %-10s %6d %6d %5d %5d", o.ID, string(title), clock(o.LockAt), rem, p, s.Totals.Daging, s.Totals.Campur, s.Totals.Sate, s.Viewers)
		if i == m.cursor {
			row = sSelect.Render(row)
		}
		b.WriteString(row + wa + "\n")
	}
	b.WriteString("\n")
	if m.me.Admin {
		b.WriteString(keys("n", "baru"))
	}
	b.WriteString(keys("↑↓", "pilih", "enter", "buka", "c", "salin link", "q", "keluar") + "\n")
}

func (m *model) viewCreate(b *strings.Builder) {
	b.WriteString(sInk.Render("BUKA PESANAN BARU") + "\n\n")
	for _, f := range m.form {
		b.WriteString("  " + f.View() + "\n")
	}
	b.WriteString("\n" + keys("tab", "pindah", "enter", "buka pesanan", "esc", "batal") + "\n")
}

func (m *model) viewOrder(b *strings.Builder) {
	if m.snap == nil {
		b.WriteString("  memuat…\n\n" + keys("esc", "kembali") + "\n")
		return
	}
	s, o, now := m.snap, m.snap.Order, m.now()
	p := phaseOf(o, now)
	fmt.Fprintf(b, "%s  %s  %s\n", sInk.Render(strings.ToUpper(o.Title)), sMuted.Render(o.DayLabel), dot(m.liveO))
	switch p {
	case "open":
		st := sBoard
		if o.LockAt.Sub(now) < 5*time.Minute {
			st = st.Foreground(lipgloss.Color("203"))
		}
		b.WriteString(st.Render(fmt.Sprintf("BUKA · tutup %s · sisa %s", clock(o.LockAt), left(o.LockAt.Sub(now)))) + "\n")
	case "late":
		b.WriteString(sBoard.Foreground(lipgloss.Color("214")).Render(fmt.Sprintf("LATE JOIN · s.d. %s · sisa %s", clock(o.LateUntil), left(o.LateUntil.Sub(now)))) + "\n")
		b.WriteString(sAmber.Render("pesanan masuk TAMBAHAN, dikirim otomatis 2 menit setelah susulan terakhir") + "\n")
	case "cancelled":
		b.WriteString(sRed.Render("DIBATALKAN") + "\n")
	default:
		b.WriteString(sRed.Render("TUTUP") + "\n")
	}
	if len(s.Viewers) > 0 {
		b.WriteString(sMuted.Render("lagi buka: "+strings.Join(s.Viewers, ", ")) + "\n")
	}
	b.WriteString("\n")

	if p == "open" || p == "late" {
		d, c := sOff.Render("  DAGING  "), sOff.Render("  CAMPUR  ")
		if m.menu == order.Daging {
			d = sOn.Render("[ DAGING ]")
		} else {
			c = sOn.Render("[ CAMPUR ]")
		}
		note := m.note.View()
		if !m.noting && m.note.Value() == "" {
			note = sMuted.Render("(n untuk isi)")
		}
		fmt.Fprintf(b, "  %s %s   SATE [-] %2d [+]   CATATAN %s\n\n", d, c, m.sate, note)
	}

	mine := m.mine()
	if len(mine) > 0 {
		b.WriteString(sMuted.Render("PESANAN KAMU") + "\n")
		for i, l := range mine {
			tag := ""
			if l.Late {
				tag = sAmber.Render(" susulan")
			}
			row := "  " + l.Order().String()
			if i == m.pick {
				row = sSelect.Render(row)
			}
			b.WriteString(row + tag + "\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(sMuted.Render("NOTA") + "\n")
	m.viewNota(b)
	b.WriteString("\n")

	if m.prompt != promptNone {
		switch m.prompt {
		case confirmLock:
			b.WriteString(sRed.Render("Kunci sekarang dan kirim ke grup? (y/n)") + "\n")
		case confirmCancel:
			b.WriteString(sRed.Render("Batalkan pesanan ini? (y/n)") + "\n")
		case confirmLateOff:
			b.WriteString(sRed.Render("Tutup late join sekarang? (y/n)") + "\n")
		case promptLockAt:
			b.WriteString("Jam tutup baru (HH:MM)\n" + m.input.View() + "\n")
		case promptLate:
			b.WriteString("Late join, menit setelah tutup (0 = tutup)\n" + m.input.View() + "\n")
		}
		return
	}
	if p == "open" || p == "late" {
		b.WriteString(keys("d/a", "daging/campur", "+/-", "sate", "n", "catatan", "enter", "pesan") + "\n")
	}
	b.WriteString(keys("↑↓", "pilih", "x", "hapus", "c", "salin link", "esc", "kembali") + "\n")
	if m.me.Admin {
		b.WriteString(keys("L", "kunci", "T", "jam tutup", "J", "late join", "X", "tutup late", "C", "batal", "R", "kirim ulang") + "\n")
	}
}

func (m *model) viewNota(b *strings.Builder) {
	s := m.snap
	if len(s.Lines) == 0 {
		b.WriteString(sMuted.Render("  belum ada pesanan") + "\n")
		return
	}
	sends := map[int]store.Send{}
	for _, x := range s.Sends {
		sends[x.Batch] = x
	}
	groups := map[int][]store.Line{}
	var batches []int
	var waiting []store.Line
	for _, l := range s.Lines {
		if l.Batch == nil {
			waiting = append(waiting, l)
			continue
		}
		if _, ok := groups[*l.Batch]; !ok {
			batches = append(batches, *l.Batch)
		}
		groups[*l.Batch] = append(groups[*l.Batch], l)
	}
	line := func(l store.Line) {
		fmt.Fprintf(b, "  %-28s %s\n", l.Order().String(), sMuted.Render(l.Name))
	}
	for i, k := range batches {
		title := "PESANAN " + clock(s.Order.LockAt)
		if i > 0 {
			title = fmt.Sprintf("TAMBAHAN #%d", i)
		}
		st := ""
		switch x := sends[k]; x.Status {
		case order.SendSent:
			st = sGreen.Render("✓ terkirim " + clock(time.Unix(x.SentAt, 0)))
		case order.SendPending:
			st = sAmber.Render("mengirim…")
		case order.SendFailed:
			st = sRed.Render("gagal: " + x.Error)
		}
		fmt.Fprintf(b, "  %s  %s\n", sInk.Render(title), st)
		for _, l := range groups[k] {
			line(l)
		}
	}
	if len(waiting) > 0 {
		at := ""
		if s.CutAt > 0 {
			at = "kirim ±" + clock(time.UnixMilli(s.CutAt))
		}
		fmt.Fprintf(b, "  %s  %s\n", sInk.Render("SUSULAN, MENUNGGU"), sAmber.Render(at))
		for _, l := range waiting {
			line(l)
		}
	}
	t := s.Totals
	fmt.Fprintf(b, "  %s\n  DAGING %d · CAMPUR %d · SATE %d · %s\n", strings.Repeat("-", 40), t.Daging, t.Campur, t.Sate, sInk.Render(fmt.Sprintf("TOTAL %d PORSI", t.Bowls)))
}
