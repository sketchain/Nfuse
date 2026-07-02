// Package tui is the interactive terminal UI for managing Nfuse: it samples and
// displays per-port in/out detail plus per-account aggregate usage vs quota, and
// drives account/port/tier/reset operations through the engine.
package tui

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
	"github.com/sketchain/nfuse/internal/rpc"
)

// healthProvider is optionally implemented by a Controller (the RPC client) to
// expose daemon health metadata for the status bar. The in-process engine does
// not implement it, so the extra status line simply doesn't appear in that role.
type healthProvider interface {
	Health() (rpc.HealthResult, error)
}

// Controller is the set of operations the UI drives. It is satisfied both by the
// in-process engine (server role) and by the RPC client (client role), so the
// TUI is identical whether it talks to a local engine or a remote daemon. All
// mutations return only an error; the UI re-renders by re-reading View(), which
// on the client performs a fresh full GetState.
type Controller interface {
	View() ([]engine.AccountView, string)
	AddAccount(name string, tier model.Tier, limitGiB float64, anchorDay int) (int64, error)
	DeleteAccount(id int64, cascade bool) error
	SetTier(id int64, tier model.Tier, limitGiB float64, anchorDay int) error
	AddPort(accountID int64, port uint16) error
	DeletePort(portID int64) error
	MovePort(portID, newAccountID int64) error
	ResetAccount(id int64) error
	SetUsage(id int64, usedBytes uint64) error
}

// UI wires a tview application to a Controller.
type UI struct {
	app     *tview.Application
	pages   *tview.Pages
	table   *tview.Table
	status  *tview.TextView
	ctrl    Controller
	refresh time.Duration

	// rowRef maps a table row index to the object it represents so key actions
	// know their target.
	rowRef map[int]rowRef

	// The selection is remembered by *entity identity* rather than by raw row
	// index: render() rebuilds the table from scratch each tick, so rows shift as
	// accounts/ports are added, removed or reordered. After every rebuild the
	// selection is restored to the same object (see restoreSelection), so a
	// refresh tick — or a mutation elsewhere — never silently repoints it.
	haveSel      bool
	selKind      rowKind
	selAccountID int64
	selPortID    int64
}

type rowKind int

const (
	rowAccount rowKind = iota
	rowPort
)

type rowRef struct {
	kind      rowKind
	accountID int64
	account   model.Account
	portID    int64
	port      uint16
}

// New builds the UI over the given controller.
func New(ctrl Controller, refresh time.Duration) *UI {
	if refresh <= 0 {
		refresh = time.Second
	}
	u := &UI{
		app:     tview.NewApplication(),
		pages:   tview.NewPages(),
		table:   tview.NewTable(),
		status:  tview.NewTextView().SetDynamicColors(true),
		ctrl:    ctrl,
		refresh: refresh,
		rowRef:  map[int]rowRef{},
	}
	u.buildLayout()
	return u
}

func (u *UI) buildLayout() {
	u.table.SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)
	u.table.SetBorder(true).SetTitle(" Nfuse — accounts / ports ")

	u.status.SetBorder(true).SetTitle(" status ")
	help := tview.NewTextView().SetDynamicColors(true)
	help.SetText("[yellow]a[-] add acct  [yellow]d[-] del acct  [yellow]p[-] add port  [yellow]x[-] del port  [yellow]m[-] move port  [yellow]t[-] tier  [yellow]r[-] reset  [yellow]u[-] set usage  [yellow]q[-] quit")
	help.SetBorder(true).SetTitle(" keys ")

	main := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(u.table, 0, 1, true).
		AddItem(u.status, 4, 0, false).
		AddItem(help, 3, 0, false)

	u.pages.AddPage("main", main, true, true)
	u.app.SetRoot(u.pages, true).EnableMouse(true)
	u.table.SetInputCapture(u.onKey)
	// Track the selected entity by identity whenever the row selection changes
	// (via keyboard navigation or a click on the table), so it survives rebuilds.
	u.table.SetSelectionChangedFunc(u.onSelectionChanged)
}

// onSelectionChanged records the entity behind the newly selected row so the
// selection can be restored by identity after the next render() rebuild.
func (u *UI) onSelectionChanged(row, _ int) {
	if ref, ok := u.rowRef[row]; ok {
		u.haveSel = true
		u.selKind = ref.kind
		u.selAccountID = ref.accountID
		u.selPortID = ref.portID
	}
}

