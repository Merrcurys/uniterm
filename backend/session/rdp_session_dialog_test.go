//go:build windows

package session

import "testing"

// dialogDismissIDs decides which dialog control IDs to auto-click.
// Credential-style dialogs (any edit control present) must never be touched:
// auto-confirming them with a missing/wrong password would re-show the dialog
// forever. Pure warning/confirmation dialogs are dismissed by posting the
// standard control IDs, which are locale-independent Windows constants.
func TestDialogDismissIDs(t *testing.T) {
	if ids := dialogDismissIDs(true); ids != nil {
		t.Errorf("dialogDismissIDs(hasEdit=true) = %v, want nil (credential dialog must not be auto-confirmed)", ids)
	}
	want := []uintptr{IDYES, IDOK, IDNO}
	if ids := dialogDismissIDs(false); !equalIDs(ids, want) {
		t.Errorf("dialogDismissIDs(hasEdit=false) = %v, want %v", ids, want)
	}
}

func equalIDs(a, b []uintptr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ownerChain walks the GW_OWNER chain of a window. Security dialogs popped by
// the RDP ActiveX are identified by their owner chain leading back to the RDP
// child window — no localized title matching involved. The walk must terminate
// on a zero owner and survive cycles (defensive: a broken owner chain must not
// hang the dismiss poller).
func TestOwnerChain(t *testing.T) {
	// a -> b -> c -> 0
	owners := map[uintptr]uintptr{0xA: 0xB, 0xB: 0xC, 0xC: 0}
	chain := ownerChain(0xA, func(h uintptr) uintptr { return owners[h] })
	if len(chain) != 3 || chain[0] != 0xA || chain[1] != 0xB || chain[2] != 0xC {
		t.Errorf("ownerChain(a) = %v, want [a b c]", chain)
	}

	// Cycle: a -> b -> a
	cycled := map[uintptr]uintptr{0xA: 0xB, 0xB: 0xA}
	chain = ownerChain(0xA, func(h uintptr) uintptr { return cycled[h] })
	if len(chain) != 2 {
		t.Errorf("ownerChain with cycle = %v, want exactly the two distinct windows", chain)
	}
}

// dismissableDialog decides whether a #32770 dialog of our process belongs to
// the RDP connect flow. The mstscax warning dialogs are owned either by the
// RDP child window or directly by the main window (verified live: the
// "网站正在尝试启动远程连接" redirection warning is owned by the WailsWebviewWindow,
// not by the AtlAxWin child), so both owner chains are accepted. Dialogs owned
// by anything else are left alone.
func TestDismissableDialog(t *testing.T) {
	owners := map[uintptr]uintptr{
		0x20: 0x10, // dialog -> rdp child
		0x21: 0x11, // dialog -> main window
		0x22: 0x99, // dialog -> unrelated window
	}
	getOwner := func(h uintptr) uintptr { return owners[h] }
	if !dismissableDialog(getOwner, 0x20, 0x10, 0x11) {
		t.Error("dialog owned by the RDP child must be dismissable")
	}
	if !dismissableDialog(getOwner, 0x21, 0x10, 0x11) {
		t.Error("dialog owned directly by the main window must be dismissable")
	}
	if dismissableDialog(getOwner, 0x22, 0x10, 0x11) {
		t.Error("dialog owned by an unrelated window must not be dismissable")
	}
}

// This is the locale-independent replacement for the zh/en title list: the
// cert/redirection warnings shown by the ActiveX control are owned (directly
// or transitively) by the RDP child window, while unrelated dialogs of our
// process are not.
func TestOwnerChainContains(t *testing.T) {
	// rdpChild(0x10) owns dialog(0x20) directly.
	owners := map[uintptr]uintptr{0x20: 0x10}
	getOwner := func(h uintptr) uintptr { return owners[h] }
	if !ownerChainContains(getOwner, 0x20, 0x10) {
		t.Error("dialog owned directly by the RDP child must be detected")
	}
	// rdpChild(0x10) <- intermediate(0x30) <- dialog(0x20): transitive.
	owners[0x30] = 0x10
	owners[0x20] = 0x30
	if !ownerChainContains(getOwner, 0x20, 0x10) {
		t.Error("dialog owned transitively by the RDP child must be detected")
	}
	// Unrelated dialog owned by some other window (e.g. the main window only).
	if ownerChainContains(getOwner, 0x20, 0x99) {
		t.Error("unrelated owner chain must not match")
	}
}
