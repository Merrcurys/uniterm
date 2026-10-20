package platform

import "testing"

func TestRDPKeyboardRemoteChords(t *testing.T) {
	for _, chord := range []struct {
		name  string
		scans []uint16
	}{
		{"AltTab", []uint16{0x38, 0x0f}},
		{"AltShiftTab", []uint16{0x38, 0x2a, 0x0f}},
		{"LeftWin", []uint16{0xe05b}},
		{"RightWin", []uint16{0xe05c}},
		{"WinE", []uint16{0xe05b, 0x12}},
		{"AltF4", []uint16{0x38, 0x3e}},
		{"CtrlShiftEscape", []uint16{0x1d, 0x2a, 0x01}},
	} {
		t.Run(chord.name, func(t *testing.T) {
			var state rdpKeyboardState
			state.activate("remote")
			for _, scan := range chord.scans {
				event, swallow := state.key(scan, true, true, false)
				if !swallow || event.CaptureID != "remote" || event.Scancode != scan || !event.Down || event.Repeat {
					t.Fatalf("key down must reach only remote: %+v, swallow=%v", event, swallow)
				}
			}
			for i := len(chord.scans) - 1; i >= 0; i-- {
				event, swallow := state.key(chord.scans[i], false, true, false)
				if !swallow || event.CaptureID != "remote" || event.Down {
					t.Fatalf("key up: %+v, %v", event, swallow)
				}
			}
			if len(state.pressed) != 0 {
				t.Fatal("stuck remote keys")
			}
		})
	}
}

func TestRDPKeyboardWinReleaseAfterBlur(t *testing.T) {
	var state rdpKeyboardState
	state.activate("remote")
	state.key(0xe05b, true, true, false)
	state.activate("")
	if event, swallow := state.key(0xe05b, false, false, false); !swallow || event.CaptureID != "" {
		t.Fatalf("Win release must not open local Start menu: %+v, %v", event, swallow)
	}
	if _, swallow := state.key(0xe05b, true, false, false); swallow {
		t.Fatal("new local Win press swallowed after blur")
	}
}

func TestRDPKeyboardPanelChangeDoesNotInheritHeldKeys(t *testing.T) {
	var state rdpKeyboardState
	state.activate("first")
	state.key(0x38, true, true, false)
	state.activate("second")
	for _, down := range []bool{true, false} {
		if event, swallow := state.key(0x38, down, true, false); !swallow || event.CaptureID != "" {
			t.Fatalf("old held key leaked into new panel: %+v, %v", event, swallow)
		}
	}
	if event, _ := state.key(0x38, true, true, false); event.CaptureID != "second" {
		t.Fatal("new key must reach new panel")
	}
}

func TestRDPKeyboardPasteModifiersAndRepeat(t *testing.T) {
	var state rdpKeyboardState
	state.activate("remote")
	state.key(0xe01d, true, true, false)
	for i := 0; i < 2; i++ {
		event, _ := state.key(0x2f, true, true, false)
		if !event.Ctrl || event.Repeat != (i == 1) {
			t.Fatalf("Ctrl+V state: %+v", event)
		}
	}
	state.key(0x2f, false, true, false)
	state.key(0xe01d, false, true, false)
	state.key(0x36, true, true, false)
	if event, _ := state.key(0xe052, true, true, false); !event.Shift || event.Ctrl {
		t.Fatalf("Shift+Insert state: %+v", event)
	}
}

func TestRDPKeyboardLocalKeysAndUnmatchedRelease(t *testing.T) {
	var state rdpKeyboardState
	state.activate("remote")
	for _, down := range []bool{true, false} {
		if _, swallow := state.key(0x58, down, true, true); swallow {
			t.Fatal("F12 must stay local")
		}
		if _, swallow := state.key(0x1e, down, false, false); swallow {
			t.Fatal("background typing must stay local")
		}
		if _, swallow := state.key(0, down, true, false); swallow {
			t.Fatal("unsupported media key swallowed")
		}
	}
	if _, swallow := state.key(0x1d, false, true, false); swallow {
		t.Fatal("modifier held before capture must release locally")
	}
}