// Run starts the UI event loop and refresh ticker; it blocks until the user
// quits.
func (u *UI) Run() error {
	go func() {
		t := time.NewTicker(u.refresh)
		defer t.Stop()
		for range t.C {
			u.app.QueueUpdateDraw(u.render)
		}
	}()
	u.render()
	return u.app.Run()
}

// Stop stops the UI.
func (u *UI) Stop() { u.app.Stop() }

func (u *UI) render() {
	views, lastErr := u.ctrl.View()
	u.table.Clear()
	u.rowRef = map[int]rowRef{}

	headers := []string{"OBJECT", "TIER", "PORT", "IN", "OUT", "USED", "LIMIT", "STATUS"}
	for c, h := range headers {
		u.table.SetCell(0, c, tview.NewTableCell(h).
			SetTextColor(tcell.ColorYellow).
			SetSelectable(false).
			SetAttributes(tcell.AttrBold))
	}

	row := 1
	for _, av := range views {
		a := av.Account
		limit := "—"
		status := "unlimited"
		statusColor := tcell.ColorGreen
		if a.Tier.HasQuota() {
			limit = model.FormatBytes(a.LimitBytes())
			if av.UsedBytes >= a.LimitBytes() {
				status = "BREACHED"
				statusColor = tcell.ColorRed
			} else {
				status = "ok"
			}
		}
		u.setRow(row, []string{
			"▸ " + a.Name, a.Tier.Describe(), "",
			"", "", model.FormatBytes(av.UsedBytes), limit, status,
		}, tcell.ColorWhite)
		u.table.GetCell(row, 7).SetTextColor(statusColor)
		u.rowRef[row] = rowRef{kind: rowAccount, accountID: a.ID, account: a}
		row++

		for _, p := range av.Ports {
			u.setRow(row, []string{
				"   port", "", strconv.Itoa(int(p.Port)),
				model.FormatBytes(p.InBytes), model.FormatBytes(p.OutBytes),
				"", "", "",
			}, tcell.ColorGray)
			u.rowRef[row] = rowRef{kind: rowPort, accountID: a.ID, account: a, portID: p.PortID, port: p.Port}
			row++
		}
	}

	if row == 1 {
		u.table.SetCell(1, 0, tview.NewTableCell("(no accounts — press 'a' to add one)").SetSelectable(false))
	}
	u.restoreSelection()

	statusLine := fmt.Sprintf("[green]sampling[-]  %s", time.Now().Format("15:04:05"))
	if lastErr != "" {
		statusLine = "[red]" + lastErr + "[-]"
	}
	if line := u.healthLine(); line != "" {
		statusLine += "\n" + line
	}
	u.status.SetText(statusLine)
}

// healthLine renders a daemon-info line (iface, uptime, last persist) when the
// controller exposes health, or "" otherwise.
func (u *UI) healthLine() string {
	hp, ok := u.ctrl.(healthProvider)
	if !ok {
		return ""
	}
	h, err := hp.Health()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("[white]daemon[-] iface %s · up %s · last persist %s",
		h.Iface, formatUptime(h.UptimeSeconds), formatPersist(h.LastPersistUnix))
}

// formatUptime renders a seconds count as a compact h/m/s duration.
func formatUptime(seconds float64) string {
	d := (time.Duration(seconds) * time.Second).Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	return d.Truncate(time.Second).String()
}

// formatPersist renders a last-persist unix timestamp as wall-clock time, or
// "never" when nothing has been persisted yet.
func formatPersist(unix int64) string {
	if unix == 0 {
		return "never"
	}
	return time.Unix(unix, 0).Format("15:04:05")
}

func (u *UI) setRow(row int, cells []string, color tcell.Color) {
	for c, v := range cells {
		u.table.SetCell(row, c, tview.NewTableCell(v).SetTextColor(color))
	}
}

func (u *UI) selected() (rowRef, bool) {
	r, _ := u.table.GetSelection()
	ref, ok := u.rowRef[r]
	return ref, ok
}

