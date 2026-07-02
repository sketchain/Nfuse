package tui

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/sketchain/nfuse/internal/engine"
	"github.com/sketchain/nfuse/internal/model"
)

// fakeCtrl is an in-memory tui.Controller used to drive the real UI over a
// tcell SimulationScreen. Its state is mutated only from the UI's own event-loop
// goroutine (via key/mouse handlers), but a mutex guards it so the -race
// detector is satisfied across the initial-render handoff.
type fakeCtrl struct {
	mu       sync.Mutex
	accounts []engine.AccountView

	deleteCalled  bool
	deleteID      int64
	deleteCascade bool
}

func (f *fakeCtrl) View() ([]engine.AccountView, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]engine.AccountView(nil), f.accounts...), ""
}

func (f *fakeCtrl) AddAccount(string, model.Tier, float64, int) (int64, error) { return 0, nil }

func (f *fakeCtrl) DeleteAccount(id int64, cascade bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalled = true
	f.deleteID = id
	f.deleteCascade = cascade
	out := make([]engine.AccountView, 0, len(f.accounts))
	for _, a := range f.accounts {
		if a.Account.ID != id {
			out = append(out, a)
		}
	}
	f.accounts = out
	return nil
}

func (f *fakeCtrl) SetTier(int64, model.Tier, float64, int) error { return nil }
func (f *fakeCtrl) AddPort(int64, uint16) error                   { return nil }

func (f *fakeCtrl) DeletePort(portID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.accounts {
		kept := make([]engine.PortView, 0, len(f.accounts[i].Ports))
		for _, p := range f.accounts[i].Ports {
			if p.PortID != portID {
				kept = append(kept, p)
			}
		}
		f.accounts[i].Ports = kept
	}
	return nil
}

func (f *fakeCtrl) MovePort(int64, int64) error  { return nil }
func (f *fakeCtrl) ResetAccount(int64) error     { return nil }
func (f *fakeCtrl) SetUsage(int64, uint64) error { return nil }

// oneAccountTwoPorts builds a single monthly account (id 1) owning ports 8080
// (portID 10) and 9090 (portID 20).
func oneAccountTwoPorts() *fakeCtrl {
	return &fakeCtrl{accounts: []engine.AccountView{{
		Account: model.Account{ID: 1, Name: "alice", Tier: model.TierMonthly, LimitGiB: 1, BillingAnchorDay: 15},
		Ports: []engine.PortView{
			{PortID: 10, Port: 8080},
			{PortID: 20, Port: 9090},
		},
	}}}
}

// testUI starts the real UI over a simulation screen and returns handles plus a
// cleanup func. The refresh interval is set huge so the only renders are the
// ones the test triggers, keeping the sequence deterministic.
func testUI(t *testing.T, ctrl Controller) (*UI, tcell.SimulationScreen, func()) {
	t.Helper()
	// Collapse the double-click window so two clicks issued by a test are never
	// coalesced into a double-click.
	tview.DoubleClickInterval = 5 * time.Millisecond

	u := New(ctrl, time.Hour)
	screen := tcell.NewSimulationScreen("UTF-8")
	u.app.SetScreen(screen) // also initializes the screen
	screen.SetSize(120, 40)
	u.render() // initial build before the loop starts (no concurrency yet)

	done := make(chan error, 1)
	go func() { done <- u.app.Run() }()

	cleanup := func() {
		u.app.Stop()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("app.Run did not return after Stop")
		}
	}
	return u, screen, cleanup
}

// onLoop runs f on the application's event-loop goroutine and returns its
// result, serializing access to UI/primitive state.
func onLoop[T any](app *tview.Application, f func() T) T {
	ch := make(chan T, 1)
	app.QueueUpdate(func() { ch <- f() })
	return <-ch
}

