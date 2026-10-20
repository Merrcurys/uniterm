package platform

// RDPKeyEvent carries physical XT set-1 scan codes, including the E0 prefix.
// CaptureID prevents queued events from crossing focus changes or reconnects.
type RDPKeyEvent struct {
	CaptureID string `json:"captureID"`
	Scancode  uint16 `json:"scancode"`
	Down      bool   `json:"down"`
	Repeat    bool   `json:"repeat"`
	Ctrl      bool   `json:"ctrl"`
	Shift     bool   `json:"shift"`
	Meta      bool   `json:"meta"`
	Reset     bool   `json:"reset"`
}

type rdpKeyboardState struct {
	captureID string
	pressed   map[uint16]string
}

func (s *rdpKeyboardState) activate(id string) {
	s.captureID = id
	if s.pressed == nil {
		s.pressed = make(map[uint16]string)
	}
}

func (s *rdpKeyboardState) key(scan uint16, down, foreground, local bool) (RDPKeyEvent, bool) {
	// F12 remains available for the local inspector. Keys already swallowed
	// must also have their releases swallowed after focus leaves the desktop;
	// otherwise a Windows-key release can open the local Start menu.
	previous, held := s.pressed[scan]
	if !foreground || s.captureID == "" || local || scan == 0 {
		if held && !down {
			delete(s.pressed, scan)
		}
		return RDPKeyEvent{}, held
	}
	if held && previous != s.captureID {
		if !down {
			delete(s.pressed, scan)
		}
		return RDPKeyEvent{}, true
	}
	if !down && !held {
		return RDPKeyEvent{}, false
	}
	if down {
		s.pressed[scan] = s.captureID
	} else {
		delete(s.pressed, scan)
	}
	pressed := func(code uint16) bool { return s.pressed[code] == s.captureID }
	return RDPKeyEvent{
		CaptureID: s.captureID, Scancode: scan, Down: down, Repeat: held && down,
		Ctrl:  pressed(0x1d) || pressed(0xe01d),
		Shift: pressed(0x2a) || pressed(0x36),
		Meta:  pressed(0xe05b) || pressed(0xe05c),
	}, true
}