// restoreSelection re-points the table selection at the remembered entity after
// render() has rebuilt rowRef. If that entity is gone (e.g. deleted), it falls
// back to the entity's parent account when a port vanished, else to the first
// selectable row. Called at the end of every render.
func (u *UI) restoreSelection() {
	if len(u.rowRef) == 0 {
		return // no selectable rows (no accounts yet)
	}
	if u.haveSel {
		switch u.selKind {
		case rowPort:
			if row, ok := u.rowForPort(u.selPortID); ok {
				u.table.Select(row, 0)
				return
			}
			// The port was removed; fall back to its parent account if it remains.
			if row, ok := u.rowForAccount(u.selAccountID); ok {
				u.table.Select(row, 0)
				return
			}
		case rowAccount:
			if row, ok := u.rowForAccount(u.selAccountID); ok {
				u.table.Select(row, 0)
				return
			}
		}
	}
	// No remembered selection, or the entity (and its parent) are gone: settle on
	// the first selectable row.
	u.table.Select(u.firstSelectableRow(), 0)
}

func (u *UI) rowForPort(id int64) (int, bool) {
	for row, ref := range u.rowRef {
		if ref.kind == rowPort && ref.portID == id {
			return row, true
		}
	}
	return 0, false
}

func (u *UI) rowForAccount(id int64) (int, bool) {
	for row, ref := range u.rowRef {
		if ref.kind == rowAccount && ref.accountID == id {
			return row, true
		}
	}
	return 0, false
}

// firstSelectableRow returns the lowest row index that maps to an entity (rowRef
// never holds the header row), defaulting to 1 when the map is unexpectedly bare.
func (u *UI) firstSelectableRow() int {
	best := -1
	for row := range u.rowRef {
		if best == -1 || row < best {
			best = row
		}
	}
	if best == -1 {
		return 1
	}
	return best
}

func (u *UI) onKey(ev *tcell.EventKey) *tcell.EventKey {
	switch ev.Rune() {
	case 'q':
		u.app.Stop()
		return nil
	case 'a':
		u.formAddAccount()
		return nil
	case 'd':
		u.doDeleteAccount()
		return nil
	case 'p':
		u.formAddPort()
		return nil
	case 'x':
		u.doDeletePort()
		return nil
	case 'm':
		u.formMovePort()
		return nil
	case 't':
		u.formChangeTier()
		return nil
	case 'r':
		u.doReset()
		return nil
	case 'u':
		u.formSetUsage()
		return nil
	}
	if ev.Key() == tcell.KeyCtrlC {
		u.app.Stop()
		return nil
	}
	return ev
}

// ── Modals & forms ───────────────────────────────────────────────────────────

func (u *UI) flash(msg string) {
	u.modal(msg, []string{"OK"}, func(int, string) { u.closeModal() })
}

func (u *UI) errf(format string, args ...any) {
	u.flash("[red]" + fmt.Sprintf(format, args...))
}

// modalHost wraps a full-screen page primitive (a tview.Modal or a centered
// form Flex) so that any mouse event the wrapped primitive does not itself
// consume is swallowed here instead of falling through to the table on the page
// beneath. tview.Modal and the centering Flex only consume clicks that land on
// their own widgets (buttons, inputs); a click on the surrounding blank area —
// or a stray MouseLeftUp/Click that the Modal leaves unconsumed — would
// otherwise reach the table and silently move its selection, or dismiss nothing
// while looking like "the click did nothing". Consuming everything the inner
// primitive declines makes the overlay truly modal to the mouse while leaving
// its own buttons/inputs fully clickable.
type modalHost struct {
	tview.Primitive
}

func (h modalHost) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(tview.Primitive)) (bool, tview.Primitive) {
	inner := h.Primitive.MouseHandler()
	return func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(tview.Primitive)) (bool, tview.Primitive) {
		if inner != nil {
			if consumed, capture := inner(action, event, setFocus); consumed {
				return consumed, capture
			}
		}
		// Backstop: consume every remaining mouse event so nothing reaches the
		// primitives on the page(s) beneath this overlay.
		return true, nil
	}
}

func (u *UI) modal(text string, buttons []string, done func(int, string)) {
	m := tview.NewModal().SetText(text).AddButtons(buttons).SetDoneFunc(done)
	u.pages.AddPage("modal", modalHost{m}, true, true)
	u.app.SetFocus(m)
}

func (u *UI) closeModal() {
	u.pages.RemovePage("modal")
	u.app.SetFocus(u.table)
}

func (u *UI) closeForm() {
	u.pages.RemovePage("form")
	u.app.SetFocus(u.table)
}

func (u *UI) showForm(form *tview.Form, title string, height int) {
	form.SetBorder(true).SetTitle(" " + title + " ")
	// Center the form using a Flex sandwich.
	wrap := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(form, 60, 0, true).
			AddItem(nil, 0, 1, false), height, 0, true).
		AddItem(nil, 0, 1, false)
	u.pages.AddPage("form", modalHost{wrap}, true, true)
	u.app.SetFocus(form)
}

