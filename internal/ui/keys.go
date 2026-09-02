package ui

import "github.com/charmbracelet/bubbles/key"

type keymap struct {
	Quit      key.Binding
	Help      key.Binding
	Tab       key.Binding
	ShiftTab  key.Binding
	Pause     key.Binding
	Jump      key.Binding
	Zoom      key.Binding
	ColsMore  key.Binding
	ColsFewer key.Binding
	ColsAuto  key.Binding
	Theme     key.Binding
}

func defaultKeymap() keymap {
	return keymap{
		Quit:      key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Help:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Tab:       key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next pane")),
		ShiftTab:  key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("⇧tab", "previous pane")),
		Pause:     key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "freeze the display")),
		Jump:      key.NewBinding(key.WithKeys("1", "2", "3", "4", "5", "6", "7", "8", "9"), key.WithHelp("1-9", "jump to pane")),
		Zoom:      key.NewBinding(key.WithKeys("z"), key.WithHelp("z", "zoom focused pane")),
		ColsMore:  key.NewBinding(key.WithKeys("]"), key.WithHelp("]", "more columns")),
		ColsFewer: key.NewBinding(key.WithKeys("["), key.WithHelp("[", "fewer columns")),
		ColsAuto:  key.NewBinding(key.WithKeys("\\"), key.WithHelp("\\", "automatic columns")),
		// Upper case, so it cannot be hit while reaching for a pane jump or a
		// freeze: a theme switch redraws the whole screen, and doing that by
		// accident mid-incident is disorienting.
		Theme: key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "cycle theme")),
	}
}

// helpLines returns the rows shown in the help overlay.
func (k keymap) helpLines() []string {
	return []string{
		"q quit",
		"tab/⇧tab cycle focus",
		"1-9 jump to pane",
		"z zoom",
		"[ / ] columns",
		"\\ auto",
		"p freeze",
		"T theme",
	}
}