// waitFor polls cond (on the event loop) until it holds or the deadline passes.
func waitFor(t *testing.T, app *tview.Application, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if onLoop(app, cond) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func pressRune(screen tcell.SimulationScreen, r rune) {
	screen.InjectKey(tcell.KeyRune, r, tcell.ModNone)
}

// clickAt injects a full press+release at (x,y) so tview synthesizes a click.
func clickAt(screen tcell.SimulationScreen, x, y int) {
	screen.InjectMouse(x, y, tcell.ButtonPrimary, tcell.ModNone)
	screen.InjectMouse(x, y, tcell.ButtonNone, tcell.ModNone)
}

// findText returns the center x and the y of the lowest (largest-y) occurrence
// of label on the screen. Buttons render below body text, so the lowest match
// of a word that also appears in the prompt is the button.
func findText(screen tcell.SimulationScreen, label string) (x, y int, ok bool) {
	cells, w, h := screen.GetContents()
	bestY := -1
	for row := 0; row < h; row++ {
		var b strings.Builder
		for col := 0; col < w; col++ {
			c := cells[row*w+col]
			if len(c.Runes) > 0 && c.Runes[0] != 0 {
				b.WriteRune(c.Runes[0])
			} else {
				b.WriteByte(' ')
			}
		}
		if idx := strings.Index(b.String(), label); idx >= 0 && row > bestY {
			bestY = row
			x = idx + len(label)/2
		}
	}
	if bestY < 0 {
		return 0, 0, false
	}
	return x, bestY, true
}

// selKind returns the kind+ids of the currently selected row's entity.
func curSel(u *UI) (rowRef, bool) {
	r, _ := u.table.GetSelection()
	ref, ok := u.rowRef[r]
	return ref, ok
}

// TestMouseModalSwallowsOutsideClicks covers task 2: while a modal is open, a
// click on the blank area outside it must not fall through to the table (which
// would silently move the selection), and the modal must stay open. A click on
// the modal's own button must still work.
func TestMouseModalSwallowsOutsideClicks(t *testing.T) {
	ctrl := oneAccountTwoPorts()
	u, screen, cleanup := testUI(t, ctrl)
	defer cleanup()

	// Initial selection is the first row: the account.
	waitFor(t, u.app, "initial selection on the account", func() bool {
		ref, ok := curSel(u)
		return ok && ref.kind == rowAccount && ref.accountID == 1
	})

	// Open the delete-account modal.
	pressRune(screen, 'd')
	waitFor(t, u.app, "modal open", func() bool { return u.pages.HasPage("modal") })

	// The prompt must mention the cascade (account owns 2 ports). The modal wraps
	// the text, so assert on the contiguous "port(s)?" tail.
	waitFor(t, u.app, "cascade prompt", func() bool {
		_, _, ok := findText(screen, "port(s)?")
		return ok
	})

	// Click far outside the centered modal, over what would be a table row
	// (screen row 4 ≈ the 9090 port row). It must be swallowed.
	clickAt(screen, 6, 4)
	// Let the injected events drain fully before asserting nothing changed.
	time.Sleep(120 * time.Millisecond)

	if onLoop(u.app, func() bool { return u.pages.HasPage("modal") }) == false {
		t.Fatal("modal closed by an outside click")
	}
	if ref, ok := onLoop(u.app, func() rowRef {
		r, _ := u.table.GetSelection()
		return u.rowRef[r]
	}), true; ok {
		if ref.kind != rowAccount || ref.accountID != 1 {
			t.Fatalf("outside click leaked to the table and moved selection to %+v", ref)
		}
	}

	// Now click the modal's "Delete" button; it must trigger the cascade delete.
	bx, by, found := findText(screen, "Delete")
	if !found {
		t.Fatal("Delete button not found on screen")
	}
	clickAt(screen, bx, by)

	waitFor(t, u.app, "account deleted and modal closed", func() bool {
		return !u.pages.HasPage("modal") && len(u.rowRef) == 0
	})

	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if !ctrl.deleteCalled || ctrl.deleteID != 1 || !ctrl.deleteCascade {
		t.Fatalf("button click delete = {called:%v id:%d cascade:%v}, want {true 1 true}",
			ctrl.deleteCalled, ctrl.deleteID, ctrl.deleteCascade)
	}
}

// TestSelectionSurvivesRerenders covers task 3: a port selection must keep
// pointing at the same port across repeated render() ticks, and fall back to the
// parent account when that port is deleted.
func TestSelectionSurvivesRerenders(t *testing.T) {
	ctrl := oneAccountTwoPorts()
	u, screen, cleanup := testUI(t, ctrl)
	defer cleanup()

	// Select the 9090 port row (screen row 4: border+header at 0/1, account at 2,
	// 8080 at 3, 9090 at 4).
	clickAt(screen, 6, 4)
	waitFor(t, u.app, "port 9090 selected", func() bool {
		ref, ok := curSel(u)
		return ok && ref.kind == rowPort && ref.portID == 20
	})

	// Several refresh ticks rebuild the table; the selection must still resolve to
	// the same port.
	for i := 0; i < 3; i++ {
		onLoop(u.app, func() bool { u.render(); return true })
	}
	if ref, ok := onLoop(u.app, func() rowRef {
		r, _ := u.table.GetSelection()
		return u.rowRef[r]
	}), true; !ok || ref.kind != rowPort || ref.portID != 20 {
		t.Fatalf("after re-renders selection = %+v, want port 20", ref)
	}

	// Delete the selected port out from under the selection, then re-render: the
	// selection must fall back to the parent account row.
	onLoop(u.app, func() bool {
		_ = ctrl.DeletePort(20)
		u.render()
		return true
	})
	ref := onLoop(u.app, func() rowRef {
		r, _ := u.table.GetSelection()
		return u.rowRef[r]
	})
	if ref.kind != rowAccount || ref.accountID != 1 {
		t.Fatalf("after deleting the selected port, selection = %+v, want account 1", ref)
	}
}