var tierOptions = []string{"a — monthly (reset)", "b — one-shot (never reset)", "c — unlimited"}

func tierFromOption(idx int) model.Tier {
	switch idx {
	case 0:
		return model.TierMonthly
	case 1:
		return model.TierOneShot
	default:
		return model.TierUnlimited
	}
}

func (u *UI) formAddAccount() {
	form := tview.NewForm()
	form.AddInputField("Name", "", 30, nil, nil)
	form.AddDropDown("Tier", tierOptions, 0, nil)
	form.AddInputField("Limit (GiB)", "10", 12, tview.InputFieldFloat, nil)
	form.AddInputField("Billing day (1-28)", "1", 6, tview.InputFieldInteger, nil)
	form.AddButton("Create", func() {
		name := form.GetFormItemByLabel("Name").(*tview.InputField).GetText()
		tierIdx, _ := form.GetFormItemByLabel("Tier").(*tview.DropDown).GetCurrentOption()
		tier := tierFromOption(tierIdx)
		limit, _ := strconv.ParseFloat(form.GetFormItemByLabel("Limit (GiB)").(*tview.InputField).GetText(), 64)
		anchor, _ := strconv.Atoi(form.GetFormItemByLabel("Billing day (1-28)").(*tview.InputField).GetText())
		u.closeForm()
		if _, err := u.ctrl.AddAccount(name, tier, limit, anchor); err != nil {
			u.errf("add account: %v", err)
			return
		}
		u.render()
	})
	form.AddButton("Cancel", u.closeForm)
	u.showForm(form, "Add account", 13)
}

func (u *UI) doDeleteAccount() {
	ref, ok := u.selected()
	if !ok || ref.kind != rowAccount {
		u.errf("select an account row first")
		return
	}
	// Count the account's ports so the prompt is honest and the delete cascades
	// only when there is actually something to cascade to.
	var portCount int
	views, _ := u.ctrl.View()
	for _, av := range views {
		if av.Account.ID == ref.accountID {
			portCount = len(av.Ports)
			break
		}
	}
	cascade := portCount > 0
	prompt := fmt.Sprintf("Delete account %q?", ref.account.Name)
	if cascade {
		prompt = fmt.Sprintf("Delete account %q and its %d port(s)?", ref.account.Name, portCount)
	}
	u.modal(prompt, []string{"Delete", "Cancel"}, func(i int, _ string) {
		u.closeModal()
		if i != 0 {
			return
		}
		if err := u.ctrl.DeleteAccount(ref.accountID, cascade); err != nil {
			u.errf("delete account: %v", err)
			return
		}
		u.render()
	})
}

func (u *UI) formAddPort() {
	ref, ok := u.selected()
	if !ok {
		u.errf("select an account (or one of its ports) first")
		return
	}
	acctID := ref.accountID
	acctName := ref.account.Name
	form := tview.NewForm()
	form.AddTextView("Account", acctName, 30, 1, true, false)
	form.AddInputField("Port", "", 8, tview.InputFieldInteger, nil)
	form.AddButton("Add", func() {
		portStr := form.GetFormItemByLabel("Port").(*tview.InputField).GetText()
		p, err := strconv.Atoi(portStr)
		u.closeForm()
		if err != nil || p < 1 || p > 65535 {
			u.errf("invalid port %q", portStr)
			return
		}
		if err := u.ctrl.AddPort(acctID, uint16(p)); err != nil {
			u.errf("add port: %v", err)
			return
		}
		u.render()
	})
	form.AddButton("Cancel", u.closeForm)
	u.showForm(form, "Add port", 9)
}

func (u *UI) doDeletePort() {
	ref, ok := u.selected()
	if !ok || ref.kind != rowPort {
		u.errf("select a port row first")
		return
	}
	u.modal(fmt.Sprintf("Delete port %d?", ref.port), []string{"Delete", "Cancel"}, func(i int, _ string) {
		u.closeModal()
		if i != 0 {
			return
		}
		if err := u.ctrl.DeletePort(ref.portID); err != nil {
			u.errf("delete port: %v", err)
			return
		}
		u.render()
	})
}

