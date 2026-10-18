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
	viewPeople
)

type prompt int

const (
	promptNone prompt = iota
	promptLockAt
	promptLate
	confirmLock
	confirmCancel
	confirmLateOff
	confirmDelete
	promptFriend
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
	mode    string
	editID  int64
	forName string

	prompt prompt
	input  textinput.Model

	form   []textinput.Model
	formAt int

	people  []store.Person
	pickP   int
	asSince time.Time

	status string
	bad    bool
	width  int
	height int
}

func newModel(c *client, who me) *model {
	note := textinput.New()
	note.Placeholder = "no tauge"
	note.CharLimit = order.MaxNote
	note.Prompt = ""
	in := textinput.New()
	in.Prompt = "> "
	return &model{c: c, me: who, menu: order.Daging, note: note, input: in, width: 80, height: 40}
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
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		if m.me.ActingAs && time.Since(m.asSince) > 30*time.Minute {
			return m, tea.Batch(tick(), m.actAs(""))
		}
		return m, tick()
	case peopleMsg:
		if msg.err != nil {
			m.flash("", msg.err)
			break
		}
		m.people, m.pickP, m.view = msg.people, 0, viewPeople
	case actingMsg:
		if msg.err != nil {
			m.flash("", msg.err)
			break
		}
		m.me = msg.me
		m.view = viewList
		m.asSince = time.Now()
		if m.me.ActingAs {
			m.flash("🎭 sekarang sebagai "+m.me.Name, nil)
		} else {
			m.flash("kembali sebagai "+m.me.Name, nil)
		}
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
	case savedMsg:
		m.flash(msg.text, msg.err)
		if msg.err == nil {
			m.mode = ""
			m.sate = 0
			m.note.SetValue("")
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
	case viewPeople:
		switch k.String() {
		case "esc", "q":
			m.view = viewList
		case "up", "k":
			m.pickP = max(0, m.pickP-1)
		case "down", "j":
			m.pickP = min(len(m.people)-1, m.pickP+1)
		case "enter":
			if m.pickP < len(m.people) {
				return m, m.actAs(m.people[m.pickP].Email)
			}
		}
		return m, nil
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
	case "I":
		if m.me.ActingAs {
			return m, m.actAs("")
		}
		if m.me.Admin {
			return m, m.loadPeople()
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
		mk("Uji? (y/n)", "n", 1),
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
		test := strings.EqualFold(strings.TrimSpace(m.form[3].Value()), "y")
		body := map[string]any{"title": m.form[0].Value(), "lock": strings.TrimSpace(m.form[1].Value()), "late_minutes": late, "test": test}
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

type peopleMsg struct {
	people []store.Person
	err    error
}

type actingMsg struct {
	me  me
	err error
}

func (m *model) loadPeople() tea.Cmd {
	return func() tea.Msg {
		var out struct {
			People []store.Person `json:"people"`
		}
		err := m.c.call(http.MethodGet, "/api/people", nil, &out)
		return peopleMsg{people: out.People, err: err}
	}
}

// actAs switches the client to email ("" to stop) and reloads who we are.
func (m *model) actAs(email string) tea.Cmd {
	return func() tea.Msg {
		if email == "" {
			_ = m.c.call(http.MethodPost, "/api/impersonate/stop", map[string]string{}, nil)
			m.c.setAs("")
		} else {
			if err := m.c.call(http.MethodPost, "/api/impersonate", map[string]string{"email": email}, nil); err != nil {
				return actingMsg{err: err}
			}
			m.c.setAs(email)
		}
		var who me
		err := m.c.call(http.MethodGet, "/api/me", nil, &who)
		return actingMsg{me: who, err: err}
	}
}

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
	if m.formActive(p) {
		switch k.String() {
		case "esc":
			if m.mode != "" {
				m.mode, m.status = "", ""
				return m, nil
			}
			m.closeOrder()
			m.view, m.status = viewList, ""
			return m, nil
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
		case "f":
			if m.mode != "edit" && m.mode != "friend" {
				m.ask(promptFriend, "")
				return m, m.input.Focus()
			}
		}
	} else {
		mine := m.mine()
		switch k.String() {
		case "esc", "backspace":
			m.closeOrder()
			m.view, m.status = viewList, ""
		case "up", "k":
			m.pick = max(0, m.pick-1)
		case "down", "j":
			m.pick = min(len(mine)-1, m.pick+1)
		case "o", "+":
			if p == "open" || p == "late" {
				m.startForm("add", store.Line{Menu: order.Daging})
			}
		case "f":
			if p == "open" || p == "late" {
				m.ask(promptFriend, "")
				return m, m.input.Focus()
			}
		case "g", "e":
			if m.pick < len(mine) {
				if !canChange(mine[m.pick], p) {
					m.flash("", errors.New("mangkok ini sudah dikirim ke Mamang, tidak bisa diganti"))
				} else {
					m.startForm("edit", mine[m.pick])
				}
			}
		case "x", "delete":
			if m.pick < len(mine) {
				if !canChange(mine[m.pick], p) {
					m.flash("", errors.New("mangkok ini sudah dikirim ke Mamang, tidak bisa dihapus"))
				} else {
					m.editID = mine[m.pick].ID
					m.prompt = confirmDelete
				}
			}
		}
	}
	switch k.String() {
	case "q":
		if !m.noting {
			return m, tea.Quit
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

func canChange(l store.Line, p string) bool {
	if !l.Late {
		return p == "open"
	}
	return l.Batch == nil && p != "cancelled"
}

// formActive reports whether the order form shows: before someone's first
// bowl, or when they chose to add or change one. Otherwise they see their card.
func (m *model) formActive(p string) bool {
	if p != "open" && p != "late" {
		return false
	}
	if m.mode == "edit" {
		for _, l := range m.mine() {
			if l.ID == m.editID && canChange(l, p) {
				return true
			}
		}
		m.mode = ""
	}
	return m.mode == "add" || m.mode == "friend" || len(m.mine()) == 0
}

func (m *model) startForm(mode string, l store.Line) {
	m.mode, m.editID, m.status, m.forName = mode, l.ID, "", l.For
	m.menu, m.sate = l.Menu, l.Sate
	m.note.SetValue(l.Note)
}

type savedMsg struct {
	text string
	err  error
}

func (m *model) submit() tea.Cmd {
	if m.snap == nil {
		return nil
	}
	l := order.Line{Menu: m.menu, Sate: m.sate, Note: order.CleanText(m.note.Value())}
	friend := ""
	if m.mode == "friend" || m.mode == "edit" {
		friend = m.forName
	}
	body := map[string]any{"menu": m.menu, "sate": m.sate, "note": m.note.Value(), "for": friend}
	label := l.String()
	if friend != "" {
		label = strings.ToUpper(friend) + ": " + label
	}
	method, path, done := http.MethodPost, "/api/orders/"+m.snap.Order.ID+"/lines", "✓ masuk: "+label
	if m.mode == "edit" {
		method, path, done = http.MethodPut, fmt.Sprintf("%s/%d", path, m.editID), "✓ diganti: "+label
	}
	return func() tea.Msg {
		return savedMsg{text: done, err: m.c.call(method, path, body, nil)}
	}
}

func (m *model) promptKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	base := "/api/orders/" + m.orderID
	switch m.prompt {
	case confirmLock, confirmCancel, confirmLateOff, confirmDelete:
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
		case confirmDelete:
			return m, m.act("mangkok dihapus", http.MethodDelete, fmt.Sprintf("%s/lines/%d", base, m.editID), nil)
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
		if p == promptFriend {
			name, err := order.CleanFriend(v)
			if err == nil && name == "" {
				err = errors.New("isi nama teman dulu ya Kakak")
			}
			if err != nil {
				m.flash("", err)
				return m, nil
			}
			m.startForm("friend", store.Line{Menu: order.Daging, For: name})
			return m, nil
		}
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
	cInk   = lipgloss.Color("230")
	cMuted = lipgloss.Color("244")
	cRed   = lipgloss.Color("160")
	cGreen = lipgloss.Color("71")
	cAmber = lipgloss.Color("214")
	cBoard = lipgloss.Color("235")

	sInk   = lipgloss.NewStyle().Bold(true)
	sMuted = lipgloss.NewStyle().Foreground(cMuted)
	sRed   = lipgloss.NewStyle().Foreground(cRed).Bold(true)
	sGreen = lipgloss.NewStyle().Foreground(cGreen)
	sAmber = lipgloss.NewStyle().Foreground(cAmber)
	sKey   = lipgloss.NewStyle().Foreground(cInk).Background(lipgloss.Color("238")).Padding(0, 1)
	sSel   = lipgloss.NewStyle().Reverse(true)
	sFrame = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("180")).Padding(0, 2)
	sBoard = lipgloss.NewStyle().Background(cBoard).Foreground(lipgloss.Color("229")).Padding(0, 1)
)

// inner is the content width of the centered box.
func (m *model) inner() int { return max(40, min(66, m.width-6)) }

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

// row puts l on the left and r on the right of a w-wide line.
func row(l, r string, w int) string {
	gap := w - lipgloss.Width(l) - lipgloss.Width(r)
	if gap < 1 {
		gap = 1
	}
	return l + strings.Repeat(" ", gap) + r
}

// keys lays out key hints, wrapping them to the box.
func keys(w int, pairs ...string) string {
	var lines []string
	cur := ""
	for i := 0; i+1 < len(pairs); i += 2 {
		item := sKey.Render(pairs[i]) + " " + sMuted.Render(pairs[i+1])
		if cur != "" && lipgloss.Width(cur)+2+lipgloss.Width(item) > w {
			lines = append(lines, cur)
			cur = ""
		}
		if cur != "" {
			cur += "  "
		}
		cur += item
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n")
}

func (m *model) View() string {
	w := m.inner()
	var b strings.Builder
	who := m.me.Name
	if m.me.Admin {
		who += sAmber.Render(" admin")
	}
	live := m.liveL
	if m.view == viewOrder {
		live = m.liveO
	}
	b.WriteString(row(sInk.Render("🍲 TANGKAR"), dot(live)+"  "+sMuted.Render(who), w) + "\n")
	b.WriteString(sMuted.Render(strings.Repeat("═", w)) + "\n")
	if m.me.ActingAs {
		left := 30*time.Minute - time.Since(m.asSince)
		b.WriteString(lipgloss.NewStyle().Width(w).Background(lipgloss.Color("221")).Foreground(lipgloss.Color("16")).Bold(true).Padding(0, 1).
			Render(row("🎭 SEBAGAI "+strings.ToUpper(m.me.Name), fmt.Sprintf("%d mnt · I kembali ke %s", int(left.Minutes())+1, m.me.RealName), w-2)) + "\n")
	}
	b.WriteString("\n")
	switch m.view {
	case viewPeople:
		b.WriteString(sInk.Render("🎭 IMPERSONATE") + "\n" + sMuted.Render("30 menit, semua perubahan tercatat atas nama Kakak. UJI 1-3 hanya di pesanan uji.") + "\n\n")
		for i, p := range m.people {
			item := fmt.Sprintf(" %-16s %s ", p.Name, sMuted.Render(p.Email))
			if i == m.pickP {
				item = sSel.Render(fmt.Sprintf(" %-16s %s ", p.Name, p.Email))
			}
			b.WriteString(item + "\n")
		}
		b.WriteString("\n" + keys(w, "↑↓", "pilih", "enter", "masuk sebagai", "esc", "batal") + "\n")
	case viewCreate:
		m.viewCreate(&b, w)
	case viewOrder:
		m.viewOrder(&b, w)
	default:
		m.viewList(&b, w)
	}
	if m.status != "" {
		st := sGreen
		if m.bad {
			st = sRed
		}
		b.WriteString("\n" + lipgloss.NewStyle().Width(w).Inherit(st).Render(m.status))
	}
	box := sFrame.Width(w + 4).Render(strings.TrimRight(b.String(), "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

var phaseLabel = map[string]string{"open": "BUKA", "late": "LATE JOIN", "closed": "TUTUP", "cancelled": "BATAL"}

func (m *model) viewList(b *strings.Builder, w int) {
	now := m.now()
	b.WriteString(sMuted.Render("PESANAN HARI INI") + "\n")
	if len(m.list.Orders) == 0 {
		msg := "Belum ada pesanan hari ini, Kakak."
		if !m.me.Admin {
			msg += " Tunggu link dari admin ya."
		}
		b.WriteString("\n" + sMuted.Render(msg) + "\n")
	}
	for i, s := range m.list.Orders {
		o := s.Order
		p := phaseOf(o, now)
		when := o.DayLabel
		switch p {
		case "open":
			when = "tutup " + clock(o.LockAt) + " · sisa " + left(o.LockAt.Sub(now))
		case "late":
			when = "late join s.d. " + clock(o.LateUntil) + " · sisa " + left(o.LateUntil.Sub(now))
		}
		st := sMuted
		switch p {
		case "open":
			st = sGreen
		case "late":
			st = sAmber
		}
		totals := fmt.Sprintf("DAGING %d · CAMPUR %d · SATE %d", s.Totals.Daging, s.Totals.Campur, s.Totals.Sate)
		if s.Viewers > 0 {
			totals += fmt.Sprintf(" · %d buka", s.Viewers)
		}
		for _, x := range s.Sends {
			if x.Status == order.SendFailed {
				totals += sRed.Render(" · WA gagal")
			}
		}
		tw := w - 4
		head := sInk.Render(strings.ToUpper(o.Title))
		if o.Test {
			head += sAmber.Render(" UJI")
		}
		body := row(head, st.Bold(true).Render(phaseLabel[p]), tw) + "\n" +
			sMuted.Render(when) + "\n" + totals
		card := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1).Width(w - 2)
		if i == m.cursor {
			card = card.Border(lipgloss.ThickBorder()).BorderForeground(cRed)
		}
		b.WriteString(card.Render(body) + "\n")
	}
	b.WriteString("\n")
	pairs := []string{"↑↓", "pilih", "enter", "buka", "c", "salin link"}
	if m.me.Admin {
		pairs = append(pairs, "n", "baru", "I", "impersonate")
	}
	if m.me.ActingAs {
		pairs = append(pairs, "I", "kembali")
	}
	b.WriteString(keys(w, append(pairs, "q", "keluar")...) + "\n")
}

func (m *model) viewCreate(b *strings.Builder, w int) {
	b.WriteString(sInk.Render("BUKA PESANAN BARU") + "\n\n")
	for _, f := range m.form {
		b.WriteString(f.View() + "\n")
	}
	b.WriteString("\n" + keys(w, "tab", "pindah", "enter", "buka pesanan", "esc", "batal") + "\n")
}

func (m *model) board(w int, o store.Order, p string, now time.Time) string {
	var label, state, big, at string
	st := sBoard
	switch p {
	case "open":
		label, state, big, at = "TUTUP DALAM", "BUKA", left(o.LockAt.Sub(now)), "tutup "+clock(o.LockAt)
		if o.LockAt.Sub(now) < 5*time.Minute {
			st = st.Foreground(lipgloss.Color("210"))
		}
	case "late":
		label, state, big, at = "LATE JOIN SISA", "LATE JOIN", left(o.LateUntil.Sub(now)), "s.d. "+clock(o.LateUntil)
		st = st.Background(lipgloss.Color("94"))
	case "cancelled":
		label, state, big = "DIBATALKAN", "BATAL", clock(o.LockAt)
	default:
		label, state, big = "SUDAH TUTUP", "TUTUP", clock(o.LockAt)
	}
	iw := w - 2
	return st.Width(w).Render(row(label, state, iw) + "\n" + row(lipgloss.NewStyle().Bold(true).Render(big), at, iw))
}

func (m *model) sendBar(w int) string {
	var last *store.Send
	for i := range m.snap.Sends {
		if m.snap.Sends[i].Status != order.SendSkipped {
			last = &m.snap.Sends[i]
		}
	}
	if last == nil {
		return ""
	}
	bar := lipgloss.NewStyle().Width(w).Padding(0, 1).Bold(true)
	switch {
	case last.Status == order.SendPending:
		return bar.Background(lipgloss.Color("236")).Foreground(lipgloss.Color("229")).Render("🛵 LAGI DIKIRIM KE MAMANG! SEBENTAR YA KAKAK 🙏") + "\n"
	case last.Status == order.SendFailed:
		return bar.Foreground(cRed).Render("Mamang belum nerima, lagi dicoba lagi ya Kakak 🙏") + "\n"
	case m.now().Sub(time.Unix(last.SentAt, 0)) < 15*time.Minute:
		return bar.Foreground(cGreen).Render("✓ Sudah sampai ke Mamang, Kakak-kakak 😋 ("+clock(time.Unix(last.SentAt, 0))+")") + "\n"
	}
	return ""
}

func (m *model) menuKeys(w int) string {
	bw := (w - 2) / 2
	key := func(name, sub string, on bool) string {
		st := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Width(bw - 2).Align(lipgloss.Center)
		if on {
			st = st.Border(lipgloss.ThickBorder()).BorderForeground(cRed).Background(cRed).Foreground(cInk)
		}
		return st.Render(lipgloss.NewStyle().Bold(true).Render(name) + "\n" + sub)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		key("DAGING", "soto daging", m.menu == order.Daging), "  ",
		key("CAMPUR", "daging + jeroan", m.menu == order.Campur))
}

func (m *model) viewOrder(b *strings.Builder, w int) {
	if m.snap == nil {
		b.WriteString(sMuted.Render("memuat…") + "\n\n" + keys(w, "esc", "kembali") + "\n")
		return
	}
	s, o, now := m.snap, m.snap.Order, m.now()
	p := phaseOf(o, now)
	title := sInk.Render(strings.ToUpper(o.Title))
	if o.Test {
		title += "  " + lipgloss.NewStyle().Background(lipgloss.Color("221")).Foreground(lipgloss.Color("16")).Bold(true).Render(" UJI · WA ke grup tes ")
	}
	b.WriteString(title + "\n")
	b.WriteString(sMuted.Render(o.DayLabel) + "\n\n")
	b.WriteString(m.board(w, o, p, now) + "\n")
	if p == "late" {
		b.WriteString(lipgloss.NewStyle().Width(w).Foreground(cAmber).Render("Pesanan Kakak masuk TAMBAHAN, dikirim otomatis 2 menit setelah susulan terakhir (paling lama 4 menit).") + "\n")
	}
	b.WriteString(m.sendBar(w))
	b.WriteString("\n")

	if m.formActive(p) {
		switch m.mode {
		case "add":
			title := "TAMBAH MANGKOK LAGI"
			if p == "late" {
				title = "ORDER SUSULAN"
			}
			b.WriteString(row(sInk.Render(title), sMuted.Render("esc batal"), w) + "\n")
		case "friend":
			b.WriteString(row(sInk.Render("UNTUK: "+strings.ToUpper(m.forName)), sMuted.Render("esc batal"), w) + "\n")
		case "edit":
			for _, l := range m.mine() {
				if l.ID == m.editID {
					b.WriteString(row(sInk.Render("GANTI: "+friendPrefix(l)+l.Order().String()), sMuted.Render("esc batal"), w) + "\n")
				}
			}
		}
		b.WriteString(m.menuKeys(w) + "\n")
		note := m.note.View()
		if !m.noting && m.note.Value() == "" {
			note = sMuted.Render("n untuk isi")
		}
		b.WriteString(fmt.Sprintf("SATE  [-] %s [+]     CATATAN  %s", sInk.Render(fmt.Sprintf("%2d", m.sate)), note) + "\n\n")
	} else if mine := m.mine(); len(mine) > 0 {
		b.WriteString(m.card(w, p, mine) + "\n\n")
	}

	nota := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1).Width(w - 2)
	b.WriteString(nota.Render(m.nota(w-4)) + "\n")
	if len(s.Viewers) > 0 {
		b.WriteString(sMuted.Render("lagi buka: "+strings.Join(s.Viewers, ", ")) + "\n")
	}
	b.WriteString("\n")

	switch m.prompt {
	case confirmLock:
		b.WriteString(sRed.Render("Kunci sekarang dan kirim ke Mamang? (y/n)") + "\n")
		return
	case confirmCancel:
		b.WriteString(sRed.Render("Batalkan pesanan ini? (y/n)") + "\n")
		return
	case confirmLateOff:
		b.WriteString(sRed.Render("Tutup late join sekarang? (y/n)") + "\n")
		return
	case confirmDelete:
		b.WriteString(sRed.Render("Hapus mangkok ini? (y/n)") + "\n")
		return
	case promptLockAt:
		b.WriteString("Jam tutup baru (HH:MM)\n" + m.input.View() + "\n")
		return
	case promptLate:
		b.WriteString("Late join, menit setelah tutup (0 = tutup)\n" + m.input.View() + "\n")
		return
	case promptFriend:
		b.WriteString("Orderkan teman: nama temannya (maks 20 huruf)\n" + m.input.View() + "\n" + sMuted.Render("enter lanjut · esc batal") + "\n")
		return
	}
	var pairs []string
	switch {
	case m.formActive(p) && m.mode == "edit":
		pairs = []string{"d/a", "daging/campur", "+/-", "sate", "n", "catatan", "enter", "simpan", "esc", "batal"}
	case m.formActive(p) && m.mode == "add":
		pairs = []string{"d/a", "daging/campur", "+/-", "sate", "n", "catatan", "enter", "pesan lagi", "esc", "batal"}
	case m.formActive(p) && m.mode == "friend":
		pairs = []string{"d/a", "daging/campur", "+/-", "sate", "n", "catatan", "enter", "pesan untuk teman", "esc", "batal"}
	case m.formActive(p):
		pairs = []string{"d/a", "daging/campur", "+/-", "sate", "n", "catatan", "enter", "pesan", "f", "orderkan teman", "esc", "kembali"}
	default:
		if p == "open" || p == "late" {
			pairs = append(pairs, "o", "order lagi", "f", "orderkan teman")
		}
		if len(m.mine()) > 0 {
			pairs = append(pairs, "↑↓", "pilih", "g", "ganti", "x", "hapus")
		}
		pairs = append(pairs, "esc", "kembali")
	}
	pairs = append(pairs, "c", "salin link")
	b.WriteString(keys(w, pairs...) + "\n")
	if m.me.Admin {
		b.WriteString(keys(w, "L", "kunci", "T", "jam tutup", "J", "late join", "X", "tutup late", "C", "batal", "R", "kirim ulang") + "\n")
	}
}

func (m *model) nota(w int) string {
	var b strings.Builder
	s := m.snap
	b.WriteString(lipgloss.NewStyle().Width(w).Align(lipgloss.Center).Bold(true).Render("N O T A") + "\n")
	if len(s.Lines) == 0 {
		b.WriteString(sMuted.Render("belum ada pesanan"))
		return b.String()
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
	rule := sMuted.Render(strings.Repeat("-", w))
	line := func(l store.Line) {
		name := l.Name
		if l.For != "" {
			name = l.For + " (via " + l.Name + ")"
		}
		if l.By != "" {
			name += " 🎭" + l.By
		}
		b.WriteString(row(l.Order().String(), sMuted.Render(name), w) + "\n")
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
			st = sRed.Render("gagal, dicoba lagi")
		}
		if k == 0 && phaseOf(s.Order, m.now()) == "open" {
			st = sMuted.Render("dikirim " + clock(s.Order.LockAt))
		}
		b.WriteString(rule + "\n" + row(sInk.Render(title), st, w) + "\n")
		for _, l := range groups[k] {
			line(l)
		}
	}
	if len(waiting) > 0 {
		at := ""
		if s.CutAt > 0 {
			at = sAmber.Render("ke Mamang ±" + clock(time.UnixMilli(s.CutAt)))
		}
		b.WriteString(rule + "\n" + row(sInk.Render("SUSULAN, MENUNGGU"), at, w) + "\n")
		for _, l := range waiting {
			line(l)
		}
	}
	t := s.Totals
	b.WriteString(rule + "\n")
	b.WriteString(row(fmt.Sprintf("DAGING %d  CAMPUR %d", t.Daging, t.Campur), fmt.Sprintf("SATE %d", t.Sate), w) + "\n")
	b.WriteString(row(sInk.Render("TOTAL"), sInk.Render(fmt.Sprintf("%d porsi", t.Bowls)), w))
	return b.String()
}

// card is what someone sees after ordering: their bowls and where they are,
// instead of an empty form that looks like nothing happened.
func (m *model) card(w int, p string, mine []store.Line) string {
	sent := func(l store.Line) bool {
		if l.Batch == nil {
			return false
		}
		for _, x := range m.snap.Sends {
			if x.Batch == *l.Batch && x.Status == order.SendSent {
				return true
			}
		}
		return false
	}
	all := true
	waiting := false
	for _, l := range mine {
		all = all && sent(l)
		waiting = waiting || (l.Late && l.Batch == nil)
	}
	var b strings.Builder
	title, color := "✓ PESANAN KAKAK SUDAH MASUK 😋", cGreen
	if all {
		title, color = "✓ SUDAH SAMPAI KE MAMANG 😋", cInk
	}
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(color).Render(title) + "\n")
	for i, l := range mine {
		tag := ""
		if l.For != "" {
			tag += sMuted.Render(" teman")
		}
		if l.Late {
			tag += sAmber.Render(" susulan")
		}
		if sent(l) {
			tag += sGreen.Render(" terkirim")
		}
		item := " " + friendPrefix(l) + l.Order().String() + " "
		mark := "  "
		if i == m.pick {
			item, mark = sSel.Render(item), "▸ "
		}
		b.WriteString(mark + item + tag + "\n")
	}
	switch {
	case p == "open":
		b.WriteString(sMuted.Render("Dikirim ke Mamang jam " + clock(m.snap.Order.LockAt) + ", masih bisa diganti sampai jam itu."))
	case waiting && m.snap.CutAt > 0:
		b.WriteString(sAmber.Render("Susulan Kakak dikirim ke Mamang ±" + clock(time.UnixMilli(m.snap.CutAt))))
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color).Padding(0, 1).Width(w - 2).
		Render(strings.TrimRight(b.String(), "\n"))
}

func friendPrefix(l store.Line) string {
	if l.For == "" {
		return ""
	}
	return strings.ToUpper(l.For) + ": "
}
