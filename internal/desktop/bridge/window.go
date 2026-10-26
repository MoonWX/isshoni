package bridge

// Window is the bound Window service (03 §6.2): the app's own window calls. Page script never reaches the Wails
// runtime's Window object, which the Guard refuses (03 §5.6, W21).
type Window struct {
	shell WindowShell // nil: not_implemented
}

// SetFullscreen turns the window's fullscreen on or off, together with the element fullscreen inside the page
// (03 §12.1, W16).
func (s *Window) SetFullscreen(on bool) error {
	if s.shell == nil {
		return notImplemented("Window.SetFullscreen")
	}
	s.shell.SetFullscreen(on)
	return nil
}