func (u *UI) formMovePort() {
	ref, ok := u.selected()
	if !ok || ref.kind != rowPort {
		u.errf("select a port row first")
		return
	}
	portID := ref.portID
	port := ref.port

	// Offer every account as a destination.
	views, _ := u.ctrl.View()
	var labels []string
	var ids []int64
	cur := 0
	for _, av := range views {
		if av.Account.ID == ref.accountID {
			cur = len(ids)
		}
		labels = append(labels, fmt.Sprintf("%s (%s)", av.Account.Name, av.Account.Tier.Describe()))
		ids = append(ids, av.Account.ID)
	}
	if len(ids) < 2 {
		u.errf("need at least two accounts to move a port")
		return
	}
	form := tview.NewForm()
	form.AddTextView("Port", strconv.Itoa(int(port)), 30, 1, true, false)
	form.AddDropDown("Destination", labels, cur, nil)
	form.AddButton("Move", func() {
		idx, _ := form.GetFormItemByLabel("Destination").(*tview.DropDown).GetCurrentOption()
		u.closeForm()
		if idx < 0 || idx >= len(ids) {
			u.errf("invalid destination")
			return
		}
		if err := u.ctrl.MovePort(portID, ids[idx]); err != nil {
			u.errf("move port: %v", err)
			return
		}
		u.render()
	})
	form.AddButton("Cancel", u.closeForm)
	u.showForm(form, "Move port", 9)
}

func (u *UI) formSetUsage() {
	ref, ok := u.selected()
	if !ok || ref.kind != rowAccount {
		u.errf("select an account row first")
		return
	}
	acct := ref.account
	if !acct.Tier.HasQuota() {
		u.errf("account %q is unlimited; no usage to set", acct.Name)
		return
	}
	form := tview.NewForm()
	form.AddInputField("Used (GiB)", "0", 12, tview.InputFieldFloat, nil)
	form.AddButton("Apply", func() {
		gib, err := strconv.ParseFloat(form.GetFormItemByLabel("Used (GiB)").(*tview.InputField).GetText(), 64)
		u.closeForm()
		if err != nil || gib < 0 {
			u.errf("invalid usage value")
			return
		}
		bytes := uint64(gib * float64(1<<30))
		if err := u.ctrl.SetUsage(acct.ID, bytes); err != nil {
			u.errf("set usage: %v", err)
			return
		}
		u.render()
	})
	form.AddButton("Cancel", u.closeForm)
	u.showForm(form, "Set usage: "+acct.Name, 9)
}

func (u *UI) formChangeTier() {
	ref, ok := u.selected()
	if !ok {
		u.errf("select an account first")
		return
	}
	acct := ref.account
	cur := 2
	switch acct.Tier {
	case model.TierMonthly:
		cur = 0
	case model.TierOneShot:
		cur = 1
	}
	form := tview.NewForm()
	form.AddDropDown("Tier", tierOptions, cur, nil)
	form.AddInputField("Limit (GiB)", strconv.FormatFloat(acct.LimitGiB, 'f', -1, 64), 12, tview.InputFieldFloat, nil)
	form.AddInputField("Billing day (1-28)", strconv.Itoa(acct.BillingAnchorDay), 6, tview.InputFieldInteger, nil)
	form.AddButton("Apply", func() {
		tierIdx, _ := form.GetFormItemByLabel("Tier").(*tview.DropDown).GetCurrentOption()
		tier := tierFromOption(tierIdx)
		limit, _ := strconv.ParseFloat(form.GetFormItemByLabel("Limit (GiB)").(*tview.InputField).GetText(), 64)
		anchor, _ := strconv.Atoi(form.GetFormItemByLabel("Billing day (1-28)").(*tview.InputField).GetText())
		u.closeForm()
		if err := u.ctrl.SetTier(acct.ID, tier, limit, anchor); err != nil {
			u.errf("change tier: %v", err)
			return
		}
		u.render()
	})
	form.AddButton("Cancel", u.closeForm)
	u.showForm(form, "Change tier: "+acct.Name, 11)
}

func (u *UI) doReset() {
	ref, ok := u.selected()
	if !ok {
		u.errf("select an account first")
		return
	}
	u.modal(fmt.Sprintf("Reset quota usage for %q to 0?", ref.account.Name), []string{"Reset", "Cancel"}, func(i int, _ string) {
		u.closeModal()
		if i != 0 {
			return
		}
		if err := u.ctrl.ResetAccount(ref.accountID); err != nil {
			u.errf("reset: %v", err)
			return
		}
		u.render()
	})
}
